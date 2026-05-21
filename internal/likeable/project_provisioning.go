package likeable

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/fibegg/likeable/internal/fibe"
	projecttext "github.com/fibegg/likeable/internal/project"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

const projectResourceRefreshInterval = 60 * time.Second
const projectProvisioningRecoveryGrace = 2 * time.Minute
const projectProvisioningRecoveryEnqueueTTL = projectProvisioningRecoveryGrace

func (s *Server) ensureDefaultProject(ctx context.Context, user *User) {
	if user == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(user.AccessStatus), "restricted") {
		return
	}
	projects, err := s.store.ProjectsForUser(ctx, user.ID)
	if err != nil {
		log.Printf("load projects for starter: %v", err)
		return
	}
	if len(projects) > 0 || s.projectCapForUser(ctx, user) <= 0 {
		return
	}
	project, err := s.createProjectRecord(ctx, user, "New playground")
	if err != nil {
		log.Printf("create starter project: %v", err)
		return
	}
	if err := s.provisionProjectAsync(user.ID, user.Email, project.ID, ""); err != nil {
		log.Printf("schedule starter project provisioning: %v", err)
		_ = s.store.DeleteProject(ctx, project.ID, user.ID)
	}
}

func (s *Server) createProjectRecord(ctx context.Context, user *User, title string) (*Project, error) {
	projectID := uuid.NewString()
	cfg, err := s.store.ConfigMap(ctx)
	if err != nil {
		return nil, err
	}
	assignment, err := fibe.AssignmentForNewProject(cfg, projectID)
	if err != nil {
		return nil, err
	}
	project := &Project{
		ID:             projectID,
		UserID:         user.ID,
		Title:          title,
		ConversationID: "likeable-" + strings.ReplaceAll(projectID, "-", ""),
		AgentID:        assignment.AgentID,
		MarqueeID:      assignment.MarqueeID,
		PlaygroundName: projecttext.SourceNameForProject(&Project{ID: projectID, Title: title}),
		Status:         "creating",
	}
	if err := s.store.CreateProject(ctx, project); err != nil {
		return nil, err
	}
	return project, nil
}

func (s *Server) provisionProjectAsync(userID, userEmail, projectID, prompt string) error {
	if s.jobs != nil {
		if err := s.enqueueProjectJob(context.Background(), taskProvisionProject, projectJobPayload{UserID: userID, UserEmail: userEmail, ProjectID: projectID, Prompt: prompt}, asynq.Queue(projectProvisionQueue), asynq.MaxRetry(6), asynq.Timeout(15*time.Minute), asynq.Unique(projectProvisionUniqueTTL)); err != nil {
			log.Printf("enqueue project provisioning %s: %v", projectID, err)
			return err
		}
		return nil
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()
		project, err := s.store.ProjectForUser(ctx, userID, projectID)
		if err != nil {
			log.Printf("load project for provisioning: %v", err)
			return
		}
		if err := s.provisionProject(ctx, userID, userEmail, project, prompt); err != nil {
			log.Printf("provision project %s: %v", project.ID, err)
			s.recordProjectProvisionFailure(ctx, userID, project, err, true)
		}
	}()
	return nil
}

