package likeable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	fibegateway "github.com/fibegg/likeable/internal/fibe"
	projecttext "github.com/fibegg/likeable/internal/project"
)

var (
	errInvalidPlaygroundAction  = errors.New("invalid playground action")
	errProjectPlaygroundMissing = errors.New("project has no playground")
)

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	switch r.Method {
	case http.MethodGet:
		projects, err := s.store.ProjectsForUser(r.Context(), user.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		projects = s.refreshProjectListResources(r.Context(), user, projects)
		s.recoverProjectsAsync(user.ID, user.Email, projects)
		projectQuota := s.projectQuota(r.Context(), user)
		writeJSON(w, http.StatusOK, map[string]any{"projects": projects, "projectCap": projectQuota["limit"], "projectQuota": projectQuota})
	case http.MethodPost:
		s.handleCreateProject(w, r, user)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) refreshProjectListResources(ctx context.Context, user *User, projects []Project) []Project {
	if user == nil || len(projects) == 0 {
		return projects
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out := append([]Project(nil), projects...)
	for i := range out {
		if strings.TrimSpace(out[i].PlaygroundID) == "" || out[i].Status == "deleting" {
			continue
		}
		updated, err := s.refreshProjectResourcesIfDue(ctx, user, &out[i])
		if err != nil {
			log.Printf("refresh project resources for list %s: %v", out[i].ID, err)
			continue
		}
		if updated != nil {
			out[i] = *updated
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request, user *User) {
	var body struct {
		Prompt  string `json:"prompt"`
		Title   string `json:"title"`
		Confirm bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusPreconditionRequired, "new project requires confirmation")
		return
	}
	body.Prompt = strings.TrimSpace(body.Prompt)
	if body.Prompt != "" {
		allowed, err := s.hourAllowance(r.Context(), user)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !allowed {
			writeError(w, http.StatusPaymentRequired, "hour pack required")
			return
		}
	}
	count, err := s.store.ProjectCountForUser(r.Context(), user.ID)
	if err != nil {
		log.Printf("count projects for user %s: %v", user.ID, err)
		writeError(w, http.StatusInternalServerError, "could not create the project")
		return
	}
	cap := s.projectCapForUser(r.Context(), user)
	if count >= cap {
		writeError(w, http.StatusForbidden, fmt.Sprintf("project cap reached (%d)", cap))
		return
	}
	title := projecttext.CleanTitle(body.Title)
	if title == "" && body.Prompt != "" {
		title = projecttext.TitleFromPrompt(body.Prompt)
	}
	if title == "" {
		title = projecttext.DefaultTitle(count)
	}
	project, err := s.createProjectRecord(r.Context(), user, title)
	if err != nil {
		log.Printf("create project record for user %s: %v", user.ID, err)
		writeError(w, http.StatusInternalServerError, "workspace configuration needs attention")
		return
	}
	if err := s.provisionProjectAsync(user.ID, user.Email, project.ID, body.Prompt); err != nil {
		log.Printf("schedule project provisioning for user %s project %s: %v", user.ID, project.ID, err)
		_ = s.store.DeleteProject(r.Context(), project.ID, user.ID)
		writeError(w, http.StatusInternalServerError, "could not schedule workspace provisioning")
		return
	}
	if body.Prompt != "" {
		_, _ = s.store.AddMessage(r.Context(), project.ID, "user", body.Prompt)
	}
	s.notifyProjectQuotaIfNeeded(r.Context(), user)
	created, _ := s.store.ProjectForUser(r.Context(), user.ID, project.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"project": created})
}

func (s *Server) handleProjectRoute(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/api/projects/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	project, err := s.store.ProjectForUser(r.Context(), user.ID, parts[0])
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.recoverProjectAsync(user.ID, user.Email, project)
			writeJSON(w, http.StatusOK, map[string]any{"project": project})
		case http.MethodPatch:
			s.handleProjectUpdate(w, r, user, project)
		case http.MethodDelete:
			s.handleProjectDelete(w, r, user, project)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	switch parts[1] {
	case "messages":
		s.handleProjectMessages(w, r, user, project)
	case "prompt-improve":
		s.handleProjectPromptImprove(w, r, user, project)
	case "feed":
		s.handleProjectFeed(w, r, user, project)
	case "preview-status":
		s.handleProjectPreviewStatus(w, r, project)
	case "agent":
		if len(parts) == 3 && parts[2] == "interrupt" {
			s.handleProjectAgentInterrupt(w, r, user, project)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
	case "playground":
		s.handleProjectPlaygroundAction(w, r, user, project)
	case "attachments":
		if len(parts) != 3 {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.handleProjectAttachment(w, r, project, parts[2])
	case "export":
		s.handleProjectExport(w, r, user, project)
	case "archive":
		s.handleProjectArchiveExport(w, r, user, project)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request, user *User, project *Project) {
	var body struct {
		Title               string `json:"title"`
		SelectedServiceName string `json:"selectedServiceName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	title := projecttext.CleanTitle(body.Title)
	selectedService := strings.TrimSpace(body.SelectedServiceName)
	if title == "" && selectedService == "" {
		writeError(w, http.StatusBadRequest, "project title or service required")
		return
	}
	if title != "" {
		if err := s.store.UpdateProjectTitle(r.Context(), project.ID, user.ID, title); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "project not found")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if selectedService != "" {
		if err := s.store.UpdateProjectSelectedService(r.Context(), project.ID, user.ID, selectedService); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "project service not found")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	updated, err := s.store.ProjectForUser(r.Context(), user.ID, project.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": updated})
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request, user *User, project *Project) {
	if project.Status == "deleting" {
		s.deleteProjectResourcesAsync(user.ID, user.Email, project)
		writeJSON(w, http.StatusAccepted, map[string]any{"project": project})
		return
	}
	if err := s.store.UpdateProjectStatus(r.Context(), project.ID, user.ID, "deleting"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	project.Status = "deleting"
	s.notifyProjectDeletionScheduled(r.Context(), user, project)
	s.deleteProjectResourcesAsync(user.ID, user.Email, project)
	writeJSON(w, http.StatusAccepted, map[string]any{"project": project})
}

func (s *Server) handleProjectAgentInterrupt(w http.ResponseWriter, r *http.Request, user *User, project *Project) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := s.ensureProjectDevelopmentAllowed(r.Context(), user, project); err != nil {
		writeError(w, http.StatusConflict, developmentBlockedMessage(err))
		return
	}
	fibe, err := s.fibeClientForProject(r.Context(), project, user.Email)
	if err != nil {
		log.Printf("interrupt workspace client for project %s: %v", project.ID, err)
		writeError(w, http.StatusServiceUnavailable, "workspace messaging is not configured")
		return
	}
	if err := fibe.Interrupt(r.Context(), project.ConversationID); err != nil {
		s.observePlatformError(err)
		log.Printf("interrupt workspace message for project %s: %v", project.ID, err)
		if isPlatformRateLimited(err) {
			writeError(w, http.StatusServiceUnavailable, "workspace platform is rate limited; try again shortly")
		} else {
			writeError(w, http.StatusBadGateway, "could not stop the workspace agent")
		}
		return
	}
	s.clearPlatformBackoff()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleProjectPlaygroundAction(w http.ResponseWriter, r *http.Request, user *User, project *Project) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	updated, err := s.controlProjectPlayground(r.Context(), user, project, strings.ToLower(strings.TrimSpace(body.Action)))
	if err != nil {
		if errors.Is(err, errProjectExportOnly) || errors.Is(err, errProjectRetiring) {
			writeError(w, http.StatusConflict, developmentBlockedMessage(err))
			return
		}
		if errors.Is(err, errInvalidPlaygroundAction) || errors.Is(err, errProjectPlaygroundMissing) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("project playground action %s for project %s: %v", body.Action, project.ID, err)
		if isPlatformRateLimited(err) {
			writeError(w, http.StatusServiceUnavailable, "workspace platform is rate limited; try again shortly")
		} else {
			writeError(w, http.StatusBadGateway, "could not update the playground")
		}
		return
	}
	s.clearPlatformBackoff()
	writeJSON(w, http.StatusAccepted, map[string]any{"project": updated})
}

func (s *Server) controlProjectPlayground(ctx context.Context, user *User, project *Project, action string) (*Project, error) {
	if user == nil || project == nil {
		return nil, sql.ErrNoRows
	}
	if err := s.ensureProjectDevelopmentAllowed(ctx, user, project); err != nil {
		return nil, err
	}
	if project.Status == "deleting" {
		return nil, errInvalidPlaygroundAction
	}
	playgroundID := strings.TrimSpace(project.PlaygroundID)
	if playgroundID == "" {
		return nil, errProjectPlaygroundMissing
	}
	fibeClient, err := s.fibeClientForProject(ctx, project, user.Email)
	if err != nil {
		return nil, err
	}
	actionCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	nextStatus := "launching"
	switch action {
	case "start":
		err = fibeClient.StartPlayground(actionCtx, playgroundID)
	case "stop":
		nextStatus = "stopped"
		err = fibeClient.StopPlayground(actionCtx, playgroundID)
	case "restart":
		err = fibeClient.RestartPlayground(actionCtx, playgroundID)
	default:
		return nil, errInvalidPlaygroundAction
	}
	if err != nil {
		if action == "stop" && (fibegateway.IsPlaygroundAlreadyStoppedError(err) || fibegateway.IsPlaygroundMissingError(err)) {
			if updateErr := s.store.UpdateProjectStatus(ctx, project.ID, user.ID, "stopped"); updateErr != nil {
				return nil, updateErr
			}
			if touchErr := s.store.TouchProjectPlaygroundUsage(ctx, project.ID, user.ID); touchErr != nil {
				return nil, touchErr
			}
			return s.store.ProjectForUser(ctx, user.ID, project.ID)
		}
		s.observePlatformError(err)
		return nil, err
	}
	if err := s.store.UpdateProjectStatus(ctx, project.ID, user.ID, nextStatus); err != nil {
		return nil, err
	}
	if err := s.store.TouchProjectPlaygroundUsage(ctx, project.ID, user.ID); err != nil {
		return nil, err
	}
	return s.store.ProjectForUser(ctx, user.ID, project.ID)
}

func (s *Server) handleProjectFeed(w http.ResponseWriter, r *http.Request, user *User, project *Project) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if exportOnly, err := s.projectIsExportOnly(r.Context(), user, project); err != nil {
		log.Printf("load project feed binding for project %s: %v", project.ID, err)
	} else if exportOnly {
		local, _ := s.store.MessagesForProject(r.Context(), project.ID)
		timings, timingErr := s.store.ProjectNotificationTimingMap(r.Context(), project.ID)
		if timingErr != nil {
			log.Printf("load project notification timings for project %s: %v", project.ID, timingErr)
			timings = map[string]ProjectNotificationTiming{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"project": project, "localMessages": local, "messages": []any{}, "activity": []any{}, "live": nil, "notificationTimings": timings, "warning": "This project is archived. Export it or create a new project."})
		return
	}
	s.recoverProjectAsync(user.ID, user.Email, project)
	if project.Status == "ready" {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		if updated, err := s.refreshProjectResourcesIfDue(ctx, user, project); err == nil && updated != nil {
			project = updated
		} else if err != nil {
			log.Printf("refresh project resources for feed %s: %v", project.ID, err)
		}
		cancel()
	}
	snapshot, err := s.loadProjectFeedSnapshot(r.Context(), user, project, true)
	if err != nil {
		log.Printf("load project feed for project %s: %v", project.ID, err)
		writeError(w, http.StatusInternalServerError, "could not load project feed")
		return
	}
	writeJSON(w, http.StatusOK, snapshot.response())
}

func (s *Server) handleProjectPreviewStatus(w http.ResponseWriter, r *http.Request, project *Project) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user := userFromContext(r.Context())
	if exportOnly, err := s.projectIsExportOnly(r.Context(), user, project); err != nil {
		log.Printf("preview status binding for project %s: %v", project.ID, err)
	} else if exportOnly {
		writeJSON(w, http.StatusOK, map[string]any{
			"ready":     false,
			"status":    "archived",
			"checkedAt": nowString(),
			"project":   project,
		})
		return
	}
	readinessRefreshed := false
	if projectNeedsReadinessRecovery(project) && user != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		updated, err := s.refreshProjectReadiness(ctx, user, project)
		cancel()
		if err == nil && updated != nil {
			project = updated
			readinessRefreshed = true
		} else {
			log.Printf("preview status recovery for project %s is still pending: %v", project.ID, err)
			s.recoverProjectAsync(user.ID, user.Email, project)
		}
	}
	if user != nil && !readinessRefreshed && strings.TrimSpace(project.PlaygroundID) != "" && project.Status != "deleting" {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		updated, err := s.refreshProjectResourcesIfDue(ctx, user, project)
		cancel()
		if err == nil && updated != nil {
			project = updated
		} else if err != nil {
			log.Printf("preview status project resource refresh %s: %v", project.ID, err)
		}
	}
	if project.Status == "stopped" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ready":     false,
			"status":    "stopped",
			"checkedAt": nowString(),
			"project":   project,
		})
		return
	}
	if strings.TrimSpace(project.PreviewURL) == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ready":     false,
			"status":    publicPreviewProbeStatus(project.Status),
			"checkedAt": nowString(),
			"project":   project,
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, ready, status, maintenance, err := s.promoteProjectFromReachablePreview(ctx, project.UserID, project)
	if err != nil {
		log.Printf("preview status probe for project %s failed: %v", project.ID, err)
		ready = false
		maintenance = false
		status = "starting"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":       ready,
		"maintenance": maintenance,
		"status":      publicPreviewProbeStatus(status),
		"checkedAt":   nowString(),
		"project":     project,
	})
}

func publicPreviewProbeStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "starting"
	}
	code := strings.Fields(status)
	if len(code) == 0 || len(code[0]) != 3 || code[0][0] < '0' || code[0][0] > '9' || code[0][1] < '0' || code[0][1] > '9' || code[0][2] < '0' || code[0][2] > '9' {
		return "starting"
	}
	if code[0] == "404" {
		return "starting"
	}
	return status
}
