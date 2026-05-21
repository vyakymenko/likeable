package likeable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fibegg/likeable/internal/domain"
	projecttext "github.com/fibegg/likeable/internal/project"
	"github.com/hibiken/asynq"
)

const projectProvisionRetryDelay = 2 * time.Minute
const projectProvisionUniqueTTL = 24 * time.Hour
const projectProvisionQueue = "provisioning"
const projectCleanupQueue = "cleanup"
const projectCleanupUniqueTTL = 30 * time.Minute
const projectCleanupTaskTimeout = 2 * time.Minute
const projectCleanupLeaseTTL = projectCleanupTaskTimeout + 30*time.Second
const projectCleanupSweepTimeout = 30 * time.Second
const projectDeletionSweepInterval = 5 * time.Minute
const projectDeletionSweepUniqueTTL = 4 * time.Minute
const maxConcurrentProjectCleanup = 1
const defaultJobWorkerConcurrency = 32
const idleProjectStopAfter = domain.PlaygroundIdleStopAfter

const (
	taskProvisionProject            = "likeable:project:provision"
	taskRecoverProject              = "likeable:project:recover"
	taskDeleteProjectResources      = "likeable:project:delete_resources"
	taskDeleteAccount               = "likeable:account:delete"
	taskProjectDeletionSweep        = "likeable:project:deletion_sweep"
	taskArchiveDeleteProject        = "likeable:project:archive_delete"
	taskStopIdleProjectsSweep       = "likeable:project:stop_idle_sweep"
	taskStopIdleProject             = "likeable:project:stop_idle"
	taskMonitorProjectNotifications = "likeable:project:monitor_notifications"
	taskSendEmail                   = "likeable:email:send"
	taskProjectQuotaSweep           = "likeable:project_quota:sweep"
)

var errProjectCleanupConcurrencyLimit = errors.New("project cleanup concurrency limit reached")

type JobSystem struct {
	client *asynq.Client
	server *asynq.Server
	mux    *asynq.ServeMux
}