func (s *Server) provisionProject(ctx context.Context, userID, userEmail string, project *Project, prompt string) error {
	acquired, err := s.store.TryAcquireProjectProvisioning(ctx, project.ID, userID, 15*time.Minute)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer func() {
		if err := s.store.ClearProjectProvisioningLease(context.Background(), project.ID, userID); err != nil {
			log.Printf("clear project provisioning lease %s: %v", project.ID, err)
		}
	}()
	if current, err := s.store.ProjectForUser(ctx, userID, project.ID); err == nil {
		project = current
	} else if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if project.Status == "ready" && strings.TrimSpace(project.PlaygroundID) != "" {
		return nil
	}
	ensureProjectPlaygroundName(project)
	fibeClient, err := s.fibeClientForProject(ctx, project, userEmail)
	if err != nil {
		return err
	}
	if strings.TrimSpace(project.PlaygroundID) == "" {
		result, err := fibeClient.CreateGreenfield(ctx, project)
		if err != nil {
			return err
		}
		project.PlaygroundID = result.PlaygroundID
		project.PlaygroundName = firstNonEmpty(result.PlaygroundName, project.PlaygroundName, projecttext.SourceNameForProject(project))
		project.RepoURL = result.RepoURL
		project.PreviewURL = result.PreviewURL
		project.PlayspecID = result.PlayspecID
		project.PropID = result.PropID
		project.SelectedService = result.SelectedServiceName
		project.Repositories = projectRepositoriesFromGreenfield(project.ID, result)
		project.Services = projectServicesFromGreenfield(project.ID, result)
		project.Status = "launching"
		if err := s.store.SaveProjectProvisioningSnapshot(ctx, project, project.Status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				_ = fibeClient.DeleteProjectResources(ctx, project)
			}
			return err
		}
	} else if project.Status != "launching" {
		project.Status = "launching"
		if err := s.store.UpdateProjectStatus(ctx, project.ID, userID, project.Status); err != nil {
			return err
		}
	}
	if err := s.startProjectAgentChat(ctx, project, fibeClient, "project provisioning"); err != nil {
		log.Printf("start workspace agent chat during project provisioning %s: %v", project.ID, err)
	}
	if err := fibeClient.WaitPlaygroundReady(ctx, project.PlaygroundID); err != nil {
		return err
	}
	if project.PreviewURL == "" || len(project.Repositories) == 0 || len(project.Services) == 0 {
		if recovered, err := fibeClient.GreenfieldByPlaygroundID(ctx, project.PlaygroundID); err == nil {
			mergeProjectGreenfieldResult(project, recovered)
			if err := s.store.SaveProjectProvisioningSnapshot(ctx, project, "launching"); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					_ = fibeClient.DeleteProjectResources(ctx, project)
				}
				return err
			}
		}
	}
	if err := fibeClient.WaitPreviewReachable(ctx, project.PreviewURL); err != nil {
		return err
	}
	project.Status = "ready"
	if err := s.store.SaveProjectProvisioningSnapshot(ctx, project, project.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_ = fibeClient.DeleteProjectResources(ctx, project)
		}
		return err
	}
	if strings.TrimSpace(prompt) != "" {
		if err := fibeClient.EnsureConversation(ctx, project.ConversationID, project.Title); err != nil {
			log.Printf("create initial conversation for project %s: %v", project.ID, err)
			return nil
		}
		if err := fibeClient.SendMessage(ctx, project.ConversationID, projecttext.AgentPrompt(project, prompt), nil, "queue"); err != nil {
			log.Printf("send initial prompt for project %s: %v", project.ID, err)
		} else {
			s.enqueueProjectNotificationMonitor(context.Background(), userID, userEmail, project.ID, 0)
		}
	}
	return nil
}

func (s *Server) recordProjectProvisionFailure(ctx context.Context, userID string, project *Project, err error, retriesRemaining bool) {
	if retriesRemaining && retryProjectProvisionLater(project, err) {
		status := "creating"
		if projectHasProvisionedResources(project) {
			status = "launching"
		}
		_ = s.store.UpdateProjectStatus(ctx, project.ID, userID, status)
		return
	}
	_ = s.store.UpdateProjectErrorFromError(ctx, project.ID, userID, err)
}

func retryProjectProvisionLater(project *Project, err error) bool {
	if previewEmbeddingBlocked(err) {
		return false
	}
	if projectHasProvisionedResources(project) {
		return true
	}
	return fibe.IsRetryableProvisioningError(err)
}

func (s *Server) recoverProjectsAsync(userID, userEmail string, projects []Project) {
	for i := range projects {
		s.recoverProjectAsync(userID, userEmail, &projects[i])
	}
}

