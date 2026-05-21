package likeable

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

func (s *Server) addSystemNoticeAndEmail(ctx context.Context, user *User, severity, body, subject, emailBody string) {
	if user == nil {
		return
	}
	notice, err := s.store.AddUserNotice(ctx, UserNotice{UserID: user.ID, Sender: "system", Severity: severity, Body: body})
	if err != nil {
		log.Printf("add system notice for %s: %v", user.Email, err)
		return
	}
	if subject != "" && emailBody != "" {
		s.sendUserEmailAsync(user.Email, subject, emailBody)
	}
	_ = notice
}

func (s *Server) notifyHourQuotaIfNeeded(ctx context.Context, user *User) {
	if user == nil {
		return
	}
	limitMs := s.freeHourLimitMs(ctx)
	if limitMs <= 0 {
		return
	}
	windowHours := s.freeHourWindowHours(ctx)
	now := time.Now().UTC()
	windowStart, windowEnd := s.freeHourWindow(now, ctx)
	usedMs, err := s.store.UserWorkMsBetween(ctx, user.ID, windowStart, windowEnd, now)
	if err != nil {
		log.Printf("hour quota notice count for %s: %v", user.Email, err)
		return
	}
	remainingMs := limitMs - usedMs
	if remainingMs < 0 {
		remainingMs = 0
	}
	thresholdMs := limitMs / 5
	if thresholdMs < 15*60*1000 {
		thresholdMs = 15 * 60 * 1000
	}
	if thresholdMs > msPerHour {
		thresholdMs = msPerHour
	}
	paidRemainingMs, _ := s.store.PaidHourCreditBalance(ctx, user.ID)
	if remainingMs <= thresholdMs {
		prefix := "Hour quota:"
		exists, err := s.store.NoticeExistsSince(ctx, user.ID, "system", prefix, windowStart)
		if err == nil && !exists {
			body := fmt.Sprintf("%s You have %s/%s free build time remaining in this %d-hour window.", prefix, formatDurationForNotice(remainingMs), formatDurationForNotice(limitMs), windowHours)
			if paidRemainingMs > 0 {
				body += fmt.Sprintf(" Your %s paid build time is used only after free hours are spent.", formatDurationForNotice(paidRemainingMs))
			} else {
				body += " Buy an hour pack if you need more before the next reset."
			}
			s.addSystemNoticeAndEmail(ctx, user, "warning", body, "Likeable build hours running low", body+"\n\nManage hours:\n"+s.profileURL())
		}
	}
	if remainingMs == 0 && paidRemainingMs > 0 && paidRemainingMs <= msPerHour {
		prefix := "Paid hours:"
		exists, err := s.store.NoticeExistsSince(ctx, user.ID, "system", prefix, windowStart)
		if err == nil && !exists {
			body := fmt.Sprintf("%s You have %s paid build time left.", prefix, formatDurationForNotice(paidRemainingMs))
			s.addSystemNoticeAndEmail(ctx, user, "warning", body, "Likeable paid hours running low", body+"\n\nBuy more hours:\n"+s.profileURL())
		}
	}
}

func (s *Server) notifyProjectQuotaIfNeeded(ctx context.Context, user *User) {
	if user == nil {
		return
	}
	cap := s.projectCapForUser(ctx, user)
	if cap <= 0 {
		return
	}
	count, err := s.store.ProjectCountForUser(ctx, user.ID)
	if err != nil {
		log.Printf("project quota notice count for %s: %v", user.Email, err)
		return
	}
	remaining := cap - count
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 1 {
		return
	}
	prefix := fmt.Sprintf("Project quota: You are using %d/%d", count, cap)
	exists, err := s.store.NoticeExistsSince(ctx, user.ID, "system", prefix, time.Now().UTC().Add(-7*24*time.Hour))
	if err != nil || exists {
		return
	}
	body := prefix + " project slots."
	if remaining == 0 {
		body += " Delete or export older projects before creating another one."
	} else {
		body += " You have one project slot left."
	}
	s.addSystemNoticeAndEmail(ctx, user, "warning", body, "Likeable project quota running low", body+"\n\nManage projects:\n"+s.profileURL())
}

func (s *Server) notifyHourCreditsPurchased(ctx context.Context, userID string, hours int) {
	if hours <= 0 {
		return
	}
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		log.Printf("load user for purchase notice %s: %v", userID, err)
		return
	}
	body := fmt.Sprintf("Build hours purchased: %d paid build hour was added to your Likeable account.", hours)
	if hours != 1 {
		body = fmt.Sprintf("Build hours purchased: %d paid build hours were added to your Likeable account.", hours)
	}
	s.addSystemNoticeAndEmail(ctx, user, "info", body, "Likeable build hours added", body+"\n\nView your balance:\n"+s.profileURL())
}

func formatDurationForNotice(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	minutes := (ms + int64(time.Minute/time.Millisecond) - 1) / int64(time.Minute/time.Millisecond)
	hours := minutes / 60
	remMinutes := minutes % 60
	if hours > 0 && remMinutes > 0 {
		return fmt.Sprintf("%dh %dm", hours, remMinutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", remMinutes)
}

func (s *Server) notifyProjectQuotaPurchased(ctx context.Context, userID string, slots int, expiresAt time.Time) {
	if slots <= 0 {
		return
	}
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		log.Printf("load user for project quota purchase notice %s: %v", userID, err)
		return
	}
	body := fmt.Sprintf("Project quota purchased: %d extra project slot is active until %s.", slots, expiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	if slots != 1 {
		body = fmt.Sprintf("Project quota purchased: %d extra project slots are active until %s.", slots, expiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	s.addSystemNoticeAndEmail(ctx, user, "info", body, "Likeable project quota added", body+"\n\nManage projects:\n"+s.profileURL())
}

func (s *Server) notifyProjectExportReady(ctx context.Context, user *User, project *Project, repoURL string) {
	if user == nil || project == nil || strings.TrimSpace(repoURL) == "" {
		return
	}
	body := fmt.Sprintf("Project export ready: %q has been exported to GitHub.\n\n%s", project.Title, repoURL)
	s.addSystemNoticeAndEmail(ctx, user, "info", body, "Likeable project export ready", body)
}

func (s *Server) notifyProjectArchiveReady(ctx context.Context, user *User, projectTitle, downloadURL string, expiresAt time.Time) {
	if user == nil || strings.TrimSpace(downloadURL) == "" {
		return
	}
	body := fmt.Sprintf("Project archive ready: %q is ready to download.\n\n%s", projectTitle, downloadURL)
	if !expiresAt.IsZero() {
		body += "\n\nThis archive is scheduled to expire on " + expiresAt.UTC().Format("2006-01-02 15:04 UTC") + "."
	}
	s.addSystemNoticeAndEmail(ctx, user, "warning", body, "Likeable project archive ready", body)
}

func (s *Server) notifyProjectDeletionScheduled(ctx context.Context, user *User, project *Project) {
	if user == nil || project == nil {
		return
	}
	prefix := fmt.Sprintf("Project deletion started: %q", project.Title)
	exists, err := s.store.NoticeExistsSince(ctx, user.ID, "system", prefix, time.Now().UTC().Add(-24*time.Hour))
	if err == nil && exists {
		return
	}
	if err != nil {
		log.Printf("project deletion notice dedupe for %s: %v", user.Email, err)
	}
	body := prefix + " and its workspace resources are being removed."
	s.addSystemNoticeAndEmail(ctx, user, "warning", body, "Likeable project deletion started", body)
}

func (s *Server) profileURL() string {
	return strings.TrimRight(s.config.BaseURL, "/") + "/profile"
}
