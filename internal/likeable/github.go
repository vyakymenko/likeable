package likeable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	projecttext "github.com/fibegg/likeable/internal/project"
	"golang.org/x/oauth2"
)

var (
	githubRepoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	urlUserInfoPattern    = regexp.MustCompile(`https?://[^\s/]+@`)
	githubOAuthScopes     = []string{"repo", "workflow"}
	errGithubRepoExists   = errors.New("github repository already exists")
)

func (s *Server) githubOAuthConfig(ctx context.Context) (*oauth2.Config, error) {
	cfg, err := s.store.ConfigMap(ctx)
	if err != nil {
		return nil, err
	}
	clientID := strings.TrimSpace(cfg["github_client_id"])
	clientSecret := strings.TrimSpace(cfg["github_client_secret"])
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("GitHub OAuth is not configured")
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  s.config.BaseURL + "/api/profile/github/callback",
		Scopes:       githubOAuthScopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://github.com/login/oauth/authorize",
			TokenURL: "https://github.com/login/oauth/access_token",
		},
	}, nil
}

func (s *Server) handleGithubStart(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.githubOAuthConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	state := randomToken()
	http.SetCookie(w, &http.Cookie{Name: "likeable_github_state", Value: state, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(s.config.BaseURL), MaxAge: 600})
	http.Redirect(w, r, cfg.AuthCodeURL(state), http.StatusFound)
}