func (s *Server) recoverProjectAsync(userID, userEmail string, project *Project) {
	if projectNeedsProvisioningRecovery(project) {
		key := "provisioning:" + userID + ":" + project.ID
		if !s.reserveProjectRecovery(key, projectProvisioningRecoveryEnqueueTTL) {
			return
		}
		log.Printf("recover project %s by retrying provisioning", project.ID)
		if err := s.provisionProjectAsync(userID, userEmail, project.ID, ""); err != nil {
			s.recovering.Delete(key)
			log.Printf("schedule project provisioning recovery %s: %v", project.ID, err)
		}
		return
	}
	if !projectNeedsReadinessRecovery(project) {
		return
	}
	key := "readiness:" + userID + ":" + project.ID
	if !s.reserveProjectRecovery(key, 0) {
		return
	}
	if s.jobs != nil {
		if err := s.enqueueProjectJob(context.Background(), taskRecoverProject, projectJobPayload{UserID: userID, UserEmail: userEmail, ProjectID: project.ID}, asynq.Queue("default"), asynq.MaxRetry(3), asynq.Timeout(90*time.Second), asynq.Unique(45*time.Second)); err != nil {
			log.Printf("enqueue project recovery %s: %v", project.ID, err)
		}
		s.recovering.Delete(key)
		return
	}
	go func() {
		defer s.recovering.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		current, err := s.store.ProjectForUser(ctx, userID, project.ID)
		if err != nil || !projectNeedsReadinessRecovery(current) {
			return
		}
		if blocked, err := s.projectIsExportOnly(ctx, &User{ID: userID, Email: userEmail}, current); err != nil || blocked {
			return
		}
		fibe, err := s.fibeClientForProject(ctx, current, userEmail)
		if err != nil {
			return
		}
		if err := s.recoverProjectReadiness(ctx, userID, current, fibe); err != nil {
			return
		}
	}()
}

func (s *Server) reserveProjectRecovery(key string, ttl time.Duration) bool {
	if _, loaded := s.recovering.LoadOrStore(key, true); loaded {
		return false
	}
	if ttl > 0 {
		time.AfterFunc(ttl, func() {
			s.recovering.Delete(key)
		})
	}
	return true
}

func (s *Server) recoverProjectReadiness(ctx context.Context, userID string, project *Project, fibe *fibe.Client) error {
	if _, ready, _, maintenance, err := s.promoteProjectFromReachablePreview(ctx, userID, project); ready || maintenance || previewEmbeddingBlocked(err) {
		return err
	}
	ready, status, err := fibe.PlaygroundReady(ctx, project.PlaygroundID)
	if err != nil {
		return err
	}
	if !ready {
		switch projectStatusFromFibePlayground(status) {
		case "stopped":
			project.Status = "stopped"
			return s.store.UpdateProjectStatus(ctx, project.ID, userID, project.Status)
		case "error":
			if err := s.store.UpdateProjectError(ctx, project.ID, userID, "The linked Fibe playground is in an error state."); err != nil {
				return err
			}
			return nil
		}
		if strings.TrimSpace(project.PreviewURL) != "" {
			_ = s.store.UpdateProjectStatus(ctx, project.ID, userID, "launching")
			return fmt.Errorf("workspace is still converging: %s", status)
		}
		return fmt.Errorf("workspace is still starting: %s", status)
	}
	if strings.TrimSpace(project.PreviewURL) == "" {
		if recovered, err := fibe.GreenfieldByPlaygroundID(ctx, project.PlaygroundID); err == nil {
			mergeProjectGreenfieldResult(project, recovered)
			if err := s.store.SaveProjectProvisioningSnapshot(ctx, project, "launching"); err != nil {
				return err
			}
		}
	}
	if _, previewReady, previewStatus, maintenance, err := s.promoteProjectFromReachablePreview(ctx, userID, project); err != nil {
		return err
	} else if !previewReady && !maintenance {
		_ = s.store.UpdateProjectStatus(ctx, project.ID, userID, "launching")
		return fmt.Errorf("preview is still starting: %s", previewStatus)
	}
	return nil
}