type projectJobPayload struct {
	UserID    string `json:"user_id"`
	UserEmail string `json:"user_email"`
	ProjectID string `json:"project_id"`
	Prompt    string `json:"prompt,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type accountDeletionPayload struct {
	UserID    string `json:"user_id"`
	UserEmail string `json:"user_email"`
}

type emailJobPayload struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func newJobSystem(redisOpt asynq.RedisClientOpt, s *Server) *JobSystem {
	mux := asynq.NewServeMux()
	mux.HandleFunc(taskProvisionProject, s.handleProvisionProjectTask)
	mux.HandleFunc(taskRecoverProject, s.handleRecoverProjectTask)
	mux.HandleFunc(taskDeleteProjectResources, s.handleDeleteProjectResourcesTask)
	mux.HandleFunc(taskDeleteAccount, s.handleDeleteAccountTask)
	mux.HandleFunc(taskProjectDeletionSweep, s.handleProjectDeletionSweepTask)
	mux.HandleFunc(taskArchiveDeleteProject, s.handleArchiveDeleteProjectTask)
	mux.HandleFunc(taskStopIdleProjectsSweep, s.handleStopIdleProjectsSweepTask)
	mux.HandleFunc(taskStopIdleProject, s.handleStopIdleProjectTask)
	mux.HandleFunc(taskMonitorProjectNotifications, s.handleMonitorProjectNotificationsTask)
	mux.HandleFunc(taskSendEmail, s.handleSendEmailTask)
	mux.HandleFunc(taskProjectQuotaSweep, s.handleProjectQuotaSweepTask)
	server := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency:     jobWorkerConcurrency(),
		ShutdownTimeout: 20 * time.Second,
		Queues: map[string]int{
			projectProvisionQueue: 12,
			"critical":            6,
			"default":             4,
			projectCleanupQueue:   1,
			"low":                 1,
		},
		ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
			retried, _ := asynq.GetRetryCount(ctx)
			maxRetry, _ := asynq.GetMaxRetry(ctx)
			log.Printf("job %s failed retry=%d/%d: %v", task.Type(), retried, maxRetry, err)
		}),
	})
	return &JobSystem{client: asynq.NewClient(redisOpt), server: server, mux: mux}
}

func jobWorkerConcurrency() int {
	raw := strings.TrimSpace(os.Getenv("LIKEABLE_JOB_CONCURRENCY"))
	if raw == "" {
		return defaultJobWorkerConcurrency
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		log.Printf("invalid LIKEABLE_JOB_CONCURRENCY=%q; using %d", raw, defaultJobWorkerConcurrency)
		return defaultJobWorkerConcurrency
	}
	return value
}

func newJobClient(redisOpt asynq.RedisClientOpt) *JobSystem {
	return &JobSystem{client: asynq.NewClient(redisOpt)}
}

func (j *JobSystem) Start() {
	if j == nil || j.server == nil {
		return
	}
	go func() {
		if err := j.server.Run(j.mux); err != nil {
			log.Printf("asynq worker stopped: %v", err)
		}
	}()
}

func (j *JobSystem) Close() {
	if j == nil {
		return
	}
	if j.server != nil {
		j.server.Shutdown()
	}
	if j.client != nil {
		_ = j.client.Close()
	}
}

func (j *JobSystem) Run(ctx context.Context) error {
	if j == nil || j.server == nil {
		return errors.New("job worker is not configured")
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- j.server.Run(j.mux)
	}()
	select {
	case <-ctx.Done():
		j.server.Shutdown()
		err := <-errCh
		if err != nil {
			log.Printf("asynq worker stopped during shutdown: %v", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) enqueueProjectJob(ctx context.Context, taskType string, payload projectJobPayload, opts ...asynq.Option) error {
	if s.jobs == nil {
		return errors.New("job system is not configured")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.jobs.client.EnqueueContext(ctx, asynq.NewTask(taskType, data), opts...)
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return err
}

func (s *Server) enqueueAccountDeletionJob(ctx context.Context, payload accountDeletionPayload, opts ...asynq.Option) error {
	if s.jobs == nil {
		return errors.New("job system is not configured")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if len(opts) == 0 {
		opts = []asynq.Option{asynq.Queue(projectCleanupQueue), asynq.MaxRetry(30), asynq.Timeout(projectCleanupTaskTimeout)}
	}
	_, err = s.jobs.client.EnqueueContext(ctx, asynq.NewTask(taskDeleteAccount, data), opts...)
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return err
}

func (s *Server) enqueueEmailJob(ctx context.Context, payload emailJobPayload) error {
	if s.jobs == nil {
		return errors.New("job system is not configured")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.jobs.client.EnqueueContext(ctx, asynq.NewTask(taskSendEmail, data), asynq.Queue("low"), asynq.MaxRetry(8), asynq.Timeout(30*time.Second))
	if errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return err
}

func (s *Server) enqueueProjectQuotaSweep(ctx context.Context, delay time.Duration) {
	if s.jobs == nil {
		return
	}
	opts := []asynq.Option{asynq.Queue("low"), asynq.MaxRetry(2), asynq.Timeout(15 * time.Minute), asynq.Unique(50 * time.Minute)}
	if delay > 0 {
		opts = append(opts, asynq.ProcessIn(delay))
	}
	_, err := s.jobs.client.EnqueueContext(ctx, asynq.NewTask(taskProjectQuotaSweep, nil), opts...)
	if err != nil && !errors.Is(err, asynq.ErrDuplicateTask) {
		log.Printf("enqueue project quota sweep: %v", err)
	}
}

func (s *Server) enqueueProjectDeletionSweep(ctx context.Context, delay time.Duration) {
	if s.jobs == nil {
		return
	}
	opts := []asynq.Option{asynq.Queue(projectCleanupQueue), asynq.MaxRetry(2), asynq.Timeout(projectCleanupSweepTimeout), asynq.Unique(projectDeletionSweepUniqueTTL)}
	if delay > 0 {
		opts = append(opts, asynq.ProcessIn(delay))
	}
	_, err := s.jobs.client.EnqueueContext(ctx, asynq.NewTask(taskProjectDeletionSweep, nil), opts...)
	if err != nil && !errors.Is(err, asynq.ErrDuplicateTask) {
		log.Printf("enqueue project deletion sweep: %v", err)
	}
}

func (s *Server) enqueueStopIdleProjectsSweep(ctx context.Context, delay time.Duration) {
	if s.jobs == nil {
		return
	}
	opts := []asynq.Option{asynq.Queue("low"), asynq.MaxRetry(2), asynq.Timeout(15 * time.Minute), asynq.Unique(30 * time.Minute)}
	if delay > 0 {
		opts = append(opts, asynq.ProcessIn(delay))
	}
	_, err := s.jobs.client.EnqueueContext(ctx, asynq.NewTask(taskStopIdleProjectsSweep, nil), opts...)
	if err != nil && !errors.Is(err, asynq.ErrDuplicateTask) {
		log.Printf("enqueue idle playground stop sweep: %v", err)
	}
}

func (s *Server) startRecurringJobs(ctx context.Context) {
	if s.jobs == nil {
		return
	}
	s.enqueueProjectQuotaSweep(ctx, 0)
	s.enqueueProjectDeletionSweep(ctx, 0)
	s.enqueueStopIdleProjectsSweep(ctx, 0)
	go func() {
		hourlyTicker := time.NewTicker(time.Hour)
		deletionTicker := time.NewTicker(projectDeletionSweepInterval)
		defer hourlyTicker.Stop()
		defer deletionTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deletionTicker.C:
				s.enqueueProjectDeletionSweep(context.Background(), 0)
			case <-hourlyTicker.C:
				s.enqueueProjectQuotaSweep(context.Background(), 0)
				s.enqueueProjectDeletionSweep(context.Background(), 0)
				s.enqueueStopIdleProjectsSweep(context.Background(), 0)
			}
		}
	}()
}

func decodeTaskPayload[T any](task *asynq.Task) (T, error) {
	var payload T
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

func (s *Server) tryAcquireProjectCleanupSlot() (func(), bool) {
	s.cleanupOnce.Do(func() {
		s.cleanupSlots = make(chan struct{}, maxConcurrentProjectCleanup)
	})
	select {
	case s.cleanupSlots <- struct{}{}:
		return func() { <-s.cleanupSlots }, true
	default:
		return nil, false
	}
}

func (s *Server) handleProvisionProjectTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodeTaskPayload[projectJobPayload](task)
	if err != nil {
		return err
	}
	project, err := s.store.ProjectForUser(ctx, payload.UserID, payload.ProjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if err := s.provisionProject(ctx, payload.UserID, payload.UserEmail, project, payload.Prompt); err != nil {
		retryLater := retryProjectProvisionLater(project, err)
		retriesRemaining := taskRetriesRemaining(ctx)
		if !retriesRemaining && retryLater {
			retriesRemaining = s.enqueueDeferredProjectProvisionRetry(context.Background(), payload, projectProvisionRetryDelay)
		}
		s.recordProjectProvisionFailure(ctx, payload.UserID, project, err, retriesRemaining)
		if !retryLater {
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) enqueueDeferredProjectProvisionRetry(ctx context.Context, payload projectJobPayload, delay time.Duration) bool {
	if s.jobs == nil {
		return false
	}
	if delay <= 0 {
		delay = projectProvisionRetryDelay
	}
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("encode deferred project provisioning retry %s: %v", payload.ProjectID, err)
		return false
	}
	_, err = s.jobs.client.EnqueueContext(ctx, asynq.NewTask(taskProvisionProject, data), asynq.Queue(projectProvisionQueue), asynq.MaxRetry(6), asynq.Timeout(15*time.Minute), asynq.ProcessIn(delay), asynq.Unique(projectProvisionUniqueTTL))
	if errors.Is(err, asynq.ErrDuplicateTask) {
		log.Printf("deferred project provisioning retry %s was already queued", payload.ProjectID)
		return false
	}
	if err != nil {
		log.Printf("enqueue deferred project provisioning retry %s: %v", payload.ProjectID, err)
		return false
	}
	return true
}

func taskRetriesRemaining(ctx context.Context) bool {
	retried, retriedOK := asynq.GetRetryCount(ctx)
	maxRetry, maxRetryOK := asynq.GetMaxRetry(ctx)
	if !retriedOK || !maxRetryOK {
		return true
	}
	return retried < maxRetry
}

func (s *Server) handleRecoverProjectTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodeTaskPayload[projectJobPayload](task)
	if err != nil {
		return err
	}
	project, err := s.store.ProjectForUser(ctx, payload.UserID, payload.ProjectID)
	if err != nil || !projectNeedsReadinessRecovery(project) {
		return nil
	}
	if blocked, err := s.projectIsExportOnly(ctx, &User{ID: payload.UserID, Email: payload.UserEmail}, project); err != nil || blocked {
		return err
	}
	fibe, err := s.fibeClientForProject(ctx, project, payload.UserEmail)
	if err != nil {
		return err
	}
	return s.recoverProjectReadiness(ctx, payload.UserID, project, fibe)
}

func (s *Server) handleDeleteProjectResourcesTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodeTaskPayload[projectJobPayload](task)
	if err != nil {
		return err
	}
	project, err := s.store.ProjectForUser(ctx, payload.UserID, payload.ProjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	acquired, err := s.store.TryAcquireProjectCleanup(ctx, project.ID, payload.UserID, projectCleanupLeaseTTL)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	log.Printf("cleanup transition=retrying project_id=%s user_id=%s", project.ID, payload.UserID)
	defer func() {
		if err := s.store.ClearProjectCleanupLease(context.Background(), project.ID, payload.UserID); err != nil {
			log.Printf("clear project cleanup lease %s: %v", project.ID, err)
		}
	}()
	releaseCleanup, ok := s.tryAcquireProjectCleanupSlot()
	if !ok {
		return errProjectCleanupConcurrencyLimit
	}
	defer releaseCleanup()
	log.Printf("delete project %s resources: started", project.ID)
	if project.Status == "archived" {
		if err := s.deleteProjectLocally(ctx, project, payload.UserID); err != nil {
			_ = s.store.UpdateProjectCleanupError(context.Background(), project.ID, payload.UserID, err.Error())
			log.Printf("cleanup transition=failed project_id=%s user_id=%s error=%q", project.ID, payload.UserID, err.Error())
			return err
		}
		if err := s.finalizePendingAccountDeletionIfReady(ctx, accountDeletionPayload{UserID: payload.UserID, UserEmail: payload.UserEmail}); err != nil {
			return err
		}
		log.Printf("delete archived project %s locally: completed", project.ID)
		log.Printf("cleanup transition=succeeded project_id=%s user_id=%s", project.ID, payload.UserID)
		return nil
	}
	fibeClient, err := s.completeProjectResourceSnapshot(ctx, payload.UserEmail, project)
	if err != nil {
		if fibeClient == nil || !projectHasFibeResources(project) {
			_ = s.store.UpdateProjectCleanupError(context.Background(), project.ID, payload.UserID, err.Error())
			log.Printf("cleanup transition=failed project_id=%s user_id=%s error=%q", project.ID, payload.UserID, err.Error())
			return err
		}
		log.Printf("delete project %s resources: continuing with stored resources after snapshot error: %v", project.ID, err)
	}
	if projectHasFibeResources(project) {
		if err := fibeClient.DeleteProjectResources(ctx, project); err != nil {
			_ = s.store.UpdateProjectCleanupError(context.Background(), project.ID, payload.UserID, err.Error())
			log.Printf("cleanup transition=failed project_id=%s user_id=%s error=%q", project.ID, payload.UserID, err.Error())
			return err
		}
	} else {
		log.Printf("delete project %s resources: no remote resources found", project.ID)
	}
	if err := s.deleteProjectLocally(ctx, project, payload.UserID); err != nil {
		_ = s.store.UpdateProjectCleanupError(context.Background(), project.ID, payload.UserID, err.Error())
		log.Printf("cleanup transition=failed project_id=%s user_id=%s error=%q", project.ID, payload.UserID, err.Error())
		return err
	}
	if err := s.finalizePendingAccountDeletionIfReady(ctx, accountDeletionPayload{UserID: payload.UserID, UserEmail: payload.UserEmail}); err != nil {
		return err
	}
	log.Printf("delete project %s resources: completed", project.ID)
	log.Printf("cleanup transition=succeeded project_id=%s user_id=%s", project.ID, payload.UserID)
	return nil
}

func (s *Server) handleDeleteAccountTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodeTaskPayload[accountDeletionPayload](task)
	if err != nil {
		return err
	}
	return s.finalizeAccountDeletion(ctx, payload)
}

func (s *Server) finalizeAccountDeletion(ctx context.Context, payload accountDeletionPayload) error {
	user, err := s.store.UserByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	projects, err := s.store.AllProjectsForUser(ctx, payload.UserID)
	if err != nil {
		return err
	}
	if len(projects) > 0 {
		if s.jobs != nil {
			cleanupEmail := normalizeEmail(payload.UserEmail)
			if cleanupEmail == "" {
				cleanupEmail = user.Email
			}
			log.Printf("account deletion transition=waiting user_id=%s project_count=%d", user.ID, len(projects))
			for i := range projects {
				project := &projects[i]
				log.Printf("cleanup transition=queued project_id=%s user_id=%s source=account_deletion", project.ID, user.ID)
				if err := s.enqueueProjectJob(ctx, taskDeleteProjectResources, projectJobPayload{UserID: user.ID, UserEmail: cleanupEmail, ProjectID: project.ID}, asynq.Queue(projectCleanupQueue), asynq.MaxRetry(10), asynq.Timeout(projectCleanupTaskTimeout), asynq.Unique(projectCleanupUniqueTTL)); err != nil {
					return err
				}
			}
			return nil
		}
		return errors.New("account deletion pending project cleanup")
	}
	if err := s.store.RemoveEmailFromSignupAllowlist(ctx, payload.UserEmail); err != nil {
		return err
	}
	log.Printf("account deletion transition=finalizing user_id=%s", payload.UserID)
	if err := s.store.DeleteUser(ctx, payload.UserID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	log.Printf("delete account %s: completed", payload.UserID)
	return nil
}

func (s *Server) handleProjectDeletionSweepTask(ctx context.Context, _ *asynq.Task) error {
	projects, err := s.store.DeletingProjects(ctx, 100)
	if err != nil {
		return err
	}
	for i := range projects {
		project := &projects[i]
		user, err := s.store.UserByID(ctx, project.UserID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		log.Printf("cleanup transition=queued project_id=%s user_id=%s source=deletion_sweep", project.ID, user.ID)
		if err := s.enqueueProjectJob(ctx, taskDeleteProjectResources, projectJobPayload{UserID: user.ID, UserEmail: user.Email, ProjectID: project.ID}, asynq.Queue(projectCleanupQueue), asynq.MaxRetry(10), asynq.Timeout(projectCleanupTaskTimeout), asynq.Unique(projectCleanupUniqueTTL)); err != nil {
			return err
		}
	}
	return s.finalizeReadyAccountDeletions(ctx, 100)
}

func (s *Server) finalizeReadyAccountDeletions(ctx context.Context, limit int) error {
	users, err := s.store.PendingAccountDeletionUsers(ctx, accountDeletionAccessNote, limit)
	if err != nil {
		return err
	}
	for i := range users {
		user := &users[i]
		if err := s.finalizePendingAccountDeletionIfReady(ctx, accountDeletionPayload{UserID: user.ID, UserEmail: user.Email}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleArchiveDeleteProjectTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodeTaskPayload[projectJobPayload](task)
	if err != nil {
		return err
	}
	user, err := s.store.UserByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	project, err := s.store.ProjectForUser(ctx, payload.UserID, payload.ProjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	archive, err := s.archiveProjectSource(ctx, user, project)
	if err != nil {
		return err
	}
	if conn, connected, needsReconnect, err := s.githubExportConnection(ctx, user.ID); err != nil {
		log.Printf("archive project %s github export credential lookup failed: %v", project.ID, err)
	} else if connected && !needsReconnect && conn != nil {
		repoName := projecttext.SourceName(project.Title) + "-archive-" + strings.ReplaceAll(project.ID[:min(len(project.ID), 8)], "-", "")
		if repoURL, err := s.exportProjectToGithub(ctx, user, project, conn, repoName, true); err == nil {
			archive.GithubRepoURL = repoURL
			_ = s.store.UpsertProjectArchive(ctx, archive)
			s.notifyProjectExportReady(ctx, user, project, repoURL)
		} else {
			log.Printf("archive project %s github export failed: %v", project.ID, err)
		}
	}
	s.notifyProjectArchiveReady(ctx, user, project.Title, archive.DownloadURL, parseTimeOrZero(archive.ExpiresAt))
	if err := s.store.UpdateProjectStatus(ctx, project.ID, user.ID, "deleting"); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return s.enqueueProjectJob(ctx, taskDeleteProjectResources, projectJobPayload{UserID: user.ID, UserEmail: user.Email, ProjectID: project.ID}, asynq.Queue(projectCleanupQueue), asynq.MaxRetry(10), asynq.Timeout(projectCleanupTaskTimeout), asynq.Unique(projectCleanupUniqueTTL))
}

func (s *Server) handleSendEmailTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodeTaskPayload[emailJobPayload](task)
	if err != nil {
		return err
	}
	return s.sendEmail(ctx, emailMessage{To: payload.To, Subject: payload.Subject, Body: payload.Body})
}

func (s *Server) handleProjectQuotaSweepTask(ctx context.Context, _ *asynq.Task) error {
	users, err := s.store.UsersWithProjects(ctx)
	if err != nil {
		return err
	}
	for i := range users {
		user := &users[i]
		limit := s.projectCapForUser(ctx, user)
		excess, err := s.store.ProjectsExceedingQuota(ctx, user.ID, limit)
		if err != nil {
			return err
		}
		for j := range excess {
			project := &excess[j]
			if err := s.enqueueProjectJob(ctx, taskArchiveDeleteProject, projectJobPayload{UserID: user.ID, UserEmail: user.Email, ProjectID: project.ID, Reason: "project quota exceeded"}, asynq.Queue(projectCleanupQueue), asynq.MaxRetry(10), asynq.Timeout(projectCleanupTaskTimeout), asynq.Unique(24*time.Hour)); err != nil {
				return err
			}
		}
	}
	return s.cleanupExpiredArchives(ctx)
}

func (s *Server) handleStopIdleProjectsSweepTask(ctx context.Context, _ *asynq.Task) error {
	cutoff := time.Now().UTC().Add(-idleProjectStopAfter)
	projects, err := s.store.IdleProjectsForPlaygroundStop(ctx, cutoff, 100)
	if err != nil {
		return err
	}
	for i := range projects {
		project := &projects[i]
		user, err := s.store.UserByID(ctx, project.UserID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		if err := s.enqueueProjectJob(ctx, taskStopIdleProject, projectJobPayload{UserID: user.ID, UserEmail: user.Email, ProjectID: project.ID, Reason: "idle for 8 hours"}, asynq.Queue("low"), asynq.MaxRetry(6), asynq.Timeout(2*time.Minute), asynq.Unique(30*time.Minute)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleStopIdleProjectTask(ctx context.Context, task *asynq.Task) error {
	payload, err := decodeTaskPayload[projectJobPayload](task)
	if err != nil {
		return err
	}
	project, err := s.store.ProjectForUser(ctx, payload.UserID, payload.ProjectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	idle, skipReason, err := s.store.ProjectIdleForPlaygroundStop(ctx, project.ID, time.Now().UTC().Add(-idleProjectStopAfter))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if skipReason != "" {
		log.Printf("skip idle playground stop for project %s: %s", project.ID, skipReason)
		return nil
	}
	if !idle {
		return nil
	}
	user, err := s.store.UserByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	log.Printf("stop idle playground for project %s: no explicit playground activity for %s", project.ID, idleProjectStopAfter)
	_, err = s.controlProjectPlayground(ctx, user, project, "stop")
	return err
}

func (s *Server) cleanupExpiredArchives(ctx context.Context) error {
	archives, err := s.store.ExpiredArchives(ctx, 100)
	if err != nil {
		return err
	}
	for _, archive := range archives {
		if archive.StoragePath != "" {
			target := filepath.Clean(filepath.Join(s.store.DataDir(), archive.StoragePath))
			if strings.HasPrefix(target, filepath.Clean(filepath.Join(s.store.DataDir(), "archives"))+string(os.PathSeparator)) {
				_ = os.Remove(target)
			}
		}
		if err := s.store.DeleteProjectArchive(ctx, archive.ID); err != nil {
			return err
		}
	}
	return nil
}

func parseTimeOrZero(raw string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	return parsed
}
