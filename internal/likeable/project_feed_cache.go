package likeable

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	fibegateway "github.com/fibegg/likeable/internal/fibe"
)

const (
	projectFeedFullActiveTTL      = 6 * time.Second
	projectFeedFullIdleTTL        = 12 * time.Second
	projectFeedLiveActiveTTL      = 1200 * time.Millisecond
	projectFeedLiveIdleTTL        = 4 * time.Second
	projectFeedForegroundTTL      = 10 * time.Second
	projectFeedForegroundDelay    = 15 * time.Second
	projectFeedBackoffWarning     = "Live updates are paused briefly because the workspace platform is temporarily unavailable."
	projectFeedUnavailableWarning = "Live updates are temporarily unavailable."
	projectMessagesWarning        = "Workspace messages are temporarily unavailable."
	projectActivityWarning        = "Workspace activity is temporarily unavailable."
	projectLiveWarning            = "Live workspace status is temporarily unavailable."
)

type projectFeedCacheEntry struct {
	mu           sync.Mutex
	foregroundAt time.Time
	snapshot     *projectFeedSnapshot
}

type projectFeedSnapshot struct {
	project       Project
	local         []Message
	messages      []any
	activity      []any
	live          *fibegateway.ConversationLiveState
	timings       map[string]ProjectNotificationTiming
	warning       string
	fullFetchedAt time.Time
	liveFetchedAt time.Time
	shouldMonitor bool
}

func (s *Server) projectFeedEntry(projectID string) *projectFeedCacheEntry {
	value, _ := s.feedCache.LoadOrStore(projectID, &projectFeedCacheEntry{})
	return value.(*projectFeedCacheEntry)
}