func (s *Server) refreshProjectReadiness(ctx context.Context, user *User, project *Project) (*Project, error) {
	if user == nil || !projectNeedsReadinessRecovery(project) {
		return project, nil
	}
	if blocked, err := s.projectIsExportOnly(ctx, user, project); err != nil || blocked {
		return project, err
	}
	fibe, err := s.fibeClientForProject(ctx, project, user.Email)
	if err != nil {
		return project, err
	}
	if err := s.recoverProjectReadiness(ctx, user.ID, project, fibe); err != nil {
		return project, err
	}
	updated, err := s.store.ProjectForUser(ctx, user.ID, project.ID)
	if err != nil {
		return project, err
	}
	return updated, nil
}

func (s *Server) refreshProjectResourcesIfDue(ctx context.Context, user *User, project *Project) (*Project, error) {
	return s.refreshProjectResources(ctx, user, project, false)
}

func (s *Server) refreshProjectResourcesNow(ctx context.Context, user *User, project *Project) (*Project, error) {
	return s.refreshProjectResources(ctx, user, project, true)
}

func (s *Server) refreshProjectResources(ctx context.Context, user *User, project *Project, force bool) (*Project, error) {
	if user == nil || project == nil || strings.TrimSpace(project.PlaygroundID) == "" || project.Status == "deleting" {
		return project, nil
	}
	if blocked, err := s.projectIsExportOnly(ctx, user, project); err != nil || blocked {
		return project, err
	}
	if err := s.applyCurrentProjectAssignment(ctx, user, project); err != nil {
		return project, err
	}
	if !force {
		if _, ok := s.platformBackoffRemaining(); ok {
			return project, nil
		}
		key := user.ID + ":" + project.ID + ":resources"
		if last, ok := s.refreshing.Load(key); ok {
			if lastRefresh, ok := last.(time.Time); ok && time.Since(lastRefresh) < projectResourceRefreshInterval {
				return project, nil
			}
		}
		s.refreshing.Store(key, time.Now())
	}
	client, err := s.fibeClientForProject(ctx, project, user.Email)
	if err != nil {
		return project, err
	}
	result, err := client.GreenfieldByPlaygroundID(ctx, project.PlaygroundID)
	if err != nil {
		s.observePlatformError(err)
		return project, err
	}
	s.clearPlatformBackoff()
	hasSnapshot := greenfieldHasResourceSnapshot(result)
	if hasSnapshot {
		applyProjectGreenfieldSnapshot(project, result)
	}
	oldStatus := project.Status
	status := project.Status
	if nextStatus := projectStatusFromFibePlayground(result.PlaygroundStatus); nextStatus != "" {
		status = nextStatus
		project.Status = nextStatus
	}
	if !hasSnapshot && status == "" {
		return project, nil
	}
	if status == "" {
		status = "ready"
	}
	if hasSnapshot {
		if err := s.store.SaveProjectProvisioningSnapshot(ctx, project, status); err != nil {
			return project, err
		}
	}
	if status == "error" {
		if err := s.store.UpdateProjectError(ctx, project.ID, user.ID, result.PlaygroundError); err != nil {
			return project, err
		}
	} else if !hasSnapshot && status != oldStatus {
		if err := s.store.UpdateProjectStatus(ctx, project.ID, user.ID, status); err != nil {
			return project, err
		}
	}
	updated, err := s.store.ProjectForUser(ctx, user.ID, project.ID)
	if err != nil {
		return project, err
	}
	return updated, nil
}

func projectStatusFromFibePlayground(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running", "ready", "has_changes":
		return "ready"
	case "pending", "in_progress", "starting", "creating":
		return "launching"
	case "stopping", "stopped":
		return "stopped"
	case "error", "failed":
		return "error"
	default:
		return ""
	}
}

func (s *Server) applyCurrentProjectAssignment(ctx context.Context, user *User, project *Project) error {
	cfg, err := s.store.ConfigMap(ctx)
	if err != nil {
		return err
	}
	assignment, changed, err := fibe.CurrentAssignmentForProject(cfg, project, project.ID)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	project.AgentID = assignment.AgentID
	project.MarqueeID = assignment.MarqueeID
	return nil
}

func projectNeedsReadinessRecovery(project *Project) bool {
	if project == nil {
		return false
	}
	switch project.Status {
	case "ready", "stopped", "deleting", "archived":
		return false
	}
	return strings.TrimSpace(project.PlaygroundID) != ""
}