func (s *Server) handleGithubCallback(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	stateCookie, err := r.Cookie("likeable_github_state")
	if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
		writeError(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	cfg, err := s.githubOAuthConfig(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	token, err := cfg.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "github oauth exchange failed")
		return
	}
	login, err := githubLogin(r.Context(), s.http, token.AccessToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	err = s.store.UpsertSocialConnection(r.Context(), SocialConnection{
		UserID:         user.ID,
		Provider:       "github",
		ProviderUserID: login,
		AccessToken:    token.AccessToken,
		Scope:          githubTokenScope(token),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/profile", http.StatusFound)
}

func githubLogin(ctx context.Context, client *http.Client, token string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("github profile failed: %s", resp.Status)
	}
	var out struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Login == "" {
		return "", fmt.Errorf("github login missing")
	}
	return out.Login, nil
}

func (s *Server) githubExportConnection(ctx context.Context, userID string) (*SocialConnection, bool, bool, error) {
	conn, err := s.store.SocialConnection(ctx, userID, "github")
	if err == nil && conn != nil {
		if githubScopeIncludes(conn.Scope, "workflow") {
			return conn, true, false, nil
		}
		fallback, err := s.configuredGithubExportConnection(ctx)
		if err != nil {
			return nil, false, false, err
		}
		if fallback != nil {
			return fallback, true, false, nil
		}
		return conn, true, true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, false, err
	}
	fallback, err := s.configuredGithubExportConnection(ctx)
	if err != nil {
		return nil, false, false, err
	}
	if fallback != nil {
		return fallback, true, false, nil
	}
	return nil, false, false, nil
}

func (s *Server) configuredGithubExportConnection(ctx context.Context) (*SocialConnection, error) {
	cfg, err := s.store.ConfigMap(ctx)
	if err != nil {
		return nil, err
	}
	token := firstNonEmptyString(cfg["github_token"], os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"))
	if token == "" {
		return nil, nil
	}
	return &SocialConnection{
		Provider:       "github",
		ProviderUserID: firstNonEmptyString(cfg["github_username"], os.Getenv("GITHUB_USERNAME"), os.Getenv("GITHUB_OWNER"), os.Getenv("GH_USERNAME"), os.Getenv("GH_OWNER")),
		AccessToken:    token,
		Scope:          strings.Join(githubOAuthScopes, ","),
	}, nil
}

func (s *Server) handleProjectExport(w http.ResponseWriter, r *http.Request, user *User, project *Project) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if project.Status == "archived" {
		writeError(w, http.StatusConflict, "Archived projects can be downloaded as ZIP exports.")
		return
	}
	var body struct {
		RepoName string `json:"repoName"`
		Private  bool   `json:"private"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(body.RepoName) == "" {
		body.RepoName = projecttext.SourceName(project.Title)
	}
	body.RepoName = strings.TrimSpace(body.RepoName)
	if !githubRepoNamePattern.MatchString(body.RepoName) {
		writeError(w, http.StatusBadRequest, "repoName may only contain letters, numbers, dots, underscores, and hyphens")
		return
	}
	conn, connected, needsReconnect, err := s.githubExportConnection(r.Context(), user.ID)
	if err != nil {
		log.Printf("load github export connection for user %s failed: %v", user.ID, err)
		writeError(w, http.StatusInternalServerError, "GitHub export configuration could not be loaded")
		return
	}
	if !connected || conn == nil {
		writeError(w, http.StatusPreconditionRequired, "connect GitHub first")
		return
	}
	if needsReconnect {
		writeError(w, http.StatusPreconditionRequired, "Reconnect GitHub to grant workflow export permission.")
		return
	}
	jobID, _ := s.store.CreateExportJob(r.Context(), project.ID)
	repoURL, err := s.exportProjectToGithub(r.Context(), user, project, conn, body.RepoName, body.Private)
	if err != nil {
		log.Printf("export project %s to GitHub failed: %v", project.ID, err)
		message := publicGithubExportError(err)
		_ = s.store.FinishExportJob(r.Context(), jobID, "error", "", message)
		writeError(w, http.StatusBadGateway, message)
		return
	}
	_ = s.store.FinishExportJob(r.Context(), jobID, "success", repoURL, "")
	s.notifyProjectExportReady(r.Context(), user, project, repoURL)
	writeJSON(w, http.StatusOK, map[string]any{"githubRepoUrl": repoURL, "jobId": jobID})
}

func (s *Server) exportProjectToGithub(ctx context.Context, user *User, project *Project, conn *SocialConnection, repoName string, private bool) (string, error) {
	repoURL, err := createGithubRepo(ctx, s.http, conn.AccessToken, conn.ProviderUserID, repoName, private)
	if err != nil {
		return "", err
	}
	owner := githubExportOwner(repoURL, conn.ProviderUserID)
	if owner == "" {
		return "", fmt.Errorf("github repository owner missing")
	}
	fibe, err := s.fibeClientForProject(ctx, project, user.Email)
	if err != nil {
		return "", err
	}
	giteaToken, err := fibe.GiteaToken(ctx)
	if err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp("", "likeable-export-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	sourceURL := withBasicAuth(project.RepoURL, giteaToken["username"], giteaToken["token"])
	targetURL := withBasicAuth("https://github.com/"+owner+"/"+repoName+".git", "x-access-token", conn.AccessToken)
	if err := runGit(ctx, temp, "clone", sourceURL, "."); err != nil {
		return "", err
	}
	if err := runGit(ctx, temp, "remote", "add", "github", targetURL); err != nil {
		return "", err
	}
	if err := runGit(ctx, temp, "push", "github", "HEAD:main", "--force"); err != nil {
		return "", err
	}
	return repoURL, nil
}

func githubTokenScope(token *oauth2.Token) string {
	if token == nil {
		return strings.Join(githubOAuthScopes, ",")
	}
	if scope, ok := token.Extra("scope").(string); ok && strings.TrimSpace(scope) != "" {
		return scope
	}
	return strings.Join(githubOAuthScopes, ",")
}

func githubScopeIncludes(scope, required string) bool {
	required = strings.ToLower(strings.TrimSpace(required))
	for _, part := range strings.FieldsFunc(strings.ToLower(scope), func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\t' || r == '\n' || r == '\r'
	}) {
		if strings.TrimSpace(part) == required {
			return true
		}
	}
	return false
}

func publicGithubExportError(err error) string {
	if errors.Is(err, errGithubRepoExists) {
		return "A GitHub repository with this name already exists. Choose another repository name, then export again."
	}
	message := strings.ToLower(strings.TrimSpace(fmt.Sprint(err)))
	switch {
	case strings.Contains(message, "workflow") && strings.Contains(message, "scope"):
		return "GitHub rejected workflow files. Grant workflow permission to the GitHub credential, then export again."
	case strings.Contains(message, "authentication failed") || strings.Contains(message, "bad credentials") || strings.Contains(message, "401"):
		return "GitHub authentication failed. Reconnect GitHub or update the configured GitHub token, then export again."
	default:
		return "Export failed. Try again later."
	}
}

func createGithubRepo(ctx context.Context, client *http.Client, token, owner, name string, private bool) (string, error) {
	body := strings.NewReader(fmt.Sprintf(`{"name":%q,"private":%t,"auto_init":false}`, name, private))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/user/repos", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	responseBody := readLimitedResponseBody(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", githubRepoCreateError(resp.Status, responseBody)
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		Owner   struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	_ = json.Unmarshal(responseBody, &out)
	if out.HTMLURL != "" {
		return out.HTMLURL, nil
	}
	if owner == "" {
		owner = strings.TrimSpace(out.Owner.Login)
	}
	if owner == "" {
		return "", fmt.Errorf("github repository owner missing")
	}
	return "https://github.com/" + owner + "/" + name, nil
}

func readLimitedResponseBody(body io.Reader) []byte {
	data, _ := io.ReadAll(io.LimitReader(body, 1<<20))
	return data
}

func githubRepoCreateError(status string, body []byte) error {
	message, validation := parseGithubErrorMessage(body)
	lower := strings.ToLower(message + "\n" + validation)
	if strings.Contains(lower, "already exists") || strings.Contains(lower, "name already exists") || strings.Contains(lower, "already_exists") {
		if message == "" {
			message = status
		}
		return fmt.Errorf("%w: %s", errGithubRepoExists, message)
	}
	if message != "" {
		return fmt.Errorf("github repo create failed: %s: %s", status, message)
	}
	return fmt.Errorf("github repo create failed: %s", status)
}

func parseGithubErrorMessage(body []byte) (string, string) {
	var out struct {
		Message string `json:"message"`
		Errors  []struct {
			Resource string `json:"resource"`
			Field    string `json:"field"`
			Code     string `json:"code"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return strings.TrimSpace(string(body)), ""
	}
	parts := make([]string, 0, len(out.Errors))
	for _, item := range out.Errors {
		parts = append(parts, strings.TrimSpace(strings.Join([]string{item.Resource, item.Field, item.Code, item.Message}, " ")))
	}
	return strings.TrimSpace(out.Message), strings.TrimSpace(strings.Join(parts, "\n"))
}

func githubOwnerFromRepoURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(parsed.Host, "github.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func githubExportOwner(createdRepoURL, configuredOwner string) string {
	if owner := githubOwnerFromRepoURL(createdRepoURL); owner != "" {
		return owner
	}
	return strings.TrimSpace(configuredOwner)
}

func withBasicAuth(raw, username, token string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parsed.User = url.UserPassword(username, token)
	return parsed.String()
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = filepath.Clean(dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		safeArgs := redactURLCredentials(strings.Join(args, " "))
		safeOutput := redactURLCredentials(strings.TrimSpace(string(output)))
		return fmt.Errorf("git %s failed: %s", safeArgs, safeOutput)
	}
	return nil
}

func redactURLCredentials(value string) string {
	return urlUserInfoPattern.ReplaceAllStringFunc(value, func(match string) string {
		if strings.HasPrefix(match, "https://") {
			return "https://[redacted]@"
		}
		return "http://[redacted]@"
	})
}