func (s *Server) invalidateProjectFeedCache(projectID string) {
	if projectID == "" {
		return
	}
	value, ok := s.feedCache.Load(projectID)
	if !ok {
		return
	}
	entry := value.(*projectFeedCacheEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.snapshot != nil {
		entry.snapshot.fullFetchedAt = time.Time{}
		entry.snapshot.liveFetchedAt = time.Time{}
	}
}

func (s *Server) projectFeedForegroundRecent(projectID string, maxAge time.Duration) bool {
	if projectID == "" || maxAge <= 0 {
		return false
	}
	value, ok := s.feedCache.Load(projectID)
	if !ok {
		return false
	}
	entry := value.(*projectFeedCacheEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return !entry.foregroundAt.IsZero() && time.Since(entry.foregroundAt) < maxAge
}

func (s *Server) loadProjectFeedSnapshot(ctx context.Context, user *User, project *Project, foreground bool) (*projectFeedSnapshot, error) {
	if project == nil {
		return nil, nil
	}
	entry := s.projectFeedEntry(project.ID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	if foreground {
		entry.foregroundAt = now
	}

	local, err := s.store.MessagesForProject(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	base := s.baseProjectFeedSnapshot(ctx, project, local, entry.snapshot)
	if !base.fullFetchedAt.IsZero() && !base.liveFetchedAt.IsZero() {
		fullTTL, liveTTL := projectFeedTTLs(base.live)
		needFull := now.Sub(base.fullFetchedAt) >= fullTTL
		needLive := needFull || now.Sub(base.liveFetchedAt) >= liveTTL
		if !needFull && !needLive {
			return base.clone(), nil
		}
	}

	if !projectFeedWorkspaceAvailable(project) {
		entry.snapshot = base.clone()
		return base.clone(), nil
	}

	if _, ok := s.platformBackoffRemaining(); ok {
		base.warning = projectFeedBackoffWarning
		entry.snapshot = base.clone()
		return base.clone(), nil
	}

	if user == nil {
		base.warning = projectFeedUnavailableWarning
		entry.snapshot = base.clone()
		return base.clone(), nil
	}
	fibeClient, err := s.fibeClientForProject(ctx, project, user.Email)
	if err != nil {
		log.Printf("load project feed workspace client for project %s: %v", project.ID, err)
		base.warning = projectFeedUnavailableWarning
		entry.snapshot = base.clone()
		return base.clone(), nil
	}

	fullTTL, liveTTL := projectFeedTTLs(base.live)
	needFull := base.fullFetchedAt.IsZero() || now.Sub(base.fullFetchedAt) >= fullTTL
	needLive := needFull || base.liveFetchedAt.IsZero() || now.Sub(base.liveFetchedAt) >= liveTTL
	if needFull {
		s.refreshProjectFeedFull(ctx, fibeClient, project, base, now)
	} else if needLive {
		s.refreshProjectFeedLive(ctx, fibeClient, project, base, now)
	}
	if base.timings == nil {
		base.timings = map[string]ProjectNotificationTiming{}
	}
	entry.snapshot = base.clone()
	return base.clone(), nil
}

func (s *Server) baseProjectFeedSnapshot(ctx context.Context, project *Project, local []Message, cached *projectFeedSnapshot) *projectFeedSnapshot {
	if cached != nil {
		next := cached.clone()
		next.project = *project
		next.local = local
		return next
	}
	timings, err := s.store.ProjectNotificationTimingMap(ctx, project.ID)
	if err != nil {
		log.Printf("load project notification timings for project %s: %v", project.ID, err)
		timings = map[string]ProjectNotificationTiming{}
	}
	return &projectFeedSnapshot{
		project:  *project,
		local:    local,
		messages: []any{},
		activity: []any{},
		live:     &fibegateway.ConversationLiveState{ConversationID: project.ConversationID, IsProcessing: false, StreamText: "", QueuedTurns: 0},
		timings:  timings,
	}
}

func (s *Server) refreshProjectFeedFull(ctx context.Context, fibeClient *fibegateway.Client, project *Project, snapshot *projectFeedSnapshot, fetchedAt time.Time) {
	warnings := []string{}
	messages, err := fibeClient.Messages(ctx, project.ConversationID)
	if err != nil {
		s.observePlatformError(err)
		if fibegateway.IsConversationMissingError(err) {
			messages = []any{}
		} else {
			log.Printf("load project feed messages for project %s: %v", project.ID, err)
			warnings = append(warnings, warningForProjectFeedError(err, projectMessagesWarning))
			if isPlatformBackoffError(err) {
				snapshot.warning = joinWarnings(warnings)
				return
			}
			messages = snapshot.messages
		}
	} else {
		s.clearPlatformBackoff()
	}
	if messages == nil {
		messages = []any{}
	}

	if _, ok := s.platformBackoffRemaining(); !ok {
		activity, activityErr := fibeClient.Activity(ctx, project.ConversationID)
		if activityErr != nil {
			if fibegateway.IsConversationMissingError(activityErr) {
				activity = []any{}
			} else {
				log.Printf("load project feed activity for project %s: %v", project.ID, activityErr)
				// Activity is durable history, not the live control plane. Keep messages/live moving
				// when this optional endpoint is slow or times out.
				warnings = append(warnings, projectActivityWarning)
				activity = snapshot.activity
			}
		} else {
			s.clearPlatformBackoff()
		}
		if activity == nil {
			activity = []any{}
		}
		snapshot.activity = activity
	}

	if _, ok := s.platformBackoffRemaining(); !ok {
		live, liveErr := fibeClient.ConversationLiveState(ctx, project.ConversationID)
		if liveErr != nil {
			s.observePlatformError(liveErr)
			if fibegateway.IsConversationMissingError(liveErr) {
				live = idleConversationLiveState(project.ConversationID)
			} else {
				log.Printf("load project feed live state for project %s: %v", project.ID, liveErr)
				warnings = append(warnings, warningForProjectFeedError(liveErr, projectLiveWarning))
				live = idleConversationLiveState(project.ConversationID)
				if isPlatformBackoffError(liveErr) {
					snapshot.messages = sanitizeAgentProtocolMessages(messages)
					snapshot.warning = joinWarnings(warnings)
					return
				}
			}
		} else {
			s.clearPlatformBackoff()
		}
		snapshot.live = live
		snapshot.liveFetchedAt = fetchedAt
	}

	snapshot.messages = sanitizeAgentProtocolMessages(messages)
	if snapshot.activity == nil {
		snapshot.activity = []any{}
	}
	if snapshot.live == nil {
		snapshot.live = idleConversationLiveState(project.ConversationID)
	}
	sanitizeAgentProtocolLiveState(snapshot.live)
	snapshot.fullFetchedAt = fetchedAt
	if snapshot.liveFetchedAt.IsZero() {
		snapshot.liveFetchedAt = fetchedAt
	}
	snapshot.warning = joinWarnings(warnings)
	s.syncProjectFeedSnapshotTimings(ctx, snapshot)
}

func (s *Server) refreshProjectFeedLive(ctx context.Context, fibeClient *fibegateway.Client, project *Project, snapshot *projectFeedSnapshot, fetchedAt time.Time) {
	live, err := fibeClient.ConversationLiveState(ctx, project.ConversationID)
	if err != nil {
		s.observePlatformError(err)
		if fibegateway.IsConversationMissingError(err) {
			live = idleConversationLiveState(project.ConversationID)
		} else {
			log.Printf("load project feed live state for project %s: %v", project.ID, err)
			snapshot.warning = warningForProjectFeedError(err, projectLiveWarning)
			if isPlatformBackoffError(err) {
				return
			}
			live = idleConversationLiveState(project.ConversationID)
		}
	} else {
		s.clearPlatformBackoff()
		snapshot.warning = ""
	}
	snapshot.live = live
	if snapshot.live == nil {
		snapshot.live = idleConversationLiveState(project.ConversationID)
	}
	sanitizeAgentProtocolLiveState(snapshot.live)
	snapshot.liveFetchedAt = fetchedAt
	s.syncProjectFeedSnapshotTimings(ctx, snapshot)
}

func (s *Server) syncProjectFeedSnapshotTimings(ctx context.Context, snapshot *projectFeedSnapshot) {
	timings, shouldMonitor, err := s.syncProjectNotificationTimings(ctx, &snapshot.project, snapshot.local, snapshot.messages, snapshot.activity, snapshot.live)
	if err != nil {
		log.Printf("sync project notification timings for project %s: %v", snapshot.project.ID, err)
		if snapshot.timings == nil {
			snapshot.timings = map[string]ProjectNotificationTiming{}
		}
		return
	}
	snapshot.timings = timings
	snapshot.shouldMonitor = shouldMonitor
}

func projectFeedTTLs(live *fibegateway.ConversationLiveState) (time.Duration, time.Duration) {
	if projectFeedLiveActive(live) {
		return projectFeedFullActiveTTL, projectFeedLiveActiveTTL
	}
	return projectFeedFullIdleTTL, projectFeedLiveIdleTTL
}

func projectFeedLiveActive(live *fibegateway.ConversationLiveState) bool {
	if live == nil {
		return false
	}
	return live.IsProcessing || live.QueuedTurns > 0 || strings.TrimSpace(live.StreamText) != ""
}

func projectFeedWorkspaceAvailable(project *Project) bool {
	if project == nil {
		return false
	}
	return strings.TrimSpace(project.PlaygroundID) != "" &&
		strings.TrimSpace(project.AgentID) != "" &&
		strings.TrimSpace(project.ConversationID) != ""
}

func warningForProjectFeedError(err error, fallback string) string {
	if isPlatformBackoffError(err) {
		return projectFeedBackoffWarning
	}
	return fallback
}

func idleConversationLiveState(conversationID string) *fibegateway.ConversationLiveState {
	return &fibegateway.ConversationLiveState{ConversationID: conversationID, IsProcessing: false, StreamText: "", QueuedTurns: 0}
}

func joinWarnings(warnings []string) string {
	if len(warnings) == 0 {
		return ""
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		warning = strings.TrimSpace(warning)
		if warning == "" || seen[warning] {
			continue
		}
		seen[warning] = true
		out = append(out, warning)
	}
	return strings.Join(out, " ")
}

func (snapshot *projectFeedSnapshot) response() map[string]any {
	if snapshot == nil {
		return map[string]any{}
	}
	local := snapshot.local
	if local == nil {
		local = []Message{}
	}
	messages := snapshot.messages
	if messages == nil {
		messages = []any{}
	}
	activity := snapshot.activity
	if activity == nil {
		activity = []any{}
	}
	timings := snapshot.timings
	if timings == nil {
		timings = map[string]ProjectNotificationTiming{}
	}
	response := map[string]any{
		"project":             snapshot.project,
		"localMessages":       local,
		"messages":            messages,
		"activity":            activity,
		"live":                cloneConversationLiveState(snapshot.live),
		"notificationTimings": timings,
	}
	if strings.TrimSpace(snapshot.warning) != "" {
		response["warning"] = strings.TrimSpace(snapshot.warning)
	}
	return response
}

func (snapshot *projectFeedSnapshot) clone() *projectFeedSnapshot {
	if snapshot == nil {
		return nil
	}
	next := *snapshot
	if snapshot.local != nil {
		next.local = append([]Message(nil), snapshot.local...)
	}
	if snapshot.messages != nil {
		next.messages = append([]any(nil), snapshot.messages...)
	}
	if snapshot.activity != nil {
		next.activity = append([]any(nil), snapshot.activity...)
	}
	next.live = cloneConversationLiveState(snapshot.live)
	if snapshot.timings != nil {
		next.timings = make(map[string]ProjectNotificationTiming, len(snapshot.timings))
		for key, value := range snapshot.timings {
			next.timings[key] = value
		}
	}
	return &next
}

func cloneConversationLiveState(live *fibegateway.ConversationLiveState) *fibegateway.ConversationLiveState {
	if live == nil {
		return nil
	}
	next := *live
	return &next
}