func projectNeedsProvisioningRecovery(project *Project) bool {
	if project == nil {
		return false
	}
	switch project.Status {
	case "creating", "launching":
		return strings.TrimSpace(project.PlaygroundID) == "" &&
			!projectProvisioningLeaseActive(project) &&
			!projectProvisioningRecentlyTouched(project)
	default:
		return false
	}
}

func projectProvisioningLeaseActive(project *Project) bool {
	if project == nil || strings.TrimSpace(project.ProvisioningLockUntil) == "" {
		return false
	}
	lockUntil, err := time.Parse(time.RFC3339Nano, project.ProvisioningLockUntil)
	return err == nil && lockUntil.After(time.Now().UTC())
}

func projectProvisioningRecentlyTouched(project *Project) bool {
	if project == nil {
		return false
	}
	for _, raw := range []string{project.UpdatedAt, project.CreatedAt} {
		changedAt, err := time.Parse(time.RFC3339Nano, raw)
		if err == nil && time.Since(changedAt) >= 0 && time.Since(changedAt) < projectProvisioningRecoveryGrace {
			return true
		}
	}
	return false
}

func ensureProjectPlaygroundName(project *Project) string {
	if project == nil {
		return ""
	}
	if strings.TrimSpace(project.PlaygroundName) == "" {
		project.PlaygroundName = projecttext.SourceNameForProject(project)
	}
	return project.PlaygroundName
}

func (s *Server) promoteProjectFromReachablePreview(ctx context.Context, userID string, project *Project) (*Project, bool, string, bool, error) {
	if project == nil || strings.TrimSpace(project.PreviewURL) == "" {
		return project, false, "starting", false, nil
	}
	result, err := fibe.ProbePreviewURLResult(ctx, s.http, project.PreviewURL)
	if err != nil {
		return project, false, result.Status, result.Maintenance, err
	}
	if result.Maintenance {
		return project, false, result.Status, true, nil
	}
	if !result.Ready {
		return project, false, result.Status, false, nil
	}
	if userID != "" && project.Status != "ready" {
		if err := s.store.SaveProjectProvisioningSnapshot(ctx, project, "ready"); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return project, false, result.Status, false, err
		}
	}
	project.Status = "ready"
	project.ErrorMessage = ""
	return project, true, result.Status, false, nil
}

func previewEmbeddingBlocked(err error) bool {
	var blocked *fibe.PreviewEmbeddingBlockedError
	return errors.As(err, &blocked)
}

func mergeProjectGreenfieldResult(project *Project, result *fibe.GreenfieldResult) {
	if project == nil || result == nil {
		return
	}
	if strings.TrimSpace(project.PlaygroundID) == "" {
		project.PlaygroundID = result.PlaygroundID
	}
	if strings.TrimSpace(project.PlaygroundName) == "" {
		project.PlaygroundName = result.PlaygroundName
	}
	if strings.TrimSpace(project.PlaygroundName) == "" {
		project.PlaygroundName = projecttext.SourceNameForProject(project)
	}
	if strings.TrimSpace(project.PlayspecID) == "" {
		project.PlayspecID = result.PlayspecID
	}
	if strings.TrimSpace(project.PropID) == "" {
		project.PropID = result.PropID
	}
	if strings.TrimSpace(project.RepoURL) == "" {
		project.RepoURL = result.RepoURL
	}
	if strings.TrimSpace(project.PreviewURL) == "" {
		project.PreviewURL = result.PreviewURL
	}
	if strings.TrimSpace(project.SelectedService) == "" {
		project.SelectedService = result.SelectedServiceName
	}
	if len(project.Repositories) == 0 {
		project.Repositories = projectRepositoriesFromGreenfield(project.ID, result)
	}
	if len(project.Services) == 0 {
		project.Services = projectServicesFromGreenfield(project.ID, result)
	}
}

func greenfieldHasResourceSnapshot(result *fibe.GreenfieldResult) bool {
	return result != nil && (len(result.Repositories) > 0 || len(result.Services) > 0 || strings.TrimSpace(result.PreviewURL) != "" || strings.TrimSpace(result.PlayspecID) != "")
}

func applyProjectGreenfieldSnapshot(project *Project, result *fibe.GreenfieldResult) {
	if project == nil || result == nil {
		return
	}
	if strings.TrimSpace(result.PlaygroundID) != "" {
		project.PlaygroundID = result.PlaygroundID
	}
	if strings.TrimSpace(result.PlaygroundName) != "" {
		project.PlaygroundName = result.PlaygroundName
	}
	ensureProjectPlaygroundName(project)
	if strings.TrimSpace(result.PlayspecID) != "" {
		project.PlayspecID = result.PlayspecID
	}
	if len(result.Repositories) > 0 {
		project.Repositories = projectRepositoriesFromGreenfield(project.ID, result)
	}
	if len(result.Services) > 0 {
		project.Services = projectServicesFromGreenfield(project.ID, result)
	}
	project.SelectedService = selectProjectServiceName(project.SelectedService, result.SelectedServiceName, project.Services)
	if serviceURL := projectServiceURL(project.Services, project.SelectedService); serviceURL != "" {
		project.PreviewURL = serviceURL
	} else if strings.TrimSpace(result.PreviewURL) != "" {
		project.PreviewURL = result.PreviewURL
	}
	if repository := projectRepositoryForService(project.Repositories, project.SelectedService); repository != nil {
		project.PropID = firstNonEmpty(repository.PropID, project.PropID, result.PropID)
		project.RepoURL = firstNonEmpty(repository.RepoURL, project.RepoURL, result.RepoURL)
	} else {
		project.PropID = firstNonEmpty(result.PropID, project.PropID)
		project.RepoURL = firstNonEmpty(result.RepoURL, project.RepoURL)
	}
}

func selectProjectServiceName(current, preferred string, services []ProjectService) string {
	current = strings.TrimSpace(current)
	preferred = strings.TrimSpace(preferred)
	if serviceNameExists(services, current) {
		return current
	}
	if serviceNameExists(services, preferred) {
		return preferred
	}
	for _, service := range services {
		if strings.TrimSpace(service.Name) != "" {
			return service.Name
		}
	}
	return preferred
}

func serviceNameExists(services []ProjectService, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, service := range services {
		if strings.EqualFold(service.Name, name) {
			return true
		}
	}
	return false
}

func projectServiceURL(services []ProjectService, name string) string {
	name = strings.TrimSpace(name)
	for _, service := range services {
		if strings.EqualFold(service.Name, name) && strings.TrimSpace(service.URL) != "" {
			return service.URL
		}
	}
	return ""
}

func projectRepositoryForService(repositories []ProjectRepository, serviceName string) *ProjectRepository {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return nil
	}
	for i := range repositories {
		for _, candidate := range repositories[i].ServiceNames {
			if strings.EqualFold(candidate, serviceName) {
				return &repositories[i]
			}
		}
	}
	return nil
}

func projectRepositoriesFromGreenfield(projectID string, result *fibe.GreenfieldResult) []ProjectRepository {
	if result == nil {
		return nil
	}
	out := make([]ProjectRepository, 0, len(result.Repositories))
	for _, repository := range result.Repositories {
		out = append(out, ProjectRepository{
			ID:            uuid.NewString(),
			ProjectID:     projectID,
			Role:          repository.Role,
			PropID:        repository.PropID,
			RepoURL:       repository.RepoURL,
			SourceRepoURL: repository.SourceRepoURL,
			Provider:      repository.Provider,
			ServiceNames:  append([]string(nil), repository.ServiceNames...),
		})
	}
	return out
}

func projectServicesFromGreenfield(projectID string, result *fibe.GreenfieldResult) []ProjectService {
	if result == nil {
		return nil
	}
	out := make([]ProjectService, 0, len(result.Services))
	for _, service := range result.Services {
		out = append(out, ProjectService{
			ID:           uuid.NewString(),
			ProjectID:    projectID,
			Name:         service.Name,
			URL:          service.URL,
			Type:         service.Type,
			Visibility:   service.Visibility,
			AuthRequired: service.AuthRequired,
		})
	}
	return out
}
