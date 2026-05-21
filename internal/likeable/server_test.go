package likeable

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fibegg/likeable/internal/fibe"
	projecttext "github.com/fibegg/likeable/internal/project"
	"github.com/fibegg/likeable/internal/store"
	"github.com/hibiken/asynq"
)

type captureEmailSender struct {
	ch chan emailMessage
}

func (c captureEmailSender) Send(_ context.Context, _ smtpSettings, message emailMessage) error {
	c.ch <- message
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testStripeSignature(secret string, body []byte) string {
	timestamp := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func responseClearsCookie(response *http.Response, name string) bool {
	for _, cookie := range response.Cookies() {
		if cookie.Name == name && cookie.MaxAge < 0 && cookie.Value == "" {
			return true
		}
	}
	return false
}

func responseHasCookie(response *http.Response, name string) bool {
	for _, cookie := range response.Cookies() {
		if cookie.Name == name && cookie.Value != "" {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, timeout time.Duration, check func() bool, failure func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(failure())
}

func TestHealthzChecksSQLite(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz returned %d: %s", rec.Code, rec.Body.String())
	}
}

func TestStaticPWAHeaders(t *testing.T) {
	webDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(webDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"index.html":                 "<!doctype html><div id=\"root\"></div>",
		"service-worker.js":          "self.addEventListener('fetch', () => {})",
		"manifest.webmanifest":       `{"name":"Likeable"}`,
		"offline.html":               "<!doctype html><h1>Offline</h1>",
		"icon.svg":                   "<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>",
		"assets/index-AbCdEf1234.js": "console.log('ok')",
	} {
		if err := os.WriteFile(filepath.Join(webDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{config: RuntimeConfig{WebDir: webDir}}

	for _, tc := range []struct {
		path         string
		cacheControl string
		contentType  string
		swAllowed    string
	}{
		{path: "/service-worker.js", cacheControl: "no-store, no-cache, max-age=0, must-revalidate", contentType: "application/javascript", swAllowed: "/"},
		{path: "/manifest.webmanifest", cacheControl: "no-cache", contentType: "application/manifest+json"},
		{path: "/offline.html", cacheControl: "no-cache", contentType: "text/html"},
		{path: "/assets/index-AbCdEf1234.js", cacheControl: "public, max-age=31536000, immutable", contentType: "application/javascript"},
		{path: "/profile", cacheControl: "no-cache", contentType: "text/html"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()

			server.handleStatic(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("static returned %d for %s: %s", rec.Code, tc.path, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != tc.cacheControl {
				t.Fatalf("Cache-Control=%q, want %q", got, tc.cacheControl)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tc.contentType) {
				t.Fatalf("Content-Type=%q, want to contain %q", got, tc.contentType)
			}
			if got := rec.Header().Get("Service-Worker-Allowed"); got != tc.swAllowed {
				t.Fatalf("Service-Worker-Allowed=%q, want %q", got, tc.swAllowed)
			}
		})
	}
}

func TestBootstrapConfigRejectsMissingOrWrongToken(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	body := `{"google_client_id":"client","google_client_secret":"secret"}`
	for _, tc := range []struct {
		name   string
		token  string
		header string
		status int
	}{
		{name: "disabled", token: "", header: "Bearer deploy-token", status: http.StatusNotFound},
		{name: "placeholder", token: "placeholder", header: "Bearer placeholder", status: http.StatusNotFound},
		{name: "missing", token: "deploy-token", header: "", status: http.StatusUnauthorized},
		{name: "wrong", token: "deploy-token", header: "Bearer wrong-token", status: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", BootstrapToken: tc.token}, http: http.DefaultClient}
			req := httptest.NewRequest(http.MethodPost, "/api/bootstrap/config", strings.NewReader(body))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()

			server.routes().ServeHTTP(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("bootstrap returned %d, want %d; body=%s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestBootstrapConfigWritesGoogleConfigWithoutExposingSecret(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", BootstrapToken: "deploy-token"}, http: http.DefaultClient}
	req := httptest.NewRequest(http.MethodPost, "/api/bootstrap/config", strings.NewReader(`{"google_client_id":"client-id","google_client_secret":"client-secret","signup_mode":"all"}`))
	req.Header.Set("Authorization", "Bearer deploy-token")
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "client-secret") || strings.Contains(rec.Body.String(), "google_client_secret") {
		t.Fatalf("bootstrap response exposed secret material: %s", rec.Body.String())
	}
	cfg, err := store.ConfigMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg["google_client_id"] != "client-id" || cfg["google_client_secret"] != "client-secret" || cfg["signup_mode"] != "all" {
		t.Fatalf("stored config=%+v, want google config and signup mode", cfg)
	}
	public := publicAdminConfig(cfg)
	entry := public["google_client_secret"].(map[string]any)
	if !entry["secret"].(bool) || !entry["set"].(bool) || entry["value"].(string) != "" {
		t.Fatalf("google secret public entry=%+v, want write-only secret", entry)
	}
	meReq := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	meRec := httptest.NewRecorder()
	server.routes().ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me returned %d: %s", meRec.Code, meRec.Body.String())
	}
	var me struct {
		Auth struct {
			GoogleConfigured bool   `json:"googleConfigured"`
			DevAuth          bool   `json:"devAuth"`
			SignupMode       string `json:"signupMode"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(meRec.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if !me.Auth.GoogleConfigured || me.Auth.DevAuth || me.Auth.SignupMode != "all" {
		t.Fatalf("me auth=%+v, want configured google, no dev auth, signup all", me.Auth)
	}
}

func TestGoogleStartShowsBrowserChoiceForInAppBrowser(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"google_client_id":     "client-id",
		"google_client_secret": "client-secret",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "https://likeable.test"}, http: http.DefaultClient}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/google/start", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 TelegramBot (likeable)")
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("google start returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/api/auth/google/start?force=1") || !strings.Contains(rec.Body.String(), "Open Likeable in Safari or Chrome") {
		t.Fatalf("browser choice page missing continue link or guidance: %s", rec.Body.String())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == "likeable_oauth_state" {
			t.Fatal("browser choice page should not set oauth state before the real browser starts auth")
		}
	}
}

func TestGoogleStartForceRedirectsWithAccountChoicePrompt(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"google_client_id":     "client-id",
		"google_client_secret": "client-secret",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "https://likeable.test"}, http: http.DefaultClient}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/google/start?force=1", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 TelegramBot (likeable)")
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("google start returned %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect location %q: %v", location, err)
	}
	if parsed.Host != "accounts.google.com" {
		t.Fatalf("redirect host=%q, want accounts.google.com", parsed.Host)
	}
	if parsed.Query().Get("prompt") != "select_account" {
		t.Fatalf("prompt=%q, want select_account in %s", parsed.Query().Get("prompt"), location)
	}
	if !responseHasCookie(rec.Result(), "likeable_oauth_state") {
		t.Fatal("forced google start should set oauth state cookie")
	}
}

func TestBootstrapConfigRejectsAfterUserExists(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.UpsertUser(t.Context(), "admin@example.com", "Admin", ""); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", BootstrapToken: "deploy-token"}, http: http.DefaultClient}
	req := httptest.NewRequest(http.MethodPost, "/api/bootstrap/config", strings.NewReader(`{"google_client_id":"client-id","google_client_secret":"client-secret"}`))
	req.Header.Set("Authorization", "Bearer deploy-token")
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("bootstrap returned %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := store.ConfigMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg["google_client_id"] != "" || cfg["google_client_secret"] != "" {
		t.Fatalf("bootstrap wrote config after user existed: %+v", cfg)
	}
}

func TestBootstrapConfigIsOneTime(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", BootstrapToken: "deploy-token"}, http: http.DefaultClient}
	first := httptest.NewRequest(http.MethodPost, "/api/bootstrap/config", strings.NewReader(`{"google_client_id":"client-id","google_client_secret":"client-secret"}`))
	first.Header.Set("Authorization", "Bearer deploy-token")
	firstRec := httptest.NewRecorder()
	server.routes().ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first bootstrap returned %d, want 200; body=%s", firstRec.Code, firstRec.Body.String())
	}
	second := httptest.NewRequest(http.MethodPost, "/api/bootstrap/config", strings.NewReader(`{"google_client_id":"other","google_client_secret":"other-secret"}`))
	second.Header.Set("Authorization", "Bearer deploy-token")
	secondRec := httptest.NewRecorder()
	server.routes().ServeHTTP(secondRec, second)
	if secondRec.Code != http.StatusConflict {
		t.Fatalf("second bootstrap returned %d, want 409; body=%s", secondRec.Code, secondRec.Body.String())
	}
	cfg, err := store.ConfigMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg["google_client_id"] != "client-id" || cfg["google_client_secret"] != "client-secret" {
		t.Fatalf("bootstrap was overwritten: %+v", cfg)
	}
}

func TestMeIncludesGithubConnectionState(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "github@example.com", "GitHub User", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "github-token", time.Hour); err != nil {
		t.Fatal(err)
	}

	readGithubState := func() (bool, bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "github-token"})
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("me returned %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		value, ok := body["githubConnected"].(bool)
		if !ok {
			t.Fatalf("githubConnected missing from /api/me response: %+v", body)
		}
		needsReconnect, ok := body["githubNeedsReconnect"].(bool)
		if !ok {
			t.Fatalf("githubNeedsReconnect missing from /api/me response: %+v", body)
		}
		return value, needsReconnect
	}

	connected, needsReconnect := readGithubState()
	if connected || needsReconnect {
		t.Fatalf("github state=%t/%t before connecting GitHub, want disconnected without reconnect prompt", connected, needsReconnect)
	}
	if err := store.UpsertSocialConnection(t.Context(), SocialConnection{UserID: user.ID, Provider: "github", ProviderUserID: "gh-user", AccessToken: "token", Scope: "repo"}); err != nil {
		t.Fatal(err)
	}
	connected, needsReconnect = readGithubState()
	if !connected || !needsReconnect {
		t.Fatalf("github state=%t/%t with repo-only scope, want connected with reconnect prompt", connected, needsReconnect)
	}
	if err := store.UpsertSocialConnection(t.Context(), SocialConnection{UserID: user.ID, Provider: "github", ProviderUserID: "gh-user", AccessToken: "token", Scope: "repo,workflow"}); err != nil {
		t.Fatal(err)
	}
	connected, needsReconnect = readGithubState()
	if !connected || needsReconnect {
		t.Fatalf("github state=%t/%t with workflow scope, want connected without reconnect prompt", connected, needsReconnect)
	}
}

func TestMeUsesConfiguredGithubFallback(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "github-fallback@example.com", "GitHub Fallback", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "github-fallback-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"github_username": "fallback-owner",
		"github_token":    "ghp_secret",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "github-fallback-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("me returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["githubConnected"] != true || body["githubNeedsReconnect"] != false {
		t.Fatalf("github state=%v/%v with configured fallback, want connected without reconnect", body["githubConnected"], body["githubNeedsReconnect"])
	}
}

func TestGithubExportConnectionPrefersConfiguredFallbackForRepoOnlyOAuth(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "github-fallback-export@example.com", "GitHub Export", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSocialConnection(t.Context(), SocialConnection{UserID: user.ID, Provider: "github", ProviderUserID: "repo-only", AccessToken: "oauth-token", Scope: "repo"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"github_username": "fallback-owner",
		"github_token":    "fallback-token",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}

	conn, connected, needsReconnect, err := server.githubExportConnection(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !connected || needsReconnect || conn == nil {
		t.Fatalf("github fallback state connected=%t needsReconnect=%t conn=%+v, want usable fallback", connected, needsReconnect, conn)
	}
	if conn.ProviderUserID != "fallback-owner" || conn.AccessToken != "fallback-token" {
		t.Fatalf("github fallback conn=%+v, want configured owner/token", conn)
	}
}

func TestGithubExportConnectionUsesEnvironmentFallback(t *testing.T) {
	t.Setenv("GITHUB_USERNAME", "env-owner")
	t.Setenv("GITHUB_TOKEN", "env-token")
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "github-env-export@example.com", "GitHub Env Export", "")
	if err != nil {
		t.Fatal(err)
	}

	conn, connected, needsReconnect, err := server.githubExportConnection(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !connected || needsReconnect || conn == nil {
		t.Fatalf("github env fallback state connected=%t needsReconnect=%t conn=%+v, want usable fallback", connected, needsReconnect, conn)
	}
	if conn.ProviderUserID != "env-owner" || conn.AccessToken != "env-token" {
		t.Fatalf("github env fallback conn=%+v, want env owner/token", conn)
	}
}

func TestMeFiltersTransientShellNotices(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "notices@example.com", "Notice User", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "notice-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := store.AddUserNotice(t.Context(), UserNotice{UserID: user.ID, Sender: "admin", Severity: "warning", Body: "Please review your account.", CreatedAt: now.Add(-30 * time.Minute).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddUserNotice(t.Context(), UserNotice{UserID: user.ID, Sender: "system", Severity: "warning", Body: "Project deletion started: \"Old app\" and its workspace resources are being removed.", CreatedAt: now.Add(-20 * time.Minute).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddUserNotice(t.Context(), UserNotice{UserID: user.ID, Sender: "system", Severity: "warning", Body: "Project quota: You are using 2/3 project slots. You have one project slot left.", CreatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "notice-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("me returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Notices []UserNotice `json:"notices"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Notices) != 1 || body.Notices[0].Body != "Please review your account." {
		t.Fatalf("notices=%+v, want only non-transient admin notice", body.Notices)
	}
}

func TestMeIncludesOnlyConfiguredBillingProducts(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "billing@example.com", "Billing User", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "billing-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	readProducts := func() struct {
		HourPacks    []int `json:"hourPacks"`
		ProjectQuota bool  `json:"projectQuota"`
	} {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "billing-token"})
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("me returned %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			BillingProducts struct {
				HourPacks    []int `json:"hourPacks"`
				ProjectQuota bool  `json:"projectQuota"`
			} `json:"billingProducts"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.BillingProducts
	}

	products := readProducts()
	if len(products.HourPacks) != 0 || products.ProjectQuota {
		t.Fatalf("billing products=%+v before Stripe config, want none", products)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"stripe_secret_key":             "sk_test",
		"stripe_price_id_1_hour":        "price_1h",
		"stripe_price_id_100_hours":     "price_100h",
		"stripe_project_quota_price_id": "price_project_slot",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	products = readProducts()
	if !reflect.DeepEqual(products.HourPacks, []int{1, 100}) || !products.ProjectQuota {
		t.Fatalf("billing products=%+v, want configured packs and project quota", products)
	}
}

func TestProjectEndpointsEnforceUserOwnership(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"}, http: http.DefaultClient}

	userA, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	userB, _ := store.UpsertUser(t.Context(), "b@example.com", "B", "")
	if err := store.CreateSession(t.Context(), userA.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), userB.ID, "token-b", time.Hour); err != nil {
		t.Fatal(err)
	}
	projectB := &Project{
		ID:             "project-b",
		UserID:         userB.ID,
		Title:          "B project",
		ConversationID: "likeable-secret-conversation",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), projectB); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/projects/project-b", ""},
		{http.MethodPatch, "/api/projects/project-b", `{"title":"stolen"}`},
		{http.MethodGet, "/api/projects/project-b/feed", ""},
		{http.MethodGet, "/api/projects/project-b/preview-status", ""},
		{http.MethodPost, "/api/projects/project-b/messages", `{"text":"steal"}`},
		{http.MethodPost, "/api/projects/project-b/export", `{"repoName":"steal"}`},
		{http.MethodPost, "/api/projects/project-b/archive", `{}`},
		{http.MethodDelete, "/api/projects/project-b", ""},
		{http.MethodGet, "/api/projects/likeable-secret-conversation/feed", ""},
		{http.MethodGet, "/api/projects/likeable-secret-conversation/preview-status", ""},
		{http.MethodPost, "/api/projects/likeable-secret-conversation/messages", `{"text":"steal"}`},
		{http.MethodPost, "/api/projects/likeable-secret-conversation/export", `{"repoName":"steal"}`},
		{http.MethodPost, "/api/projects/likeable-secret-conversation/archive", `{}`},
		{http.MethodPatch, "/api/projects/likeable-secret-conversation", `{"title":"stolen"}`},
		{http.MethodDelete, "/api/projects/likeable-secret-conversation", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
		rec := httptest.NewRecorder()

		server.routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s returned %d, want 404; body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestProjectExportRequiresWorkflowGithubScope(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "github@example.com", "GitHub User", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "github-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-export", UserID: user.ID, Title: "Export Me", ConversationID: "conv-export", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSocialConnection(t.Context(), SocialConnection{UserID: user.ID, Provider: "github", ProviderUserID: "gh-user", AccessToken: "token", Scope: "repo"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-export/export", strings.NewReader(`{"repoName":"export-me","private":true}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "github-token"})
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("export returned %d, want 428; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "workflow") {
		t.Fatalf("export body=%s, want workflow reconnect guidance", rec.Body.String())
	}
}

func TestCreateGithubRepoRejectsExistingRepository(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/user/repos" {
			t.Fatalf("unexpected GitHub path %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusUnprocessableEntity,
			Status:     "422 Unprocessable Entity",
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"message":"Validation Failed",
				"errors":[{"resource":"Repository","field":"name","code":"custom","message":"name already exists on this account"}]
			}`)),
		}, nil
	})}

	_, err := createGithubRepo(t.Context(), client, "token", "owner", "existing-repo", true)
	if !errors.Is(err, errGithubRepoExists) {
		t.Fatalf("err=%v, want errGithubRepoExists", err)
	}
	if got := publicGithubExportError(err); !strings.Contains(got, "already exists") {
		t.Fatalf("public error=%q, want already-exists guidance", got)
	}
}

func TestProjectArchiveExportCreatesDownloadableZip(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "archive@example.com", "Archive User", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "archive-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-archive", UserID: user.ID, Title: "Zip Me", ConversationID: "conv-archive", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-archive/archive", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "archive-token"})
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("archive export returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Archive     ProjectArchive `json:"archive"`
		DownloadURL string         `json:"downloadUrl"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Archive.ID == "" || body.DownloadURL == "" {
		t.Fatalf("archive response=%+v, want archive id and download URL", body)
	}
	downloadReq := httptest.NewRequest(http.MethodGet, "/api/profile/archives/"+body.Archive.ID+"/download", nil)
	downloadReq.AddCookie(&http.Cookie{Name: "likeable_session", Value: "archive-token"})
	downloadRec := httptest.NewRecorder()
	server.routes().ServeHTTP(downloadRec, downloadReq)
	if downloadRec.Code != http.StatusOK {
		t.Fatalf("archive download returned %d, want 200; body=%s", downloadRec.Code, downloadRec.Body.String())
	}
	reader, err := zip.NewReader(bytes.NewReader(downloadRec.Body.Bytes()), int64(downloadRec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) == 0 || reader.File[0].Name != "README.txt" {
		t.Fatalf("zip files=%+v, want fallback README", reader.File)
	}
}

func TestProjectTitleCanBeUpdatedByOwner(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-a",
		UserID:         user.ID,
		Title:          "Old name",
		ConversationID: "likeable-a",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/projects/project-a", strings.NewReader(`{"title":"  New project name  "}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("rename returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != "New project name" {
		t.Fatalf("title=%q, want cleaned title", stored.Title)
	}
}

func TestProjectResponsesDoNotExposePlatformInternals(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-a",
		UserID:         user.ID,
		Title:          "A project",
		ConversationID: "likeable-secret-conversation",
		AgentID:        "agent-1",
		MarqueeID:      "runner-1",
		PlaygroundID:   "workspace-1",
		PlayspecID:     "spec-1",
		PropID:         "prop-1",
		RepoURL:        "https://server.example.test/source/private.git",
		PreviewURL:     "https://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/projects", "/api/projects/project-a"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
		rec := httptest.NewRecorder()

		server.routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d, want 200; body=%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, token := range []string{
			`"conversationId"`,
			`"agentId"`,
			`"marqueeId"`,
			`"playgroundId"`,
			`"playspecId"`,
			`"propId"`,
			`"repoUrl"`,
			`"UserID"`,
			`"ConversationID"`,
			"server.example.test/source",
		} {
			if strings.Contains(body, token) {
				t.Fatalf("%s response leaks %q: %s", path, token, body)
			}
		}
		if !strings.Contains(body, "previewUrl") {
			t.Fatalf("%s response should still include public previewUrl: %s", path, body)
		}
	}
}

func TestDeletingProjectFreesProjectQuota(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	project := &Project{
		ID:             "project-a",
		UserID:         user.ID,
		Title:          "A project",
		ConversationID: "likeable-a",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateProjectStatus(t.Context(), project.ID, user.ID, "deleting"); err != nil {
		t.Fatal(err)
	}
	count, err := store.ProjectCountForUser(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count=%d, want 0", count)
	}
	projects, err := store.ProjectsForUser(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("listed projects=%d, want 0", len(projects))
	}
	deleting, err := store.DeletingProjects(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleting) != 1 || deleting[0].ID != project.ID {
		t.Fatalf("deleting projects=%+v, want hidden deletion row", deleting)
	}
	if err := store.UpdateProjectProvisioning(t.Context(), project.ID, user.ID, "playground", "playspec", "prop", "repo", "preview", "", "ready"); err == nil {
		t.Fatal("provisioning update should not resurrect a deleting project")
	}
	if err := store.UpdateProjectStatus(t.Context(), project.ID, user.ID, "launching"); err == nil {
		t.Fatal("non-deleting status update should not resurrect a deleting project")
	}
	if err := store.UpdateProjectError(t.Context(), project.ID, user.ID, "failed"); err == nil {
		t.Fatal("error update should not resurrect a deleting project")
	}
	stillDeleting, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillDeleting.Status != "deleting" {
		t.Fatalf("status=%q, want deleting", stillDeleting.Status)
	}
}

func TestProfileDeleteAllRequiresEmailConfirmation(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "delete-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/profile/delete-all", strings.NewReader(`{"email":"other@example.com"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "delete-token"})
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete-all returned %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := store.UserByID(t.Context(), user.ID); err != nil {
		t.Fatalf("user was deleted without matching email confirmation: %v", err)
	}
}

func TestProjectErrorsAreSanitized(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	project := &Project{
		ID:             "project-error",
		UserID:         user.ID,
		Title:          "A project",
		ConversationID: "likeable-error",
		Status:         "creating",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	raw := "Fibe server request failed: 404 Not Found"
	if err := store.UpdateProjectError(t.Context(), project.ID, user.ID, raw); err != nil {
		t.Fatal(err)
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	leaked := []string{"Fibe", "/api/", "conversations", "404", "Not Found"}
	for _, token := range leaked {
		if strings.Contains(stored.ErrorMessage, token) {
			t.Fatalf("stored error %q leaks %q", stored.ErrorMessage, token)
		}
	}
	if stored.ErrorMessage == "" || stored.ErrorMessage == raw {
		t.Fatalf("stored error=%q, want sanitized user-facing message", stored.ErrorMessage)
	}
}

func TestProjectPreviewStatusKeepsPlatform404BehindPlaceholder(t *testing.T) {
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusNotFound)
	}))
	defer previewServer.Close()
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-preview",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview",
		PreviewURL:     previewServer.URL,
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-preview/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready  bool   `json:"ready"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Ready {
		t.Fatal("404 preview must not be marked ready")
	}
	if body.Status != "starting" {
		t.Fatalf("status=%q, want sanitized starting", body.Status)
	}
}

func TestProjectPreviewStatusAllowsMaintenancePageThrough(t *testing.T) {
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<!doctype html><html><head><title>Maintenance</title></head><body><h1>maintenance is ongoing</h1></body></html>"))
	}))
	defer previewServer.Close()
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-preview-maintenance",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview-maintenance",
		PreviewURL:     previewServer.URL,
		Status:         "error",
		ErrorMessage:   "The canvas could not start.",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-preview-maintenance/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready       bool   `json:"ready"`
		Maintenance bool   `json:"maintenance"`
		Status      string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Ready {
		t.Fatal("maintenance must not mark the project runtime ready")
	}
	if !body.Maintenance {
		t.Fatalf("body=%+v, want maintenance marker", body)
	}
	if body.Status != "503 Service Unavailable" {
		t.Fatalf("status=%q, want raw 503 status for maintenance page", body.Status)
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "error" {
		t.Fatalf("status=%q, want maintenance probe to preserve project status", updated.Status)
	}
}

func TestProjectPreviewStatusKeepsPlain503BehindPlaceholder(t *testing.T) {
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer previewServer.Close()
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-preview-plain-503",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview-plain-503",
		PreviewURL:     previewServer.URL,
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-preview-plain-503/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready       bool `json:"ready"`
		Maintenance bool `json:"maintenance"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Ready || body.Maintenance {
		t.Fatalf("body=%+v, want ordinary 503 to stay behind Likeable placeholder", body)
	}
}

func TestProjectPreviewStatusRecoversErroredProjectWithResources(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-preview-recover",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview",
		PlaygroundID:   "playground-1",
		PreviewURL:     "http://preview.example.test",
		Status:         "error",
		ErrorMessage:   "The canvas could not start.",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-preview-recover/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready  bool   `json:"ready"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Ready || body.Status != "starting" {
		t.Fatalf("body=%+v, want not ready and retryable starting state", body)
	}
}

func TestProjectPreviewStatusPromotesReachablePreviewWithoutPlatformConfig(t *testing.T) {
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>ready</title>"))
	}))
	defer previewServer.Close()
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-preview-reachable-no-platform",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview",
		PlaygroundID:   "playground-1",
		PreviewURL:     previewServer.URL,
		Status:         "launching",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-preview-reachable-no-platform/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready  bool   `json:"ready"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready || body.Status != "200 OK" {
		t.Fatalf("body=%+v, want ready preview", body)
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "ready" {
		t.Fatalf("status=%q, want ready", updated.Status)
	}
}

func TestProjectPreviewStatusMarksReachablePreviewReady(t *testing.T) {
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>ready</title>"))
	}))
	defer previewServer.Close()
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-preview-ready",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview",
		PreviewURL:     previewServer.URL,
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-preview-ready/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready  bool   `json:"ready"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready {
		t.Fatalf("ready=false status=%q", body.Status)
	}
	if body.Status != "200 OK" {
		t.Fatalf("status=%q, want 200 OK", body.Status)
	}
}

func TestProjectPreviewStatusPromotesLaunchingReadyWorkspace(t *testing.T) {
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>ready</title>"))
	}))
	defer previewServer.Close()
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fakeFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-preview-promote",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     previewServer.URL,
		Status:         "launching",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-preview-promote/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready  bool   `json:"ready"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready || body.Status != "200 OK" {
		t.Fatalf("body=%+v, want ready preview", body)
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "ready" {
		t.Fatalf("status=%q, want ready", updated.Status)
	}
}

func TestProjectMessagePromotesLaunchingReadyWorkspace(t *testing.T) {
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>ready</title>"))
	}))
	defer previewServer.Close()
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, _, stdinPath := fakeFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-message-promote",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     previewServer.URL,
		Status:         "launching",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-message-promote/messages", strings.NewReader(`{"text":"change the heading"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message returned %d: %s", rec.Code, rec.Body.String())
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "ready" {
		t.Fatalf("status=%q, want ready", updated.Status)
	}
	if !strings.Contains(readFile(t, stdinPath), "change the heading") {
		t.Fatalf("agent prompt was not sent: %s", readFile(t, stdinPath))
	}
}

func TestProjectMessageBlocksProjectBoundToMissingPoolPair(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url":          "server.test:3000",
		"fibe_api_key":           "test-key",
		"fibe_agent_server_pool": `[{"agent_id":"current-agent","server_id":"current-marquee"}]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-stale-assignment",
		UserID:         user.ID,
		Title:          "Stale",
		ConversationID: "conv-stale",
		AgentID:        "stale-agent",
		MarqueeID:      "stale-marquee",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-stale-assignment/messages", strings.NewReader(`{"text":"change the heading"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("message returned %d, want 409: %s", rec.Code, rec.Body.String())
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "archived" {
		t.Fatalf("status=%q, want archived", updated.Status)
	}
}

func TestProjectMessageAllowsDrainingPoolPair(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, logPath, _ := fakeFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url":          "server.test:3000",
		"fibe_api_key":           "test-key",
		"fibe_agent_server_pool": `[{"agent_id":"draining-agent","server_id":"draining-marquee","status":"draining"},{"agent_id":"new-agent","server_id":"new-marquee","status":"active"}]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-draining", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-draining-assignment",
		UserID:         user.ID,
		Title:          "Draining",
		ConversationID: "conv-draining",
		AgentID:        "draining-agent",
		MarqueeID:      "draining-marquee",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-draining-assignment/messages", strings.NewReader(`{"text":"keep working"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-draining"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("message returned %d: %s", rec.Code, rec.Body.String())
	}
	if log := readFile(t, logPath); !strings.Contains(log, "agents send-message draining-agent") {
		t.Fatalf("commands=%s, want draining agent used", log)
	}
}

func TestProjectMessageIsStoredBeforeForwardingToAgent(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	markerPath := filepath.Join(dir, "send.started")
	releasePath := filepath.Join(dir, "send.release")
	stdinPath := filepath.Join(dir, "stdin.json")
	script := `#!/bin/sh
case "$*" in
  *"agents send-message"*)
    printf started > "` + markerPath + `"
    cat > "` + stdinPath + `"
    while [ ! -f "` + releasePath + `" ]; do sleep 0.05; done
    echo '{"ok":true}'
    ;;
  *"agents create-conversation"*)
    echo '{"ok":true}'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { _ = os.WriteFile(releasePath, []byte("release"), 0o644) })

	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:                   "project-message-order",
		UserID:               user.ID,
		Title:                "Order",
		ConversationID:       "conv-order",
		AgentID:              "agent-1",
		PreviewURL:           "http://preview.example.test",
		Status:               "ready",
		PlaygroundLastUsedAt: time.Now().UTC().Add(-9 * time.Hour).Format(time.RFC3339Nano),
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	previousLastUsedAt := project.PlaygroundLastUsedAt

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-message-order/messages", strings.NewReader(`{"text":"change the heading"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.routes().ServeHTTP(rec, req)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(markerPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("send command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	messages, err := store.MessagesForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Body != "change the heading" {
		t.Fatalf("messages=%+v, want local message committed before agent send finishes", messages)
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("message handler did not finish")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("message returned %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(readFile(t, stdinPath), "change the heading") {
		t.Fatalf("agent prompt was not sent: %s", readFile(t, stdinPath))
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PlaygroundLastUsedAt == "" || stored.PlaygroundLastUsedAt == previousLastUsedAt {
		t.Fatalf("playground_last_used_at=%q, previous=%q; want message touch", stored.PlaygroundLastUsedAt, previousLastUsedAt)
	}
}

func TestProjectMessageAttachmentFailureIsHumanReadable(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	script := `#!/bin/sh
case "$*" in
  *"agents send-message"*)
    cat >/dev/null
    printf '%s\n' '{"error":{"message":"Unsupported or blocked file type","code":"BAD_REQUEST","status":400}}' >&2
    exit 1
    ;;
  *"agents create-conversation"*)
    echo '{"ok":true}'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-message-webp",
		UserID:         user.ID,
		Title:          "Attachment",
		ConversationID: "conv-webp",
		AgentID:        "agent-1",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("text", "use this image"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("attachments", "mock.webp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("webp")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-message-webp/messages", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("message returned %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "workspace rejected this WEBP attachment") {
		t.Fatalf("body=%s, want human-readable WEBP message", rec.Body.String())
	}
	messages, err := store.MessagesForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages=%+v, want failed optimistic message removed", messages)
	}
}

func TestProjectMessageStartsOfflineAgentAndReturnsHumanReadableRetry(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	logPath := filepath.Join(dir, "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"agents create-conversation"*)
    printf '%s\n' '{"error":{"message":"No running AgentChat for Agent#1","code":"UNPROCESSABLE_ENTITY","status":422}}' >&2
    exit 1
    ;;
  *"agents start-chat"*)
    echo '{"id":1,"status":"pending"}'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-message-offline-agent",
		UserID:         user.ID,
		Title:          "Offline agent",
		ConversationID: "conv-offline-agent",
		AgentID:        "agent-1",
		MarqueeID:      "multipass",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-message-offline-agent/messages", strings.NewReader(`{"text":"change the heading"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("message returned %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "build agent was offline") || !strings.Contains(rec.Body.String(), "starting") {
		t.Fatalf("body=%s, want human-readable agent-starting message", rec.Body.String())
	}
	log := readFile(t, logPath)
	if !strings.Contains(log, "agents start-chat agent-1 --marquee-id multipass") {
		t.Fatalf("log=%s, want start-chat command after offline runtime", log)
	}
	if strings.Contains(log, "agents send-message") {
		t.Fatalf("log=%s, send-message must not run while agent runtime is offline", log)
	}
	messages, err := store.MessagesForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages=%+v, want no local message stored before conversation exists", messages)
	}
}

func TestProjectFeedTriggersReadinessRecovery(t *testing.T) {
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>ready</title>"))
	}))
	defer previewServer.Close()
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, _, _ := fakeFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-recover",
		UserID:         user.ID,
		Title:          "Preview",
		ConversationID: "conv-preview",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     previewServer.URL,
		Status:         "launching",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(cliPath)+string(os.PathListSeparator)+os.Getenv("PATH"))

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-recover/feed", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed returned %d: %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Status == "ready" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	updated, _ := store.ProjectForUser(t.Context(), user.ID, project.ID)
	t.Fatalf("project status=%q, want recovered ready", updated.Status)
}

func TestProjectFeedSkipsWorkspaceSnapshotBeforeProvisioning(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	logPath := filepath.Join(dir, "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
echo "unexpected command: $*" >&2
exit 64
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-creating",
		UserID:         user.ID,
		Title:          "Creating",
		ConversationID: "conv-feed-creating",
		AgentID:        "agent-1",
		Status:         "creating",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-creating/feed", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if data, err := os.ReadFile(logPath); err == nil && len(data) > 0 {
		t.Fatalf("commands=%s, want no workspace calls before provisioning", data)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestProjectFeedReturnsPartialSnapshotForTransientLiveStateFailure(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	script := `#!/bin/sh
case "$*" in
  *"agents messages"*|*"agents activity"*)
    echo '{"content":[]}'
    ;;
  *"agents live-state"*)
    printf '%s\n' '{"error":{"message":"Agent unreachable: connection refused","code":"AGENT_COMMUNICATION_FAILED","status":422}}' >&2
    exit 1
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-transient-live-failure",
		UserID:         user.ID,
		Title:          "Transient Live Failure",
		ConversationID: "conv-live-failure",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-transient-live-failure/feed", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("feed returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Warning string `json:"warning"`
		Live    struct {
			ConversationID string `json:"conversationId"`
			IsProcessing   bool   `json:"isProcessing"`
			QueuedTurns    int    `json:"queuedTurns"`
		} `json:"live"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Warning, "Live workspace status is temporarily unavailable") {
		t.Fatalf("warning=%q, want live-state warning", body.Warning)
	}
	if body.Live.ConversationID != project.ConversationID || body.Live.IsProcessing || body.Live.QueuedTurns != 0 {
		t.Fatalf("live=%+v, want explicit idle fallback for unavailable live state", body.Live)
	}
}

func TestProjectFeedSanitizesAgentNotificationProtocol(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	script := `#!/bin/sh
case "$*" in
  *"agents messages"*)
    echo '{"content":[{"role":"assistant","body":"hidden prose [[LIKEABLE_NOTIFICATION_START]]Checking the preview[[LIKEABLE_NOTIFICATION_END]] more prose [[LIKEABLE_NOTIFICATION_START]]Canvas updated[[LIKEABLE_NOTIFICATION_END]]"},{"role":"user","body":"keep user body"}]}'
    ;;
  *"agents activity"*)
    echo '{"content":[]}'
    ;;
  *"agents live-state"*)
    echo '{"conversationId":"conv-clean","isProcessing":true,"streamText":"thinking [[LIKEABLE_NOTIFICATION_START]]Updating files[[LIKEABLE_NOTIFICATION_END]] noisy"}'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-sanitize",
		UserID:         user.ID,
		Title:          "Sanitize",
		ConversationID: "conv-clean",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-sanitize/feed", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Messages []struct {
			Role string `json:"role"`
			Body string `json:"body"`
		} `json:"messages"`
		Live struct {
			StreamText string `json:"streamText"`
		} `json:"live"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	wantAssistant := "[[LIKEABLE_NOTIFICATION_START]]Checking the preview[[LIKEABLE_NOTIFICATION_END]][[LIKEABLE_NOTIFICATION_START]]Canvas updated[[LIKEABLE_NOTIFICATION_END]]"
	if len(body.Messages) != 2 || body.Messages[0].Body != wantAssistant || body.Messages[1].Body != "keep user body" {
		t.Fatalf("messages=%+v, want sanitized assistant and untouched user", body.Messages)
	}
	wantLive := "[[LIKEABLE_NOTIFICATION_START]]Updating files[[LIKEABLE_NOTIFICATION_END]]"
	if body.Live.StreamText != wantLive {
		t.Fatalf("live stream=%q, want %q", body.Live.StreamText, wantLive)
	}
}

func TestProjectFeedPreservesUserFacingAgentLiveErrors(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	script := `#!/bin/sh
case "$*" in
  *"agents messages"*)
    echo '{"content":[]}'
    ;;
  *"agents activity"*)
    echo '{"content":[]}'
    ;;
  *"agents live-state"*)
    echo '{"conversationId":"conv-auth","isProcessing":true,"streamText":"Invalid API key - Fix external API key"}'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-auth-error",
		UserID:         user.ID,
		Title:          "Auth Error",
		ConversationID: "conv-auth",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-auth-error/feed", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Live struct {
			StreamText string `json:"streamText"`
		} `json:"live"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	wantLive := likeableNotificationStart + "Build agent authentication failed. Check the Fibe agent provider key, then try again." + likeableNotificationEnd
	if body.Live.StreamText != wantLive {
		t.Fatalf("live stream=%q, want %q", body.Live.StreamText, wantLive)
	}
}

func TestProjectNotificationRowsUseCamelCaseAgentTimestamps(t *testing.T) {
	firstUserAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	secondUserAt := firstUserAt.Add(5 * time.Minute)
	notification := likeableNotificationStart + "Inspecting the app" + likeableNotificationEnd
	rows := projectNotificationRows(
		[]Message{
			{Role: "user", Body: "build it", CreatedAt: firstUserAt.Format(time.RFC3339Nano)},
			{Role: "user", Body: "add theme switcher", CreatedAt: secondUserAt.Format(time.RFC3339Nano)},
		},
		[]any{
			map[string]any{
				"role":      "assistant",
				"body":      notification,
				"createdAt": firstUserAt.Add(10 * time.Second).Format(time.RFC3339Nano),
			},
		},
		nil,
		&fibe.ConversationLiveState{
			ConversationID: "conv",
			IsProcessing:   true,
			StreamText:     notification,
			StartedAt:      secondUserAt.Add(time.Second).Format(time.RFC3339Nano),
		},
	)

	oldID := notificationTurnKey(firstUserAt) + "-notification-0"
	currentID := notificationTurnKey(secondUserAt) + "-notification-0"
	if row, ok := notificationRowByID(rows, oldID); !ok || row.Active {
		t.Fatalf("old row=%+v ok=%v, want inactive durable row with camelCase timestamp", row, ok)
	}
	if row, ok := notificationRowByID(rows, currentID); !ok || !row.Active {
		t.Fatalf("current row=%+v ok=%v, want active live row after repeated notification body", row, ok)
	}
}

func TestProjectNotificationRowsUntimedDurableRowsDoNotCoverCurrentLiveTurn(t *testing.T) {
	userAt := time.Date(2026, 5, 17, 12, 30, 0, 0, time.UTC)
	notification := likeableNotificationStart + "Inspecting the app" + likeableNotificationEnd
	rows := projectNotificationRows(
		[]Message{{Role: "user", Body: "add theme switcher", CreatedAt: userAt.Format(time.RFC3339Nano)}},
		nil,
		[]any{map[string]any{"id": "old-activity", "message": notification}},
		&fibe.ConversationLiveState{
			ConversationID: "conv",
			IsProcessing:   true,
			StreamText:     notification,
			StartedAt:      userAt.Add(time.Second).Format(time.RFC3339Nano),
		},
	)

	if row, ok := notificationRowByID(rows, "activity-old-activity-notification-0"); !ok || row.Active {
		t.Fatalf("activity row=%+v ok=%v, want inactive durable row", row, ok)
	}
	currentID := notificationTurnKey(userAt) + "-notification-0"
	if row, ok := notificationRowByID(rows, currentID); !ok || !row.Active {
		t.Fatalf("current row=%+v ok=%v, want active live row when only old durable row is untimed", row, ok)
	}
}

func notificationRowByID(rows []projectNotificationRow, id string) (projectNotificationRow, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return projectNotificationRow{}, false
}

func TestProjectFeedCachesWorkspaceSnapshot(t *testing.T) {
	cliPath, logPath, _ := fakeFibeCLI(t)
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-cache",
		UserID:         user.ID,
		Title:          "Feed Cache",
		ConversationID: "conv-feed-cache",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-cache/feed", nil)
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed returned %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	}

	commands := readFile(t, logPath)
	for _, command := range []string{"agents messages", "agents activity", "agents live-state"} {
		if got := strings.Count(commands, command); got != 1 {
			t.Fatalf("%s calls=%d, want 1; commands:\n%s", command, got, commands)
		}
	}
}

func TestProjectFeedBacksOffAfterPlatformRateLimit(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	logPath := filepath.Join(dir, "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"agents messages"*)
    printf '%s\n' '{"error":{"message":"unexpected status 429","code":"INTERNAL_ERROR","status":422}}' >&2
    exit 1
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-rate-limit",
		UserID:         user.ID,
		Title:          "Rate Limit",
		ConversationID: "conv-feed-rate-limit",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-rate-limit/feed", nil)
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed returned %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Warning string `json:"warning"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body.Warning, "temporarily unavailable") {
			t.Fatalf("warning=%q, want platform backoff warning", body.Warning)
		}
	}

	commands := readFile(t, logPath)
	if got := strings.Count(commands, "agents messages"); got != 1 {
		t.Fatalf("agents messages calls=%d, want 1; commands:\n%s", got, commands)
	}
}

func TestProjectFeedBacksOffAfterPlatformTimeout(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	logPath := filepath.Join(dir, "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"agents messages"*)
    printf '%s\n' '{"error":{"message":"Get \"https://next.fibe.live/api/agents/83/live_state\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)","code":"UNKNOWN_ERROR","status":500}}' >&2
    exit 1
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-timeout",
		UserID:         user.ID,
		Title:          "Timeout",
		ConversationID: "conv-feed-timeout",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-timeout/feed", nil)
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed returned %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Warning string `json:"warning"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(body.Warning, "temporarily unavailable") {
			t.Fatalf("warning=%q, want platform backoff warning", body.Warning)
		}
	}

	commands := readFile(t, logPath)
	if got := strings.Count(commands, "agents messages"); got != 1 {
		t.Fatalf("agents messages calls=%d, want 1; commands:\n%s", got, commands)
	}
}

func TestProjectFeedActivityTimeoutDoesNotBackOffLiveUpdates(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	logPath := filepath.Join(dir, "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"agents messages"*)
    echo '{"content":[{"role":"user","body":"build it"}]}'
    ;;
  *"agents activity"*)
    printf '%s\n' '{"error":{"message":"signal: killed","code":"UNKNOWN_ERROR","status":500}}' >&2
    exit 1
    ;;
  *"agents live-state"*)
    echo '{"conversationId":"conv-feed-activity-timeout","isProcessing":false,"streamText":"","queuedTurns":0}'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-feed-activity-timeout",
		UserID:         user.ID,
		Title:          "Activity Timeout",
		ConversationID: "conv-feed-activity-timeout",
		AgentID:        "agent-1",
		PlaygroundID:   "123",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/api/projects/project-feed-activity-timeout/feed", nil)
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("feed returned %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Warning  string `json:"warning"`
			Messages []any  `json:"messages"`
			Live     struct {
				ConversationID string `json:"conversationId"`
			} `json:"live"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Warning != projectActivityWarning {
			t.Fatalf("warning=%q, want %q", body.Warning, projectActivityWarning)
		}
		if len(body.Messages) != 1 {
			t.Fatalf("messages=%d, want cached messages despite activity timeout", len(body.Messages))
		}
		if body.Live.ConversationID != project.ConversationID {
			t.Fatalf("live conversation=%q, want %q", body.Live.ConversationID, project.ConversationID)
		}
		if remaining, ok := server.platformBackoffRemaining(); ok {
			t.Fatalf("platform backoff active for %s after activity-only timeout", remaining)
		}
	}

	commands := readFile(t, logPath)
	for _, command := range []string{"agents messages", "agents activity", "agents live-state"} {
		if got := strings.Count(commands, command); got != 1 {
			t.Fatalf("%s calls=%d, want 1 cached successful snapshot; commands:\n%s", command, got, commands)
		}
	}
}

func TestProjectNotificationTimingsPersistAcrossRefresh(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "likeable.db")
	appStore, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: appStore, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := appStore.UpsertUser(t.Context(), "a@example.com", "A", "")
	project := &Project{
		ID:             "project-notification-timings",
		UserID:         user.ID,
		Title:          "Notification timings",
		ConversationID: "conv-notification-timings",
		AgentID:        "agent-1",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := appStore.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := appStore.UpsertConfig(t.Context(), map[string]string{"free_hours": "0"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	if _, err := appStore.GrantHourCredits(t.Context(), user.ID, "cs_notification_hours", 1); err != nil {
		t.Fatal(err)
	}
	userAt := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	if _, err := appStore.AddMessageAt(t.Context(), project.ID, "user", "make changes", userAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	local, err := appStore.MessagesForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstLive := &fibe.ConversationLiveState{
		ConversationID: project.ConversationID,
		IsProcessing:   true,
		StreamText:     likeableNotificationStart + "Inspecting the app" + likeableNotificationEnd,
		QueuedTurns:    1,
		StartedAt:      userAt.Add(time.Second).Format(time.RFC3339Nano),
	}
	timings, shouldContinue, err := server.syncProjectNotificationTimingsAt(t.Context(), project, local, nil, nil, firstLive, userAt.Add(13*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !shouldContinue {
		t.Fatal("sync should keep monitoring while live state is processing")
	}
	firstID := notificationTurnKey(userAt) + "-notification-0"
	secondID := notificationTurnKey(userAt) + "-notification-1"
	if timings[firstID].ElapsedMs != 0 {
		t.Fatalf("first elapsed after initial observation=%d, want 0", timings[firstID].ElapsedMs)
	}

	secondLive := &fibe.ConversationLiveState{
		ConversationID: project.ConversationID,
		IsProcessing:   true,
		StreamText:     likeableNotificationStart + "Inspecting the app" + likeableNotificationEnd + likeableNotificationStart + "Updating files" + likeableNotificationEnd,
		QueuedTurns:    1,
		StartedAt:      userAt.Add(time.Second).Format(time.RFC3339Nano),
	}
	timings, _, err = server.syncProjectNotificationTimingsAt(t.Context(), project, local, nil, nil, secondLive, userAt.Add(47*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := timings[firstID].ElapsedMs; got != 46_000 {
		t.Fatalf("first elapsed=%d, want 46000", got)
	}
	if got := timings[secondID].ElapsedMs; got != 0 {
		t.Fatalf("second elapsed=%d, want 0 until another notification arrives", got)
	}

	finalLive := &fibe.ConversationLiveState{
		ConversationID: project.ConversationID,
		IsProcessing:   false,
		StreamText: likeableNotificationStart + "Inspecting the app" + likeableNotificationEnd +
			likeableNotificationStart + "Updating files" + likeableNotificationEnd +
			likeableNotificationStart + "Refreshing the canvas" + likeableNotificationEnd +
			likeableNotificationStart + "Checking the preview" + likeableNotificationEnd +
			likeableNotificationStart + "Canvas updated" + likeableNotificationEnd,
		QueuedTurns: 0,
		StartedAt:   userAt.Add(time.Second).Format(time.RFC3339Nano),
	}
	timings, shouldContinue, err = server.syncProjectNotificationTimingsAt(t.Context(), project, local, nil, nil, finalLive, userAt.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if shouldContinue {
		t.Fatal("sync should stop monitoring after live state is idle")
	}
	for index := 0; index <= 4; index++ {
		id := fmt.Sprintf("%s-notification-%d", notificationTurnKey(userAt), index)
		if timings[id].CompletedAt == "" {
			t.Fatalf("%s completed_at is empty, want every idle notification completed", id)
		}
	}
	finalID := notificationTurnKey(userAt) + "-notification-4"
	if got, want := timings[finalID].ElapsedMs, int64(599_000); got != want {
		t.Fatalf("final elapsed=%d, want total turn elapsed %d", got, want)
	}
	balance, err := appStore.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := balance, int64(time.Hour/time.Millisecond)-599_000; got != want {
		t.Fatalf("paid hour balance=%d, want one turn billed once as %dms", got, 599_000)
	}
	if err := appStore.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.ProjectNotificationTimingMap(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted[firstID].ElapsedMs; got != 46_000 {
		t.Fatalf("persisted first elapsed=%d, want 46000", got)
	}
	if got, want := persisted[finalID].ElapsedMs, int64(599_000); got != want {
		t.Fatalf("persisted final elapsed=%d, want total turn elapsed %d", got, want)
	}
}

func TestProjectNotificationTimingsUseLiveStartWhenFirstObservedIdle(t *testing.T) {
	appStore, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer appStore.Close()
	server := &Server{store: appStore, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := appStore.UpsertUser(t.Context(), "a@example.com", "A", "")
	project := &Project{
		ID:             "project-notification-timings-first-idle",
		UserID:         user.ID,
		Title:          "First idle notification timings",
		ConversationID: "conv-notification-timings-first-idle",
		AgentID:        "agent-1",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := appStore.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := appStore.UpsertConfig(t.Context(), map[string]string{"free_hours": "0"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	if _, err := appStore.GrantHourCredits(t.Context(), user.ID, "cs_first_idle_hours", 1); err != nil {
		t.Fatal(err)
	}
	userAt := time.Date(2026, 5, 12, 13, 0, 0, 0, time.UTC)
	if _, err := appStore.AddMessageAt(t.Context(), project.ID, "user", "make changes", userAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	local, err := appStore.MessagesForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}

	observedAt := userAt.Add(10 * time.Minute)
	finalLive := &fibe.ConversationLiveState{
		ConversationID: project.ConversationID,
		IsProcessing:   false,
		StreamText: likeableNotificationStart + "Inspecting the app" + likeableNotificationEnd +
			likeableNotificationStart + "Updating files" + likeableNotificationEnd +
			likeableNotificationStart + "Refreshing the canvas" + likeableNotificationEnd +
			likeableNotificationStart + "Checking the preview" + likeableNotificationEnd +
			likeableNotificationStart + "Canvas updated" + likeableNotificationEnd,
		QueuedTurns: 0,
		StartedAt:   userAt.Add(time.Second).Format(time.RFC3339Nano),
	}
	timings, shouldContinue, err := server.syncProjectNotificationTimingsAt(t.Context(), project, local, nil, nil, finalLive, observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if shouldContinue {
		t.Fatal("sync should stop monitoring after first observed idle state")
	}
	finalID := notificationTurnKey(userAt) + "-notification-4"
	if got, want := timings[finalID].ElapsedMs, int64(599_000); got != want {
		t.Fatalf("final elapsed=%d, want total source elapsed %d", got, want)
	}
	if got, want := timings[finalID].CompletedAt, observedAt.Format(time.RFC3339Nano); got != want {
		t.Fatalf("final completed_at=%s, want observed completion %s", got, want)
	}
	balance, err := appStore.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := balance, int64(time.Hour/time.Millisecond)-599_000; got != want {
		t.Fatalf("paid hour balance=%d, want first idle turn billed as %dms", got, 599_000)
	}
}

func TestProjectNotificationTimingsBillDurableAssistantFromSourceTimes(t *testing.T) {
	appStore, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer appStore.Close()
	server := &Server{store: appStore, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := appStore.UpsertUser(t.Context(), "a@example.com", "A", "")
	project := &Project{
		ID:             "project-notification-timings-durable",
		UserID:         user.ID,
		Title:          "Durable notification timings",
		ConversationID: "conv-notification-timings-durable",
		AgentID:        "agent-1",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := appStore.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := appStore.UpsertConfig(t.Context(), map[string]string{"free_hours": "0"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	if _, err := appStore.GrantHourCredits(t.Context(), user.ID, "cs_durable_hours", 1); err != nil {
		t.Fatal(err)
	}
	userAt := time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC)
	if _, err := appStore.AddMessageAt(t.Context(), project.ID, "user", "make changes", userAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	local, err := appStore.MessagesForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := userAt.Add(75 * time.Second)
	messages := []any{map[string]any{
		"role":       "assistant",
		"body":       likeableNotificationStart + "Updating files" + likeableNotificationEnd + likeableNotificationStart + "Canvas updated" + likeableNotificationEnd,
		"created_at": completedAt.Format(time.RFC3339Nano),
	}}

	timings, _, err := server.syncProjectNotificationTimingsAt(t.Context(), project, local, messages, nil, nil, userAt.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	finalID := notificationTurnKey(userAt) + "-notification-1"
	if got, want := timings[finalID].ElapsedMs, int64(75_000); got != want {
		t.Fatalf("final elapsed=%d, want assistant source elapsed %d", got, want)
	}
	if got, want := timings[finalID].CompletedAt, completedAt.Format(time.RFC3339Nano); got != want {
		t.Fatalf("final completed_at=%s, want assistant completion %s", got, want)
	}
	balance, err := appStore.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := balance, int64(time.Hour/time.Millisecond)-75_000; got != want {
		t.Fatalf("paid hour balance=%d, want durable assistant turn billed as %dms", got, 75_000)
	}
}

func TestProjectNotificationTimingsCompleteFinishedLastRowWhileQueued(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "likeable.db")
	appStore, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer appStore.Close()

	server := &Server{store: appStore, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := appStore.UpsertUser(t.Context(), "a@example.com", "A", "")
	project := &Project{
		ID:             "project-notification-timings-queued",
		UserID:         user.ID,
		Title:          "Notification timings queued",
		ConversationID: "conv-notification-timings-queued",
		AgentID:        "agent-1",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := appStore.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	userAt := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	if _, err := appStore.AddMessageAt(t.Context(), project.ID, "user", "make changes", userAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	local, err := appStore.MessagesForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}

	firstLive := &fibe.ConversationLiveState{
		ConversationID: project.ConversationID,
		IsProcessing:   true,
		StreamText:     likeableNotificationStart + "Inspecting the app" + likeableNotificationEnd,
		QueuedTurns:    1,
		StartedAt:      userAt.Add(time.Second).Format(time.RFC3339Nano),
	}
	if _, _, err := server.syncProjectNotificationTimingsAt(t.Context(), project, local, nil, nil, firstLive, userAt.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}

	finalLive := &fibe.ConversationLiveState{
		ConversationID: project.ConversationID,
		IsProcessing:   false,
		StreamText: likeableNotificationStart + "Inspecting the app" + likeableNotificationEnd +
			likeableNotificationStart + "Updating files" + likeableNotificationEnd +
			likeableNotificationStart + "Refreshing the canvas" + likeableNotificationEnd +
			likeableNotificationStart + "Checking the preview" + likeableNotificationEnd +
			likeableNotificationStart + "Canvas updated" + likeableNotificationEnd,
		QueuedTurns: 1,
		StartedAt:   userAt.Add(time.Second).Format(time.RFC3339Nano),
	}
	timings, shouldContinue, err := server.syncProjectNotificationTimingsAt(t.Context(), project, local, nil, nil, finalLive, userAt.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !shouldContinue {
		t.Fatal("sync should keep monitoring while another turn is queued")
	}
	finalID := notificationTurnKey(userAt) + "-notification-4"
	if timings[finalID].CompletedAt == "" {
		t.Fatalf("%s completed_at is empty, want finished Canvas updated row completed despite queued turn", finalID)
	}
	if got, want := timings[finalID].ElapsedMs, int64(119_000); got != want {
		t.Fatalf("final elapsed=%d, want total turn elapsed %d", got, want)
	}
}

func TestObservedLiveWorkWithoutCanvasNotificationIsBilled(t *testing.T) {
	appStore, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer appStore.Close()
	server := &Server{store: appStore, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := appStore.UpsertUser(t.Context(), "a@example.com", "A", "")
	project := &Project{
		ID:             "project-live-work-billing",
		UserID:         user.ID,
		Title:          "Live work billing",
		ConversationID: "conv-live-work-billing",
		AgentID:        "agent-1",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := appStore.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := appStore.UpsertConfig(t.Context(), map[string]string{"free_hours": "0"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	if _, err := appStore.GrantHourCredits(t.Context(), user.ID, "cs_live_hours", 1); err != nil {
		t.Fatal(err)
	}
	userAt := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if _, err := appStore.AddMessageAt(t.Context(), project.ID, "user", "make changes", userAt.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	local, err := appStore.MessagesForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeLive := &fibe.ConversationLiveState{
		ConversationID: project.ConversationID,
		IsProcessing:   true,
		StreamText:     "",
		QueuedTurns:    0,
		StartedAt:      userAt.Add(time.Second).Format(time.RFC3339Nano),
	}
	if _, _, err := server.syncProjectNotificationTimingsAt(t.Context(), project, local, nil, nil, activeLive, userAt.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	idleLive := &fibe.ConversationLiveState{
		ConversationID: project.ConversationID,
		IsProcessing:   false,
		StreamText:     "",
		QueuedTurns:    0,
		StartedAt:      userAt.Add(time.Second).Format(time.RFC3339Nano),
	}
	if _, _, err := server.syncProjectNotificationTimingsAt(t.Context(), project, local, nil, nil, idleLive, userAt.Add(70*time.Second)); err != nil {
		t.Fatal(err)
	}
	balance, err := appStore.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := balance, int64(time.Hour/time.Millisecond)-69_000; got != want {
		t.Fatalf("paid hour balance=%d, want one observed live session billed as 69000ms", got)
	}
}

func TestProjectFeedRefreshesTransformedServiceLayout(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, _ := fakeTransformedFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:              "project-transform-feed",
		UserID:          user.ID,
		Title:           "Transform",
		ConversationID:  "conv-trns",
		AgentID:         "agent-1",
		PlaygroundID:    "321",
		PlayspecID:      "123",
		PropID:          "old-prop",
		RepoURL:         "http://gitea.test/owner/old.git",
		PreviewURL:      "http://old-app.example.test",
		SelectedService: "app",
		Status:          "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceProjectResources(t.Context(), project.ID, []ProjectRepository{
		{ProjectID: project.ID, Role: "app", PropID: "old-prop", RepoURL: "http://gitea.test/owner/old.git", ServiceNames: []string{"app"}},
	}, []ProjectService{
		{ProjectID: project.ID, Name: "app", URL: "http://old-app.example.test", Type: "dynamic", Visibility: "external"},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-transform-feed/feed", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Project Project `json:"project"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Project.SelectedService != "frontend" || body.Project.PreviewURL != "http://frontend.example.test" {
		t.Fatalf("project selected=%q preview=%q, want refreshed frontend", body.Project.SelectedService, body.Project.PreviewURL)
	}
	if len(body.Project.Services) != 2 || body.Project.Services[0].Name != "frontend" || body.Project.Services[1].Name != "api" {
		t.Fatalf("services=%+v, want frontend/api", body.Project.Services)
	}
	repositoryRoles := make([]string, 0, len(body.Project.Repositories))
	for _, repository := range body.Project.Repositories {
		repositoryRoles = append(repositoryRoles, repository.Role)
	}
	if len(body.Project.Repositories) != 2 || !stringSliceContains(repositoryRoles, "frontend") || !stringSliceContains(repositoryRoles, "api") {
		t.Fatalf("repositories=%+v, want refreshed frontend/api public metadata", body.Project.Repositories)
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SelectedService != "frontend" || stored.PreviewURL != "http://frontend.example.test" {
		t.Fatalf("stored selected=%q preview=%q, want refreshed frontend", stored.SelectedService, stored.PreviewURL)
	}
	if stored.RepoURL != "http://gitea.test/owner/frontend.git" || len(stored.Repositories) != 2 {
		t.Fatalf("stored repoURL=%q repositories=%+v, want refreshed frontend/api repos", stored.RepoURL, stored.Repositories)
	}
}

func TestProjectsListRefreshesStoppedPlaygroundStatus(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	previewServer := httptest.NewServer(http.NotFoundHandler())
	defer previewServer.Close()
	cliPath := fakeProjectStateFibeCLI(t, "stopped", previewServer.URL)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:              "project-stopped-list",
		UserID:          user.ID,
		Title:           "Stopped",
		ConversationID:  "conv-stopped",
		AgentID:         "agent-1",
		PlaygroundID:    "321",
		PlayspecID:      "654",
		PreviewURL:      previewServer.URL,
		SelectedService: "app",
		Status:          "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("projects returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Projects []Project `json:"projects"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Projects) != 1 || body.Projects[0].Status != "stopped" {
		t.Fatalf("projects=%+v, want stopped status", body.Projects)
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stopped" {
		t.Fatalf("stored status=%q, want stopped", stored.Status)
	}
}

func TestPreviewStatusReturnsRefreshedProjectStatus(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	previewServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><title>stale but reachable</title>"))
	}))
	defer previewServer.Close()
	cliPath := fakeProjectStateFibeCLI(t, "stopped", previewServer.URL)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: previewServer.Client()}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:              "project-stopped-preview",
		UserID:          user.ID,
		Title:           "Stopped",
		ConversationID:  "conv-stopped",
		AgentID:         "agent-1",
		PlaygroundID:    "321",
		PlayspecID:      "654",
		PreviewURL:      previewServer.URL,
		SelectedService: "app",
		Status:          "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-stopped-preview/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready   bool    `json:"ready"`
		Status  string  `json:"status"`
		Project Project `json:"project"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Ready || body.Status != "stopped" {
		t.Fatalf("ready=%v status=%q, want stopped preview state", body.Ready, body.Status)
	}
	if body.Project.Status != "stopped" {
		t.Fatalf("project status=%q, want stopped", body.Project.Status)
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stopped" {
		t.Fatalf("stored status=%q, want stopped", stored.Status)
	}
}

func TestPreviewStatusMarksLaunchingProjectStoppedWhenPlatformStopped(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath := fakeProjectStateFibeCLI(t, "stopped", "http://127.0.0.1:1")
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:              "project-launching-stopped-preview",
		UserID:          user.ID,
		Title:           "Stopped",
		ConversationID:  "conv-stopped",
		AgentID:         "agent-1",
		PlaygroundID:    "321",
		PlayspecID:      "654",
		PreviewURL:      "http://127.0.0.1:1",
		SelectedService: "app",
		Status:          "launching",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/project-launching-stopped-preview/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview-status returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready   bool    `json:"ready"`
		Status  string  `json:"status"`
		Project Project `json:"project"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Ready || body.Status != "stopped" {
		t.Fatalf("ready=%v status=%q, want stopped preview state", body.Ready, body.Status)
	}
	if body.Project.Status != "stopped" {
		t.Fatalf("project status=%q, want stopped", body.Project.Status)
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stopped" {
		t.Fatalf("stored status=%q, want stopped", stored.Status)
	}
}

func TestProjectMessageRefreshesServiceContextBeforeSending(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, stdinPath := fakeTransformedFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, _ := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err := store.CreateSession(t.Context(), user.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:              "project-transform-message",
		UserID:          user.ID,
		Title:           "Transform",
		ConversationID:  "conv-trns",
		AgentID:         "agent-1",
		PlaygroundID:    "321",
		PlayspecID:      "123",
		PropID:          "old-prop",
		RepoURL:         "http://gitea.test/owner/old.git",
		PreviewURL:      "http://old-app.example.test",
		SelectedService: "app",
		Status:          "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceProjectResources(t.Context(), project.ID, []ProjectRepository{
		{ProjectID: project.ID, Role: "app", PropID: "old-prop", RepoURL: "http://gitea.test/owner/old.git", ServiceNames: []string{"app"}},
	}, []ProjectService{
		{ProjectID: project.ID, Name: "app", URL: "http://old-app.example.test", Type: "dynamic", Visibility: "external"},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-transform-message/messages", strings.NewReader(`{"text":"change the admin background"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("message returned %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(readFile(t, stdinPath)), &payload); err != nil {
		t.Fatal(err)
	}
	prompt := payload.Text
	for _, want := range []string{
		"selected service: frontend http://frontend.example.test",
		"- frontend: http://frontend.example.test",
		"- api: http://api.example.test",
		"- frontend [frontend]: http://gitea.test/owner/frontend.git",
		"change the admin background",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestProfileDeleteAllDeletesFibeResourcesAndLocalData(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, logPath, _ := fakeFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url":         "server.test:3000",
		"fibe_api_key":          "test-key",
		"fibe_cli_path":         cliPath,
		"signup_allowed_emails": "pilot@example.com\n@trusted.test",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "delete-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	repoDeleted := false
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/repos/owner/repo" {
			t.Fatalf("unexpected Gitea request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token gitea-token" {
			t.Fatalf("Authorization=%q, want gitea token", got)
		}
		repoDeleted = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gitea.Close()
	repoURL := gitea.URL + "/owner/repo.git"
	project := &Project{
		ID:             "project-delete-all",
		UserID:         user.ID,
		Title:          "Delete all",
		ConversationID: "conv-delete-all",
		AgentID:        "agent-1",
		PlaygroundID:   "playground-1",
		PlayspecID:     "playspec-1",
		PropID:         "prop-1",
		RepoURL:        repoURL,
		Status:         "ready",
		Repositories: []ProjectRepository{
			{ProjectID: "project-delete-all", Role: "app", PropID: "prop-1", RepoURL: repoURL, ServiceNames: []string{"app"}},
		},
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceProjectResources(t.Context(), project.ID, project.Repositories, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessage(t.Context(), project.ID, "user", "hello"); err != nil {
		t.Fatal(err)
	}
	attachmentDir := filepath.Join(store.DataDir(), "attachments", project.ID, "message-1")
	if err := os.MkdirAll(attachmentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attachmentDir, "attachment.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/profile/delete-all", strings.NewReader(`{"email":"PILOT@example.com"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "delete-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete-all returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !responseClearsCookie(rec.Result(), "likeable_session") {
		t.Fatal("delete-all response did not clear likeable_session")
	}
	if _, err := store.UserBySessionToken(t.Context(), "delete-token"); err == nil {
		t.Fatal("session still resolves immediately after delete-all")
	}
	eventually(t, 3*time.Second, func() bool {
		_, err := store.UserByID(t.Context(), user.ID)
		return err != nil
	}, func() string {
		return "user still exists after delete-all cleanup"
	})
	log := readFile(t, logPath)
	for _, want := range []string{
		"playgrounds delete playground-1",
		"playspecs delete playspec-1",
		"templates versions destroy 321 654",
		"templates delete 321",
		"props delete prop-1",
		"agents delete-conversation agent-1 --conversation-id conv-delete-all",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing CLI command %q; log=%s", want, log)
		}
	}
	if !repoDeleted {
		t.Fatal("Gitea repository was not deleted")
	}
	if _, err := os.Stat(filepath.Join(store.DataDir(), "attachments", project.ID)); !os.IsNotExist(err) {
		t.Fatalf("attachment directory still exists or stat failed unexpectedly: %v", err)
	}
	projectRows, err := store.ProjectCountForUser(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projectRows != 0 {
		t.Fatalf("project rows=%d, want 0", projectRows)
	}
	cfg, err := store.ConfigMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg["signup_allowed_emails"], "pilot@example.com") || !strings.Contains(cfg["signup_allowed_emails"], "@trusted.test") {
		t.Fatalf("allowlist=%q, want deleted email removed and other entries preserved", cfg["signup_allowed_emails"])
	}
}

func TestProfileDeleteAllUsesStoredResourcesWithoutDebugHydration(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	logPath := filepath.Join(dir, "commands.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"playgrounds debug"*)
    echo '{"error":{"message":"debug timeout","code":"UNKNOWN_ERROR","status":500}}' >&2
    exit 1
    ;;
  *"playspecs get"*)
    echo '{"id":"playspec-1","source_template":{"id":321,"name":"delete-all-abc12345"},"source_template_version_id":654}'
    ;;
  *"templates versions list"*)
    echo '{"Data":[{"id":654,"source":{"prop_id":"prop-1"}}]}'
    ;;
  *"playgrounds delete"*|*"playspecs delete"*|*"templates versions destroy"*|*"templates delete"*|*"props delete"*|*"agents delete-conversation"*)
    echo '{"ok":true}'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "delete-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-delete-all",
		UserID:         user.ID,
		Title:          "Delete all",
		ConversationID: "conv-delete-all",
		AgentID:        "agent-1",
		PlaygroundID:   "playground-1",
		PlayspecID:     "playspec-1",
		PropID:         "prop-1",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/profile/delete-all", strings.NewReader(`{"email":"pilot@example.com"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "delete-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete-all returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !responseClearsCookie(rec.Result(), "likeable_session") {
		t.Fatal("delete-all response did not clear likeable_session")
	}
	eventually(t, 3*time.Second, func() bool {
		_, err := store.UserByID(t.Context(), user.ID)
		return err != nil
	}, func() string {
		return "user still exists after delete-all cleanup"
	})
	log := readFile(t, logPath)
	for _, want := range []string{
		"playgrounds delete playground-1",
		"playspecs delete playspec-1",
		"props delete prop-1",
		"agents delete-conversation agent-1 --conversation-id conv-delete-all",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing CLI command %q; log=%s", want, log)
		}
	}
	if strings.Contains(log, "playgrounds debug playground-1") {
		t.Fatalf("debug hydration should be skipped when stored deletion snapshot is complete; log=%s", log)
	}
}

func TestProfileDeleteAllKeepsLocalDataWhenFibeCleanupFails(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": "/does/not/exist",
		"signup_mode":   "all",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "delete-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-delete-all", UserID: user.ID, Title: "Delete all", ConversationID: "conv-delete-all", AgentID: "agent-1", PlaygroundID: "playground-1", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/profile/delete-all", strings.NewReader(`{"email":"pilot@example.com"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "delete-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete-all returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !responseClearsCookie(rec.Result(), "likeable_session") {
		t.Fatal("delete-all response did not clear likeable_session")
	}
	if _, err := store.UserBySessionToken(t.Context(), "delete-token"); err == nil {
		t.Fatal("session still resolves immediately after delete-all")
	}
	eventually(t, time.Second, func() bool {
		stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
		return err == nil && stored.CleanupLastError != ""
	}, func() string {
		stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
		if err != nil {
			return "project missing before failed cleanup could be recorded"
		}
		return "cleanup_last_error=" + stored.CleanupLastError
	})
	storedUser, err := store.UserByID(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("user should remain restricted when remote cleanup fails: %v", err)
	}
	if storedUser.AccessStatus != "restricted" {
		t.Fatalf("access_status=%q, want restricted", storedUser.AccessStatus)
	}
	if storedUser.Email == "pilot@example.com" || !strings.HasPrefix(storedUser.Email, "deleted-") {
		t.Fatalf("deleted user email=%q, want retired tombstone address", storedUser.Email)
	}
	if _, err := store.UserByEmail(t.Context(), "pilot@example.com"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted email lookup error=%v, want sql.ErrNoRows", err)
	}
	allowed, err := server.canSignInEmail(t.Context(), "pilot@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("deleted email should be allowed to sign up again while cleanup is stuck")
	}
	newUser, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot Reloaded", "")
	if err != nil {
		t.Fatal(err)
	}
	if newUser.ID == user.ID {
		t.Fatal("deleted email reused old user id")
	}
	if newUser.AccessStatus != "active" {
		t.Fatalf("new user access_status=%q, want active", newUser.AccessStatus)
	}
	if project, err := store.ProjectForUser(t.Context(), user.ID, project.ID); err != nil {
		t.Fatalf("project should remain when remote cleanup fails: %v", err)
	} else if project.Status != "deleting" {
		t.Fatalf("project status=%q, want deleting", project.Status)
	}
}

func TestProjectDeleteHydrationFailureLeavesDeletingProject(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "fibe")
	script := `#!/bin/sh
case "$*" in
  *"playgrounds debug"*)
    echo "debug failed" >&2
    exit 64
    ;;
  *)
    echo '{"ok":true}'
    ;;
esac
`
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-delete-hydration-fails",
		UserID:         user.ID,
		Title:          "Hydration fails",
		ConversationID: "conv-delete-hydration-fails",
		PlaygroundID:   "playground-bad",
		Status:         "deleting",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	server.deleteProjectResourcesAsync(user.ID, user.Email, project)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.CleanupLastError != "" {
			if stored.Status != "deleting" {
				t.Fatalf("status=%q, want deleting", stored.Status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("cleanup_last_error=%q, want hydration error recorded", stored.CleanupLastError)
}

func TestProjectPlaygroundLifecycleActions(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, logPath, _ := fakeFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "project-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	oldUsage := time.Now().UTC().Add(-9 * time.Hour).Format(time.RFC3339Nano)
	project := &Project{ID: "project-control", UserID: user.ID, Title: "Control", ConversationID: "conv-control", AgentID: "agent-1", PlaygroundID: "playground-1", Status: "ready", PlaygroundLastUsedAt: oldUsage}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	lastUsedAt := oldUsage
	for _, tc := range []struct {
		action string
		status string
		cmd    string
	}{
		{action: "stop", status: "stopped", cmd: "playgrounds stop playground-1"},
		{action: "start", status: "launching", cmd: "playgrounds start playground-1"},
		{action: "restart", status: "launching", cmd: "playgrounds hard-restart playground-1"},
	} {
		time.Sleep(time.Millisecond)
		req := httptest.NewRequest(http.MethodPost, "/api/projects/project-control/playground", strings.NewReader(`{"action":"`+tc.action+`"}`))
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "project-token"})
		rec := httptest.NewRecorder()

		server.routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s returned %d, want 202; body=%s", tc.action, rec.Code, rec.Body.String())
		}
		stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != tc.status {
			t.Fatalf("%s status=%q, want %q", tc.action, stored.Status, tc.status)
		}
		if stored.PlaygroundLastUsedAt == "" || stored.PlaygroundLastUsedAt == lastUsedAt {
			t.Fatalf("%s playground_last_used_at=%q, previous=%q; want touch", tc.action, stored.PlaygroundLastUsedAt, lastUsedAt)
		}
		lastUsedAt = stored.PlaygroundLastUsedAt
		if log := readFile(t, logPath); !strings.Contains(log, tc.cmd) {
			t.Fatalf("%s missing command %q; log=%s", tc.action, tc.cmd, log)
		}
	}
}

func TestProjectPassiveActionsDoNotTouchPlaygroundUsage(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, _, _ := fakeFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "activity@example.com", "Activity", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "activity-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	oldUsage := time.Now().UTC().Add(-9 * time.Hour).Format(time.RFC3339Nano)
	switchProject := &Project{ID: "project-service-activity", UserID: user.ID, Title: "Service", ConversationID: "conv-service-activity", AgentID: "agent-1", PlaygroundID: "playground-service-activity", PreviewURL: "http://app.example.test", Status: "ready", PlaygroundLastUsedAt: oldUsage}
	if err := store.CreateProject(t.Context(), switchProject); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceProjectResources(t.Context(), switchProject.ID, nil, []ProjectService{
		{ProjectID: switchProject.ID, Name: "app", URL: "http://app.example.test", Type: "dynamic", Visibility: "external"},
		{ProjectID: switchProject.ID, Name: "api", URL: "http://api.example.test", Type: "dynamic", Visibility: "external"},
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/projects/project-service-activity", strings.NewReader(`{"selectedServiceName":"api"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "activity-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("service switch returned %d: %s", rec.Code, rec.Body.String())
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, switchProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PlaygroundLastUsedAt != oldUsage {
		t.Fatalf("service switch changed playground_last_used_at from %q to %q", oldUsage, updated.PlaygroundLastUsedAt)
	}

	interruptProject := &Project{ID: "project-interrupt-activity", UserID: user.ID, Title: "Interrupt", ConversationID: "conv-interrupt-activity", AgentID: "agent-1", PlaygroundID: "playground-interrupt-activity", Status: "ready", PlaygroundLastUsedAt: oldUsage}
	if err := store.CreateProject(t.Context(), interruptProject); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/projects/project-interrupt-activity/agent/interrupt", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "activity-token"})
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("interrupt returned %d: %s", rec.Code, rec.Body.String())
	}
	updated, err = store.ProjectForUser(t.Context(), user.ID, interruptProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PlaygroundLastUsedAt != oldUsage {
		t.Fatalf("interrupt changed playground_last_used_at from %q to %q", oldUsage, updated.PlaygroundLastUsedAt)
	}

	passiveProject := &Project{ID: "project-passive-preview", UserID: user.ID, Title: "Passive", ConversationID: "conv-passive-preview", AgentID: "agent-1", PlaygroundID: "playground-passive-preview", Status: "ready", PlaygroundLastUsedAt: oldUsage}
	if err := store.CreateProject(t.Context(), passiveProject); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/projects/project-passive-preview/preview-status", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "activity-token"})
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status returned %d: %s", rec.Code, rec.Body.String())
	}
	updated, err = store.ProjectForUser(t.Context(), user.ID, passiveProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PlaygroundLastUsedAt != oldUsage {
		t.Fatalf("passive preview changed playground_last_used_at from %q to %q", oldUsage, updated.PlaygroundLastUsedAt)
	}
}

func TestProjectPlaygroundBlocksRetiredPoolPair(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url":          "server.test:3000",
		"fibe_api_key":           "test-key",
		"fibe_agent_server_pool": `[{"agent_id":"old-agent","server_id":"old-marquee","status":"retired"}]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "retired-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-retired-control", UserID: user.ID, Title: "Control", ConversationID: "conv-retired-control", AgentID: "old-agent", MarqueeID: "old-marquee", PlaygroundID: "playground-1", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-retired-control/playground", strings.NewReader(`{"action":"stop"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "retired-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("stop returned %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "archived" {
		t.Fatalf("status=%q, want archived", updated.Status)
	}
}

func TestProjectPlaygroundStopTreatsAlreadyStoppedAsSuccess(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, logPath := fakeAlreadyStoppedFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "project-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-already-stopped-control", UserID: user.ID, Title: "Control", ConversationID: "conv-already-stopped-control", AgentID: "agent-1", PlaygroundID: "playground-1", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-already-stopped-control/playground", strings.NewReader(`{"action":"stop"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "project-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("stop returned %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stopped" {
		t.Fatalf("status=%q, want stopped", stored.Status)
	}
	if log := readFile(t, logPath); !strings.Contains(log, "playgrounds stop playground-1") {
		t.Fatalf("missing stop command; log=%s", log)
	}
}

func TestIdleProjectStopTaskStopsPlayground(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, logPath, _ := fakeFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "idle@example.com", "Idle", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-idle", UserID: user.ID, Title: "Idle", ConversationID: "conv-idle", AgentID: "agent-1", PlaygroundID: "playground-idle", Status: "ready", PlaygroundLastUsedAt: time.Now().UTC().Add(-idleProjectStopAfter - time.Minute).Format(time.RFC3339Nano)}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessageAt(t.Context(), project.ID, "user", "old", time.Now().UTC().Add(-idleProjectStopAfter-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(projectJobPayload{UserID: user.ID, UserEmail: user.Email, ProjectID: project.ID})

	if err := server.handleStopIdleProjectTask(t.Context(), asynq.NewTask(taskStopIdleProject, payload)); err != nil {
		t.Fatal(err)
	}

	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stopped" {
		t.Fatalf("status=%q, want stopped", stored.Status)
	}
	if log := readFile(t, logPath); !strings.Contains(log, "playgrounds stop playground-idle") {
		t.Fatalf("missing stop command; log=%s", log)
	}
}

func TestIdleProjectStopTaskSkipsAfterRecentUsageReset(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, logPath, _ := fakeFibeCLI(t)
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "idle-reset@example.com", "Idle", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-idle-reset", UserID: user.ID, Title: "Idle", ConversationID: "conv-idle-reset", AgentID: "agent-1", PlaygroundID: "playground-idle-reset", Status: "ready", PlaygroundLastUsedAt: time.Now().UTC().Add(-idleProjectStopAfter - time.Minute).Format(time.RFC3339Nano)}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(projectJobPayload{UserID: user.ID, UserEmail: user.Email, ProjectID: project.ID})
	if err := store.TouchProjectPlaygroundUsage(t.Context(), project.ID, user.ID); err != nil {
		t.Fatal(err)
	}

	if err := server.handleStopIdleProjectTask(t.Context(), asynq.NewTask(taskStopIdleProject, payload)); err != nil {
		t.Fatal(err)
	}

	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "ready" {
		t.Fatalf("status=%q, want ready", stored.Status)
	}
	if log := readFile(t, logPath); strings.Contains(log, "playgrounds stop playground-idle-reset") {
		t.Fatalf("unexpected stop command after recent usage reset; log=%s", log)
	}
}

func TestIdleProjectStopTaskTreatsAlreadyStoppedAsSuccess(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, logPath := fakeAlreadyStoppedFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "idle@example.com", "Idle", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-idle-already-stopped", UserID: user.ID, Title: "Idle", ConversationID: "conv-idle-already-stopped", AgentID: "agent-1", PlaygroundID: "playground-idle", Status: "ready", PlaygroundLastUsedAt: time.Now().UTC().Add(-idleProjectStopAfter - time.Minute).Format(time.RFC3339Nano)}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessageAt(t.Context(), project.ID, "user", "old", time.Now().UTC().Add(-idleProjectStopAfter-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(projectJobPayload{UserID: user.ID, UserEmail: user.Email, ProjectID: project.ID})

	if err := server.handleStopIdleProjectTask(t.Context(), asynq.NewTask(taskStopIdleProject, payload)); err != nil {
		t.Fatal(err)
	}

	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stopped" {
		t.Fatalf("status=%q, want stopped", stored.Status)
	}
	if log := readFile(t, logPath); !strings.Contains(log, "playgrounds stop playground-idle") {
		t.Fatalf("missing stop command; log=%s", log)
	}
}

func TestIdleProjectStopTaskTreatsMissingPlaygroundAsStopped(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, logPath := fakeMissingPlaygroundFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url": "server.test:3000",
		"fibe_api_key":  "test-key",
		"fibe_cli_path": cliPath,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "idle-missing@example.com", "Idle", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-idle-missing", UserID: user.ID, Title: "Idle", ConversationID: "conv-idle-missing", AgentID: "agent-1", PlaygroundID: "playground-missing", Status: "ready", PlaygroundLastUsedAt: time.Now().UTC().Add(-idleProjectStopAfter - time.Minute).Format(time.RFC3339Nano)}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessageAt(t.Context(), project.ID, "user", "old", time.Now().UTC().Add(-idleProjectStopAfter-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(projectJobPayload{UserID: user.ID, UserEmail: user.Email, ProjectID: project.ID})

	if err := server.handleStopIdleProjectTask(t.Context(), asynq.NewTask(taskStopIdleProject, payload)); err != nil {
		t.Fatal(err)
	}

	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "stopped" {
		t.Fatalf("status=%q, want stopped", stored.Status)
	}
	if log := readFile(t, logPath); !strings.Contains(log, "playgrounds stop playground-missing") {
		t.Fatalf("missing stop command; log=%s", log)
	}
}

func TestAgentProjectPromptIncludesTargetContext(t *testing.T) {
	project := &Project{
		ID:              "project-1",
		Title:           "Starter",
		ConversationID:  "likeable-project-1",
		PlaygroundID:    "10",
		PlaygroundName:  "starter-10",
		RepoURL:         "http://gitea.test/owner/repo",
		PreviewURL:      "http://starter.test",
		SelectedService: "admin",
		Services: []ProjectService{
			{Name: "app", URL: "http://starter.test", Type: "dynamic", Visibility: "external"},
			{Name: "admin", URL: "http://starter-admin.test", Type: "dynamic", Visibility: "external"},
		},
		Repositories: []ProjectRepository{
			{Role: "backend", RepoURL: "http://gitea.test/owner/backend", ServiceNames: []string{"api", "worker"}},
			{Role: "app", RepoURL: "http://gitea.test/owner/app", ServiceNames: []string{"app"}},
			{Role: "admin", RepoURL: "http://gitea.test/owner/admin", ServiceNames: []string{"admin"}},
		},
	}
	prompt := projecttext.AgentPrompt(project, "Change the heading")
	for _, want := range []string{
		"target Fibe playground_id: 10",
		"target Fibe playground_name: starter-10",
		"target private source repo: http://gitea.test/owner/repo",
		"target preview_url: http://starter.test",
		"target app subdomain: lk-a33e35d302125bbd",
		"selected service: admin http://starter-admin.test",
		"- app: http://starter.test",
		"- admin: http://starter-admin.test",
		"- backend [api,worker]: http://gitea.test/owner/backend",
		"- app [app]: http://gitea.test/owner/app",
		"- admin [admin]: http://gitea.test/owner/admin",
		"Preserve the current product/domain and working behavior unless the user explicitly asks to replace it.",
		"Run the available build/test/start command after code changes.",
		"[[LIKEABLE_USER_CONTEXT_START]]",
		"User request:\nChange the heading",
		"[[LIKEABLE_USER_CONTEXT_END]]",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestPromptImproveUsesAssignedAgent(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cliPath, logPath, stdinPath := fakePromptImproveFibeCLI(t)
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url":          "server.test:3000",
		"fibe_api_key":           "key",
		"fibe_cli_path":          cliPath,
		"fibe_agent_server_pool": `[{"agent_id":"agent-1","server_id":"server-1","status":"active"}]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "prompt-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-prompt-improve", UserID: user.ID, Title: "car sharing webapp", ConversationID: "conv-prompt", AgentID: "agent-1", MarqueeID: "server-1", Status: "ready", PreviewURL: "http://preview.test"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	req := httptest.NewRequest(http.MethodPost, "/api/projects/project-prompt-improve/prompt-improve", strings.NewReader(`{"text":"add some cars and enhance ux","locale":"uk"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "prompt-token"})
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("prompt improve returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Text   string `json:"text"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Source != "agent" || !strings.Contains(body.Text, "Improve the existing car sharing webapp") {
		t.Fatalf("body=%+v, want agent-improved prompt", body)
	}
	commands := readFile(t, logPath)
	for _, want := range []string{
		"agents create-conversation agent-1 --conversation-id likeable-prompt-improve-",
		"agents send-message agent-1 --conversation-id likeable-prompt-improve-",
		"agents messages agent-1 --conversation-id likeable-prompt-improve-",
		"agents delete-conversation agent-1 --conversation-id likeable-prompt-improve-",
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("commands missing %q:\n%s", want, commands)
		}
	}
	payload := readFile(t, stdinPath)
	for _, want := range []string{
		"You are Likeable's prompt-improvement agent.",
		"Do not edit files, do not run tools, do not build",
		"Preferred UI language: Ukrainian",
		"Current app title: car sharing webapp",
		`User draft:\nadd some cars and enhance ux`,
	} {
		if !strings.Contains(payload, want) {
			t.Fatalf("prompt payload missing %q:\n%s", want, payload)
		}
	}
}

func TestFallbackImprovedPromptKeepsCyrillicLanguage(t *testing.T) {
	ru := fallbackImprovedPrompt("добавь машины и улучши ux", "car sharing webapp")
	if !strings.Contains(ru, "Запрошенное изменение") || !strings.Contains(ru, "car sharing webapp") {
		t.Fatalf("ru fallback=%q, want Russian prompt with app context", ru)
	}
	uk := fallbackImprovedPrompt("add billing polish", "Likeable", "uk")
	if !strings.Contains(uk, "Запитана зміна") || !strings.Contains(uk, "Likeable") {
		t.Fatalf("uk fallback=%q, want Ukrainian prompt with app context", uk)
	}
	if got := promptImprovePreferredLanguage("uk-UA"); got != "Ukrainian" {
		t.Fatalf("preferred language=%q, want Ukrainian", got)
	}
}

func fakePromptImproveFibeCLI(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fibe")
	logPath := filepath.Join(dir, "commands.log")
	stdinPath := filepath.Join(dir, "stdin.json")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *"agents send-message"*)
    cat > "` + stdinPath + `"
    echo '{"ok":true}'
    ;;
  *"agents messages"*)
    echo '{"content":[{"role":"assistant","body":"` + promptImproveStart + `\\nImprove the existing car sharing webapp by adding a clear vehicle inventory section, more polished ride/request UX, responsive spacing, empty/loading states, and a quick visual verification pass. Keep the current car sharing product intact and do not replace it with another app.\\n` + promptImproveEnd + `"}]}'
    ;;
  *"agents create-conversation"*|*"agents delete-conversation"*)
    echo '{"ok":true}'
    ;;
  *)
    echo "unexpected command: $*" >&2
    exit 64
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path, logPath, stdinPath
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestSignupPolicyDefaultsClosedButAllowsAdminExistingAndAllowlist(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"}, http: http.DefaultClient}

	allowed, err := server.canSignInEmail(t.Context(), "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("new non-admin user should be rejected when signup mode is unset")
	}

	allowed, err = server.canSignInEmail(t.Context(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("admin should be allowed even when signup is closed")
	}
	admin, err := store.UpsertUser(t.Context(), "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateUserAccess(t.Context(), admin.ID, "restricted", "manual restriction"); err != nil {
		t.Fatal(err)
	}
	allowed, err = server.canSignInEmail(t.Context(), "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("restricted admin account should stay blocked")
	}

	if _, err := store.UpsertUser(t.Context(), "existing@example.com", "Existing", ""); err != nil {
		t.Fatal(err)
	}
	allowed, err = server.canSignInEmail(t.Context(), "existing@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("existing users should be allowed to sign back in")
	}

	if err := store.UpsertConfig(t.Context(), map[string]string{
		"signup_mode":           "allowlist",
		"signup_allowed_emails": "pilot@gmail.com, @trusted.test",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"pilot@gmail.com", "crew@trusted.test"} {
		allowed, err = server.canSignInEmail(t.Context(), email)
		if err != nil {
			t.Fatal(err)
		}
		if !allowed {
			t.Fatalf("%s should be allowed by allowlist", email)
		}
	}
	allowed, err = server.canSignInEmail(t.Context(), "stranger@gmail.com")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("unlisted user should be rejected in allowlist mode")
	}
}

func TestLoginRetiresPendingDeletionUserBeforeSignIn(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{"signup_mode": "all"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", DevAuth: true}, http: http.DefaultClient}
	oldUser, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateUserAccess(t.Context(), oldUser.ID, "restricted", accountDeletionAccessNote); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/dev/login?email=pilot@example.com", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dev login returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	retired, err := store.UserByID(t.Context(), oldUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Email == "pilot@example.com" || retired.AccessStatus != "restricted" {
		t.Fatalf("retired user=%+v, want restricted tombstone", retired)
	}
	newUser, err := store.UserByEmail(t.Context(), "pilot@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if newUser.ID == oldUser.ID {
		t.Fatal("login reused pending-deletion user id")
	}
	if newUser.AccessStatus != "active" {
		t.Fatalf("new user access_status=%q, want active", newUser.AccessStatus)
	}
}

func TestNormalizeAdminConfigValuesFormatsAllowlistAndPool(t *testing.T) {
	values, err := normalizeAdminConfigValues(map[string]string{
		"signup_allowed_emails": "Pilot@Gmail.com, @Trusted.test\npilot@gmail.com",
		"fibe_agent_server_pool": `[{"label":"Main","agentId":" agent-1 ","serverId":" server-1 "},
			{"label":"","agent_id":"","server_id":""}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if values["signup_allowed_emails"] != "pilot@gmail.com\n@trusted.test" {
		t.Fatalf("allowlist=%q, want newline-normalized emails", values["signup_allowed_emails"])
	}
	pool, err := fibe.ParseAssignmentPool(values["fibe_agent_server_pool"])
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 1 || pool[0].AgentID != "agent-1" || pool[0].MarqueeID != "server-1" {
		t.Fatalf("pool=%+v, want normalized single pair", pool)
	}
}

func TestNormalizeAdminConfigRejectsIncompletePoolRows(t *testing.T) {
	_, err := normalizeAdminConfigValues(map[string]string{
		"fibe_agent_server_pool": `[{"agent_id":"agent-only"}]`,
	})
	if err == nil || !strings.Contains(err.Error(), "requires both") {
		t.Fatalf("err=%v, want incomplete pool row error", err)
	}
}

func TestNormalizeAdminConfigRejectsInvalidPoolStatus(t *testing.T) {
	_, err := normalizeAdminConfigValues(map[string]string{
		"fibe_agent_server_pool": `[{"agent_id":"agent-1","server_id":"server-1","status":"paused"}]`,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("err=%v, want invalid status error", err)
	}
}

func TestAdminRetireAgentPoolArchivesProjectsAndMarksPairRetired(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"}, http: http.DefaultClient}
	admin, err := store.UpsertUser(t.Context(), "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), admin.ID, "admin-pool-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "pilot-pool-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_agent_server_pool": `[{"label":"Old","agent_id":"old-agent","server_id":"old-server","status":"draining"},{"label":"New","agent_id":"new-agent","server_id":"new-server","status":"active"}]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-retire", UserID: user.ID, Title: "Retire", ConversationID: "conv-retire", AgentID: "old-agent", MarqueeID: "old-server", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/agent-pool/retire", strings.NewReader(`{"agent_id":"old-agent","server_id":"old-server"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "admin-pool-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("retire returned %d: %s", rec.Code, rec.Body.String())
	}
	cfg, err := store.ConfigMap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := fibe.ParseAssignmentPool(cfg["fibe_agent_server_pool"])
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 2 || pool[0].Status != fibe.AssignmentStatusRetired || pool[1].Status != fibe.AssignmentStatusActive {
		t.Fatalf("pool=%+v, want old retired and new active", pool)
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "archived" {
		t.Fatalf("status=%q, want archived", updated.Status)
	}
	archive, err := store.LatestProjectArchive(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archive.StoragePath == "" || archive.Status != "ready" {
		t.Fatalf("archive=%+v, want ready stored archive", archive)
	}
	exportReq := httptest.NewRequest(http.MethodPost, "/api/projects/project-retire/archive", strings.NewReader(`{}`))
	exportReq.AddCookie(&http.Cookie{Name: "likeable_session", Value: "pilot-pool-token"})
	exportRec := httptest.NewRecorder()
	server.routes().ServeHTTP(exportRec, exportReq)
	if exportRec.Code != http.StatusOK {
		t.Fatalf("archived project zip export returned %d: %s", exportRec.Code, exportRec.Body.String())
	}
}

func TestAdminUserResponsesExposeProjectAssignments(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"}, http: http.DefaultClient}
	admin, err := store.UpsertUser(t.Context(), "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), admin.ID, "admin-assignment-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_agent_server_pool": `[{"label":"Main","agent_id":"agent-1","server_id":"server-1","status":"active"}]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-assignment-summary", UserID: user.ID, Title: "Assigned", ConversationID: "conv-assignment-summary", AgentID: "agent-1", MarqueeID: "server-1", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	listReq.AddCookie(&http.Cookie{Name: "likeable_session", Value: "admin-assignment-token"})
	listRec := httptest.NewRecorder()
	server.routes().ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("admin users returned %d: %s", listRec.Code, listRec.Body.String())
	}
	var listBody struct {
		Users     []AdminUserSummary `json:"users"`
		AgentPool []AgentPoolOption  `json:"agentPool"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	if len(listBody.AgentPool) != 1 || listBody.AgentPool[0].AgentID != "agent-1" || listBody.AgentPool[0].Status != fibe.AssignmentStatusActive {
		t.Fatalf("agentPool=%+v, want configured active option", listBody.AgentPool)
	}
	var customer AdminUserSummary
	for _, summary := range listBody.Users {
		if summary.User.ID == user.ID {
			customer = summary
			break
		}
	}
	if len(customer.AgentPairs) != 1 || customer.AgentPairs[0].AgentID != "agent-1" || customer.AgentPairs[0].ServerID != "server-1" || customer.AgentPairs[0].Status != fibe.AssignmentStatusActive || customer.AgentPairs[0].ProjectCount != 1 {
		t.Fatalf("agentPairs=%+v, want active project assignment summary", customer.AgentPairs)
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/api/admin/users/"+user.ID, nil)
	detailReq.AddCookie(&http.Cookie{Name: "likeable_session", Value: "admin-assignment-token"})
	detailRec := httptest.NewRecorder()
	server.routes().ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("admin user detail returned %d: %s", detailRec.Code, detailRec.Body.String())
	}
	var detail AdminUserDetail
	if err := json.Unmarshal(detailRec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Projects) != 1 || detail.Projects[0].Assignment.AgentID != "agent-1" || detail.Projects[0].Assignment.ServerID != "server-1" || detail.Projects[0].Assignment.Status != fibe.AssignmentStatusActive {
		t.Fatalf("project assignments=%+v, want active assignment exposed to admin", detail.Projects)
	}
	if strings.Contains(detailRec.Body.String(), `"agentId":"agent-1"`) && strings.Contains(detailRec.Body.String(), `"project":{"`) && strings.Contains(detailRec.Body.String(), `"AgentID"`) {
		t.Fatalf("admin detail leaked Go internal field casing: %s", detailRec.Body.String())
	}
}

func TestAdminRecoveryReportsDeletionBacklog(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"}, http: http.DefaultClient}
	admin, err := store.UpsertUser(t.Context(), "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), admin.ID, "admin-recovery-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	waitingUser, err := store.UpsertUser(t.Context(), "waiting@example.com", "Waiting", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateUserAccess(t.Context(), waitingUser.ID, "restricted", accountDeletionAccessNote); err != nil {
		t.Fatal(err)
	}
	readyUser, err := store.UpsertUser(t.Context(), "ready@example.com", "Ready", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateUserAccess(t.Context(), readyUser.ID, "restricted", accountDeletionAccessNote); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-recovery",
		UserID:         waitingUser.ID,
		Title:          "Recovery",
		ConversationID: "conv-recovery",
		PlaygroundID:   "playground-recovery",
		PlayspecID:     "playspec-recovery",
		PropID:         "prop-recovery",
		Status:         "deleting",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateProjectCleanupError(t.Context(), project.ID, waitingUser.ID, "previous timeout"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/recovery", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "admin-recovery-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin recovery returned %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		CheckedAt                   string                 `json:"checkedAt"`
		DeletingProjects            []adminRecoveryProject `json:"deletingProjects"`
		PendingAccountDeletions     []adminRecoveryAccount `json:"pendingAccountDeletions"`
		DeletingProjectCount        int                    `json:"deletingProjectCount"`
		PendingAccountDeletionCount int                    `json:"pendingAccountDeletionCount"`
		SweepIntervalSeconds        int                    `json:"sweepIntervalSeconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.CheckedAt == "" || body.SweepIntervalSeconds != int(projectDeletionSweepInterval.Seconds()) {
		t.Fatalf("recovery metadata=%+v, want checkedAt and sweep interval", body)
	}
	if body.DeletingProjectCount != 1 || len(body.DeletingProjects) != 1 {
		t.Fatalf("deleting projects=%+v count=%d, want one", body.DeletingProjects, body.DeletingProjectCount)
	}
	if body.DeletingProjects[0].ID != project.ID || body.DeletingProjects[0].CleanupLastError != "previous timeout" {
		t.Fatalf("deleting project=%+v, want cleanup failure details", body.DeletingProjects[0])
	}
	if body.PendingAccountDeletionCount != 2 || len(body.PendingAccountDeletions) != 2 {
		t.Fatalf("pending accounts=%+v count=%d, want two", body.PendingAccountDeletions, body.PendingAccountDeletionCount)
	}
	accountsByEmail := map[string]adminRecoveryAccount{}
	for _, account := range body.PendingAccountDeletions {
		accountsByEmail[account.Email] = account
	}
	if account := accountsByEmail["waiting@example.com"]; account.Ready || account.ProjectCount != 1 {
		t.Fatalf("waiting account=%+v, want not ready with one project", account)
	}
	if account := accountsByEmail["ready@example.com"]; !account.Ready || account.ProjectCount != 0 {
		t.Fatalf("ready account=%+v, want ready with zero projects", account)
	}
}

func TestAdminProjectAssignmentPatchValidatesTargetAndPreservesProjectState(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"}, http: http.DefaultClient}
	admin, err := store.UpsertUser(t.Context(), "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), admin.ID, "admin-assignment-patch-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_agent_server_pool": `[
			{"label":"Old","agent_id":"old-agent","server_id":"old-server","status":"active"},
			{"label":"New","agent_id":"new-agent","server_id":"new-server","status":"active"},
			{"label":"Drain","agent_id":"drain-agent","server_id":"drain-server","status":"draining"},
			{"label":"Retired","agent_id":"retired-agent","server_id":"retired-server","status":"retired"}
		]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:              "project-assignment-patch",
		UserID:          user.ID,
		Title:           "Patch",
		ConversationID:  "conv-assignment-patch",
		AgentID:         "old-agent",
		MarqueeID:       "old-server",
		PlaygroundID:    "playground-1",
		PlaygroundName:  "playground-name",
		PlayspecID:      "playspec-1",
		PropID:          "prop-1",
		RepoURL:         "http://gitea.test/owner/repo.git",
		PreviewURL:      "http://preview.example.test",
		SelectedService: "app",
		Status:          "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	patchAssignment := func(agentID, serverID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+user.ID+"/projects/"+project.ID+"/assignment", strings.NewReader(`{"agent_id":"`+agentID+`","server_id":"`+serverID+`"}`))
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "admin-assignment-patch-token"})
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		return rec
	}
	if rec := patchAssignment("unknown-agent", "unknown-server"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown target returned %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if rec := patchAssignment("drain-agent", "drain-server"); rec.Code != http.StatusBadRequest {
		t.Fatalf("draining target returned %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := patchAssignment("retired-agent", "retired-server"); rec.Code != http.StatusBadRequest {
		t.Fatalf("retired target returned %d, want 400: %s", rec.Code, rec.Body.String())
	}
	rec := patchAssignment("new-agent", "new-server")
	if rec.Code != http.StatusOK {
		t.Fatalf("active target returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	updated, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AgentID != "new-agent" || updated.MarqueeID != "new-server" {
		t.Fatalf("assignment=%s/%s, want new active pair", updated.AgentID, updated.MarqueeID)
	}
	if updated.PlaygroundID != project.PlaygroundID || updated.PlaygroundName != project.PlaygroundName || updated.PlayspecID != project.PlayspecID || updated.PropID != project.PropID || updated.RepoURL != project.RepoURL || updated.PreviewURL != project.PreviewURL || updated.SelectedService != project.SelectedService || updated.Status != project.Status {
		t.Fatalf("project state changed during assignment: before=%+v after=%+v", project, updated)
	}
	var body struct {
		Detail  AdminUserDetail `json:"detail"`
		Warning string          `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Detail.Projects) != 1 || body.Detail.Projects[0].Assignment.AgentID != "new-agent" || body.Detail.Projects[0].Assignment.Status != fibe.AssignmentStatusActive {
		t.Fatalf("response detail=%+v, want updated active assignment", body.Detail.Projects)
	}
}

func TestAdminProjectAssignmentPatchMakesNextMessageUseNewAgentAndSameContext(t *testing.T) {
	cliPath, logPath, stdinPath := fakeFibeCLI(t)
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"}, http: http.DefaultClient}
	admin, err := store.UpsertUser(t.Context(), "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), admin.ID, "admin-assignment-message-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "pilot-assignment-message-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url":          "server.test:3000",
		"fibe_api_key":           "test-key",
		"fibe_cli_path":          cliPath,
		"fibe_agent_server_pool": `[{"label":"Old","agent_id":"old-agent","server_id":"old-server","status":"active"},{"label":"New","agent_id":"new-agent","server_id":"new-server","status":"active"}]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-assignment-message",
		UserID:         user.ID,
		Title:          "Message",
		ConversationID: "conv-assignment-message",
		AgentID:        "old-agent",
		MarqueeID:      "old-server",
		PlaygroundID:   "123",
		PlaygroundName: "lk-message",
		PlayspecID:     "456",
		PropID:         "789",
		RepoURL:        "http://gitea.test/owner/repo.git",
		PreviewURL:     "http://preview.example.test",
		Status:         "ready",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}

	patchReq := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+user.ID+"/projects/"+project.ID+"/assignment", strings.NewReader(`{"agent_id":"new-agent","server_id":"new-server"}`))
	patchReq.AddCookie(&http.Cookie{Name: "likeable_session", Value: "admin-assignment-message-token"})
	patchRec := httptest.NewRecorder()
	server.routes().ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("assignment patch returned %d: %s", patchRec.Code, patchRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+project.ID+"/messages", strings.NewReader(`{"text":"keep building"}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "pilot-assignment-message-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("message returned %d: %s", rec.Code, rec.Body.String())
	}
	log := readFile(t, logPath)
	if !strings.Contains(log, "agents start-chat new-agent --marquee-id new-server") {
		t.Fatalf("commands=%s, want reassigned agent chat warmed", log)
	}
	if !strings.Contains(log, "agents send-message new-agent") {
		t.Fatalf("commands=%s, want next user message sent to reassigned agent", log)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(readFile(t, stdinPath)), &payload); err != nil {
		t.Fatal(err)
	}
	prompt := payload.Text
	for _, want := range []string{
		"target Fibe playground_id: 123",
		"target private source repo: http://gitea.test/owner/repo.git",
		"User request:\nkeep building",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestArchivedProjectsDoNotCountTowardProjectQuota(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []*Project{
		{ID: "project-active", UserID: user.ID, Title: "Active", ConversationID: "conv-active", Status: "ready"},
		{ID: "project-archived", UserID: user.ID, Title: "Archived", ConversationID: "conv-archived", Status: "archived"},
	} {
		if err := store.CreateProject(t.Context(), project); err != nil {
			t.Fatal(err)
		}
	}
	count, err := store.ProjectCountForUser(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("project count=%d, want active project only", count)
	}
	excess, err := store.ProjectsExceedingQuota(t.Context(), user.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(excess) != 0 {
		t.Fatalf("excess=%+v, want archived project excluded", excess)
	}
}

func TestPublicAdminConfigExposesSMTPSettings(t *testing.T) {
	cfg := publicAdminConfig(map[string]string{
		"smtp_host":       "smtp.example.com",
		"smtp_port":       "2525",
		"smtp_from_email": "noreply@example.com",
		"smtp_password":   "secret",
		"github_username": "fallback-owner",
		"github_token":    "ghp_secret",
	})
	if entry := cfg["smtp_host"].(map[string]any); entry["value"] != "smtp.example.com" || entry["secret"].(bool) {
		t.Fatalf("smtp_host entry=%+v, want public value", entry)
	}
	if entry := cfg["smtp_password"].(map[string]any); !entry["secret"].(bool) || !entry["set"].(bool) || entry["value"] != "" {
		t.Fatalf("smtp_password entry=%+v, want write-only secret", entry)
	}
	if entry := cfg["github_username"].(map[string]any); entry["value"] != "fallback-owner" || entry["secret"].(bool) {
		t.Fatalf("github_username entry=%+v, want public fallback owner", entry)
	}
	if entry := cfg["github_token"].(map[string]any); !entry["secret"].(bool) || !entry["set"].(bool) || entry["value"] != "" {
		t.Fatalf("github_token entry=%+v, want write-only secret", entry)
	}
}

func TestCreateProjectRecordStoresAssignedFibePair(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_agent_server_pool": `[{"label":"A","agent_id":"agent-a","server_id":"server-a"},{"label":"B","agent_id":"agent-b","server_id":"server-b"}]`,
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	project, err := server.createProjectRecord(t.Context(), user, "Assigned app")
	if err != nil {
		t.Fatal(err)
	}
	if !((project.AgentID == "agent-a" && project.MarqueeID == "server-a") || (project.AgentID == "agent-b" && project.MarqueeID == "server-b")) {
		t.Fatalf("project assignment=%s/%s, want configured pool pair", project.AgentID, project.MarqueeID)
	}
	if project.PlaygroundName != projecttext.SourceNameForProject(project) {
		t.Fatalf("playground name=%q, want deterministic project source name", project.PlaygroundName)
	}
	stored, err := store.ProjectForUser(t.Context(), user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AgentID != project.AgentID || stored.MarqueeID != project.MarqueeID {
		t.Fatalf("stored assignment=%s/%s, want %s/%s", stored.AgentID, stored.MarqueeID, project.AgentID, project.MarqueeID)
	}
	if stored.PlaygroundName != project.PlaygroundName {
		t.Fatalf("stored playground name=%q, want %q", stored.PlaygroundName, project.PlaygroundName)
	}
}

func TestProvisionProjectLeaseSkipsDuplicateGreenfield(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, logPath, _ := fakeFibeCLI(t)
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "pilot@example.com", "Pilot", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{
		ID:             "project-lease",
		UserID:         user.ID,
		Title:          "Lease",
		ConversationID: "conv-lease",
		AgentID:        "agent-1",
		Status:         "creating",
	}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	acquired, err := store.TryAcquireProjectProvisioning(t.Context(), project.ID, user.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("setup lease was not acquired")
	}

	if err := server.provisionProject(t.Context(), user.ID, user.Email, project, ""); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(logPath); err == nil && strings.Contains(string(data), "greenfield") {
		t.Fatalf("duplicate provisioning issued Greenfield command: %s", string(data))
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestFibeClientForProjectUsesStoredAssignment(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"fibe_base_url":   "server.test:3000",
		"fibe_api_key":    "secret",
		"fibe_agent_id":   "global-agent",
		"fibe_marquee_id": "global-marquee",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	client, err := server.fibeClientForProject(t.Context(), &Project{AgentID: "stored-agent", MarqueeID: "stored-marquee"}, "pilot@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if client.AgentID() != "stored-agent" || client.MarqueeID() != "stored-marquee" {
		t.Fatalf("client pair=%s/%s, want stored pair", client.AgentID(), client.MarqueeID())
	}
	if client.BaseURL() != "http://server.test:3000" {
		t.Fatalf("baseURL=%q, want normalized local URL", client.BaseURL())
	}
}

func TestAdminUserListingAndRestrictionControls(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"}, http: http.DefaultClient}

	user, err := store.UpsertUser(t.Context(), "customer@example.com", "Customer", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-customer", UserID: user.ID, Title: "Customer app", ConversationID: "conv-customer", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddMessage(t.Context(), project.ID, "user", "build"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSocialConnection(t.Context(), SocialConnection{UserID: user.ID, Provider: "github", ProviderUserID: "gh-customer", AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPayment(t.Context(), Payment{UserID: user.ID, ProviderPaymentID: "cs_test", AmountCents: 2500, Currency: "usd", Status: "paid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddUserNotice(t.Context(), UserNotice{UserID: user.ID, Severity: "warning", Body: "Please reduce usage."}); err != nil {
		t.Fatal(err)
	}

	users, total, err := store.AdminUsers(t.Context(), AdminUserFilters{Github: "connected", Billing: "paid", Page: 1, PerPage: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(users) != 1 {
		t.Fatalf("users=%d total=%d, want single paid github-connected user", len(users), total)
	}
	got := users[0]
	startedAt := time.Now().UTC().Add(-40 * time.Minute)
	if err := store.StartProjectWorkSession(t.Context(), user.ID, project.ID, "turn-admin", startedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndBillProjectWorkSession(t.Context(), user.ID, project.ID, "turn-admin", startedAt.Add(30*time.Minute), 5, int64(5*time.Hour/time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	users, total, err = store.AdminUsers(t.Context(), AdminUserFilters{Github: "connected", Billing: "paid", Page: 1, PerPage: 10})
	if err != nil {
		t.Fatal(err)
	}
	got = users[0]
	if got.LifetimeWorkMs != int64(30*time.Minute/time.Millisecond) || got.ProjectCount != 1 || !got.GithubConnected || got.PaidTotalCents != 2500 || got.LatestNotice == nil {
		t.Fatalf("summary=%+v, want usage/github/payment/notice populated", got)
	}

	if _, err := store.UpdateUserAccess(t.Context(), user.ID, "restricted", "abuse review"); err != nil {
		t.Fatal(err)
	}
	allowed, err := server.canSignInEmail(t.Context(), user.Email)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("restricted user should not be allowed to sign in")
	}
	if err := store.CreateSession(t.Context(), user.ID, "restricted-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "restricted-token"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restricted projects request returned %d, want 403", rec.Code)
	}
}

func TestAdminNoticeSendsEmailWhenSMTPConfigured(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"smtp_host":       "smtp.example.test",
		"smtp_port":       "2525",
		"smtp_from_email": "noreply@example.test",
		"smtp_tls_mode":   "none",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	ch := make(chan emailMessage, 1)
	server := &Server{
		store:  store,
		config: RuntimeConfig{BaseURL: "http://example.test", AdminEmail: "admin@example.com"},
		http:   http.DefaultClient,
		email:  captureEmailSender{ch: ch},
	}
	admin, err := store.UpsertUser(t.Context(), "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.UpsertUser(t.Context(), "customer@example.com", "Customer", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), admin.ID, "admin-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/users/"+target.ID+"/notices", strings.NewReader(`{"severity":"warning","body":"Please reduce usage."}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "admin-token"})
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("notice returned %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case message := <-ch:
		if message.To != "customer@example.com" || message.Subject != "New Likeable message" || !strings.Contains(message.Body, "Please reduce usage.") {
			t.Fatalf("email=%+v, want customer notice email", message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for notice email")
	}
}

func TestFixedUTCFreeHoursAndPaidBalance(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "customer@example.com", "Customer", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-quota", UserID: user.ID, Title: "Quota app", ConversationID: "conv-quota", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{"free_hours": "1", "free_hour_window_hours": "24"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	windowStart, _ := server.freeHourWindow(time.Now().UTC(), t.Context())
	oldStart := windowStart.Add(-90 * time.Minute)
	if err := store.StartProjectWorkSession(t.Context(), user.ID, project.ID, "old-turn", oldStart); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndBillProjectWorkSession(t.Context(), user.ID, project.ID, "old-turn", oldStart.Add(30*time.Minute), server.freeHourWindowHours(t.Context()), server.freeHourLimitMs(t.Context())); err != nil {
		t.Fatal(err)
	}
	currentStart := windowStart.Add(time.Minute)
	if err := store.StartProjectWorkSession(t.Context(), user.ID, project.ID, "current-turn", currentStart); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndBillProjectWorkSession(t.Context(), user.ID, project.ID, "current-turn", currentStart.Add(10*time.Minute), server.freeHourWindowHours(t.Context()), server.freeHourLimitMs(t.Context())); err != nil {
		t.Fatal(err)
	}
	quota := server.hourQuota(t.Context(), user)
	if quota["usedMs"] != int64(10*time.Minute/time.Millisecond) || quota["remainingMs"] != int64(50*time.Minute/time.Millisecond) || quota["lifetimeUsedMs"] != int64(40*time.Minute/time.Millisecond) {
		t.Fatalf("quota=%+v, want current window 10m used, 50m remaining, 40m lifetime", quota)
	}
	if quota["windowHours"] != 24 {
		t.Fatalf("quota windowHours=%v, want 24", quota["windowHours"])
	}
	if _, err := store.GrantHourCredits(t.Context(), user.ID, "cs_pack", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantHourCredits(t.Context(), user.ID, "cs_pack", 10); err != nil {
		t.Fatal(err)
	}
	balance, err := store.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != int64(10*time.Hour/time.Millisecond) {
		t.Fatalf("balance=%d, want idempotent grant of 10h", balance)
	}
	allowed, err := server.hourAllowance(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("hour allowance should allow user with free time or paid balance")
	}
	paidStart := windowStart.Add(20 * time.Minute)
	if err := store.StartProjectWorkSession(t.Context(), user.ID, project.ID, "paid-turn", paidStart); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndBillProjectWorkSession(t.Context(), user.ID, project.ID, "paid-turn", paidStart.Add(70*time.Minute), server.freeHourWindowHours(t.Context()), server.freeHourLimitMs(t.Context())); err != nil {
		t.Fatal(err)
	}
	balance, err = store.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != int64((10*time.Hour-20*time.Minute)/time.Millisecond) {
		t.Fatalf("balance=%d, want 20 paid minutes consumed after free hour", balance)
	}
}

func TestFixedUTCHourWindowAnchorsToMidnight(t *testing.T) {
	tests := []struct {
		name     string
		now      string
		hours    int
		wantFrom string
		wantTo   string
	}{
		{
			name:     "regular bucket",
			now:      "2026-05-16T11:37:10Z",
			hours:    5,
			wantFrom: "2026-05-16T10:00:00Z",
			wantTo:   "2026-05-16T15:00:00Z",
		},
		{
			name:     "short final bucket returns midnight reset",
			now:      "2026-05-16T23:30:00Z",
			hours:    5,
			wantFrom: "2026-05-16T20:00:00Z",
			wantTo:   "2026-05-17T00:00:00Z",
		},
		{
			name:     "exact boundary starts new bucket",
			now:      "2026-05-16T05:00:00Z",
			hours:    5,
			wantFrom: "2026-05-16T05:00:00Z",
			wantTo:   "2026-05-16T10:00:00Z",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			from, to := fixedUTCHourWindow(now, time.Duration(tc.hours)*time.Hour)
			if from.Format(time.RFC3339) != tc.wantFrom || to.Format(time.RFC3339) != tc.wantTo {
				t.Fatalf("window=%s..%s, want %s..%s", from.Format(time.RFC3339), to.Format(time.RFC3339), tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestFreeHourWindowHoursConfig(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	if got := server.freeHourWindowHours(t.Context()); got != defaultFreeHourWindowHours {
		t.Fatalf("default window hours=%d, want %d", got, defaultFreeHourWindowHours)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{"free_hour_window_hours": "2"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	if got := server.freeHourWindowHours(t.Context()); got != 2 {
		t.Fatalf("configured window hours=%d, want 2", got)
	}
}

func TestHourQuotaResetAtUsesNextFixedWindowWhenUnused(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "unused-quota@example.com", "Unused", "")
	if err != nil {
		t.Fatal(err)
	}

	quota := server.hourQuota(t.Context(), user)
	if quota["usedMs"] != int64(0) || quota["remainingMs"] != quota["limitMs"] {
		t.Fatalf("quota=%+v, want unused full quota", quota)
	}
	resetAt, err := time.Parse(time.RFC3339, quota["resetsAt"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !resetAt.After(time.Now().UTC()) {
		t.Fatalf("resetsAt=%s, want next fixed window boundary in the future", resetAt.Format(time.RFC3339))
	}
}

func TestPaidProjectQuotaExtendsProjectCap(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "projects@example.com", "Projects", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{"project_cap": "1"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	if cap := server.projectCapForUser(t.Context(), user); cap != 1 {
		t.Fatalf("cap=%d, want base cap 1", cap)
	}
	granted, err := store.GrantProjectQuota(t.Context(), user.ID, "cs_project_slot", 1, time.Now().UTC().Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !granted {
		t.Fatal("first project quota grant should be applied")
	}
	granted, err = store.GrantProjectQuota(t.Context(), user.ID, "cs_project_slot", 1, time.Now().UTC().Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if granted {
		t.Fatal("duplicate project quota payment should be idempotent")
	}
	if cap := server.projectCapForUser(t.Context(), user); cap != 2 {
		t.Fatalf("cap=%d, want base plus paid slot", cap)
	}
	quota := server.projectQuota(t.Context(), user)
	if quota["baseLimit"] != 1 || quota["paidSlots"] != 1 || quota["limit"] != 2 {
		t.Fatalf("quota=%+v, want paid project quota reflected", quota)
	}
}

func TestProjectQuotaCheckoutBuildsStripeMetadata(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"stripe_secret_key":             "sk_test",
		"stripe_project_quota_price_id": "price_project_slot",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "buyer@example.com", "Buyer", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "buyer-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	var form url.Values
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		form, err = url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"url":"https://checkout.stripe.test/session"}`)),
		}, nil
	})}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: client}
	req := httptest.NewRequest(http.MethodPost, "/api/billing/checkout", strings.NewReader(`{"product":"project_quota","slots":1}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "buyer-token"})
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("checkout returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if form.Get("line_items[0][price]") != "price_project_slot" ||
		form.Get("metadata[purchase_kind]") != "project_quota" ||
		form.Get("metadata[project_slots]") != "1" ||
		form.Get("metadata[project_quota_days]") != "30" {
		t.Fatalf("stripe form=%v, want project quota metadata", form)
	}
}

func TestHourPackCheckoutBuildsStripeMetadata(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"stripe_secret_key":        "sk_test",
		"stripe_price_id_10_hours": "price_10h",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "buyer@example.com", "Buyer", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), user.ID, "hour-buyer-token", time.Hour); err != nil {
		t.Fatal(err)
	}
	var form url.Values
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		form, err = url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"url":"https://checkout.stripe.test/session"}`)),
		}, nil
	})}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: client}
	req := httptest.NewRequest(http.MethodPost, "/api/billing/checkout", strings.NewReader(`{"product":"hour_pack","pack":10}`))
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "hour-buyer-token"})
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("checkout returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if form.Get("line_items[0][price]") != "price_10h" ||
		form.Get("metadata[purchase_kind]") != "hour_pack" ||
		form.Get("metadata[pack_hours]") != "10" {
		t.Fatalf("stripe form=%v, want hour pack metadata", form)
	}
}

func TestStripeWebhookGrantsProjectQuota(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"stripe_secret_key":             "sk_test",
		"stripe_webhook_secret":         "whsec_test",
		"stripe_project_quota_price_id": "price_project_slot",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "buyer@example.com", "Buyer", "")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "/line_items") {
			t.Fatalf("unexpected stripe request %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"price":{"id":"price_project_slot"}}]}`)),
		}, nil
	})}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: client}
	event := map[string]any{
		"type": "checkout.session.completed",
		"data": map[string]any{"object": map[string]any{
			"id":                  "cs_project_quota",
			"client_reference_id": user.ID,
			"amount_total":        1200,
			"currency":            "usd",
			"payment_status":      "paid",
			"status":              "complete",
			"metadata": map[string]any{
				"purchase_kind": "project_quota",
				"project_slots": "1",
			},
		}},
	}
	payload, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/api/stripe/webhook", strings.NewReader(string(payload)))
	req.Header.Set("Stripe-Signature", testStripeSignature("whsec_test", payload))
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("webhook returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	slots, expiresAt, err := store.ActiveProjectQuota(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slots != 1 || expiresAt == "" {
		t.Fatalf("slots=%d expires=%q, want active paid project slot", slots, expiresAt)
	}
	notices, err := store.NoticesForUser(t.Context(), user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) == 0 || !strings.Contains(notices[0].Body, "Project quota purchased") {
		t.Fatalf("notices=%+v, want project quota purchase notice", notices)
	}
}

func TestStripeWebhookGrantsHourPack(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertConfig(t.Context(), map[string]string{
		"stripe_secret_key":         "sk_test",
		"stripe_webhook_secret":     "whsec_test",
		"stripe_price_id_100_hours": "price_100h",
	}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	user, err := store.UpsertUser(t.Context(), "buyer@example.com", "Buyer", "")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "/line_items") {
			t.Fatalf("unexpected stripe request %s", req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"price":{"id":"price_100h"}}]}`)),
		}, nil
	})}
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: client}
	event := map[string]any{
		"type": "checkout.session.completed",
		"data": map[string]any{"object": map[string]any{
			"id":                  "cs_hour_pack",
			"client_reference_id": user.ID,
			"amount_total":        9900,
			"currency":            "usd",
			"payment_status":      "paid",
			"status":              "complete",
			"metadata": map[string]any{
				"purchase_kind": "hour_pack",
				"pack_hours":    "100",
			},
		}},
	}
	payload, _ := json.Marshal(event)
	req := httptest.NewRequest(http.MethodPost, "/api/stripe/webhook", strings.NewReader(string(payload)))
	req.Header.Set("Stripe-Signature", testStripeSignature("whsec_test", payload))
	rec := httptest.NewRecorder()

	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("webhook returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	balance, err := store.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != int64(100*time.Hour/time.Millisecond) {
		t.Fatalf("balance=%d, want 100 paid hours", balance)
	}
	if granted, err := store.GrantHourCredits(t.Context(), user.ID, "cs_hour_pack", 100); err != nil || granted {
		t.Fatalf("duplicate grant granted=%v err=%v, want idempotent no-op", granted, err)
	}
	notices, err := store.NoticesForUser(t.Context(), user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) == 0 || !strings.Contains(notices[0].Body, "Build hours purchased") {
		t.Fatalf("notices=%+v, want hour purchase notice", notices)
	}
}

func TestFreeHoursAreConsumedBeforePaidHoursAndDebtBlocksFutureWork(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "paid@example.com", "Paid", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-free-first", UserID: user.ID, Title: "Free first", ConversationID: "conv-free-first", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{"free_hours": "1", "free_hour_window_hours": "24"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantHourCredits(t.Context(), user.ID, "cs_paid_pack", 1); err != nil {
		t.Fatal(err)
	}

	allowed, err := server.hourAllowance(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("first allowance should use available free hours")
	}
	windowStart, _ := server.freeHourWindow(time.Now().UTC(), t.Context())
	firstStart := windowStart.Add(time.Minute)
	if err := store.StartProjectWorkSession(t.Context(), user.ID, project.ID, "free-turn", firstStart); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndBillProjectWorkSession(t.Context(), user.ID, project.ID, "free-turn", firstStart.Add(45*time.Minute), server.freeHourWindowHours(t.Context()), server.freeHourLimitMs(t.Context())); err != nil {
		t.Fatal(err)
	}
	balance, err := store.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != int64(time.Hour/time.Millisecond) {
		t.Fatalf("balance=%d, want paid hours untouched while free hours remain", balance)
	}

	overrunStart := windowStart.Add(time.Hour)
	if err := store.StartProjectWorkSession(t.Context(), user.ID, project.ID, "overrun-turn", overrunStart); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndBillProjectWorkSession(t.Context(), user.ID, project.ID, "overrun-turn", overrunStart.Add(100*time.Minute), server.freeHourWindowHours(t.Context()), server.freeHourLimitMs(t.Context())); err != nil {
		t.Fatal(err)
	}
	balance, err = store.PaidHourCreditBalance(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != int64(-25*time.Minute/time.Millisecond) {
		t.Fatalf("balance=%d, want 25m debt after overrun", balance)
	}

	allowed, err = server.hourAllowance(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("negative paid-hour balance should block future work")
	}
}

func TestOldMessageCreditsDoNotAllowHourBilling(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "old-credits@example.com", "Old Credits", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{"free_hours": "0"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantMessageCredits(t.Context(), user.ID, "old_message_pack", 100); err != nil {
		t.Fatal(err)
	}
	allowed, err := server.hourAllowance(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("old message credits should not allow hour-billed work")
	}
}

func TestHourQuotaNotificationIsDedupedPerWindow(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{store: store, config: RuntimeConfig{BaseURL: "http://example.test"}, http: http.DefaultClient}
	user, err := store.UpsertUser(t.Context(), "quota@example.com", "Quota", "")
	if err != nil {
		t.Fatal(err)
	}
	project := &Project{ID: "project-quota-notice", UserID: user.ID, Title: "Quota notice", ConversationID: "conv-quota-notice", Status: "ready"}
	if err := store.CreateProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertConfig(t.Context(), map[string]string{"free_hours": "1", "free_hour_window_hours": "24"}, secretConfigKeys); err != nil {
		t.Fatal(err)
	}
	windowStart, _ := server.freeHourWindow(time.Now().UTC(), t.Context())
	startedAt := windowStart.Add(time.Minute)
	if err := store.StartProjectWorkSession(t.Context(), user.ID, project.ID, "quota-turn", startedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAndBillProjectWorkSession(t.Context(), user.ID, project.ID, "quota-turn", startedAt.Add(55*time.Minute), server.freeHourWindowHours(t.Context()), server.freeHourLimitMs(t.Context())); err != nil {
		t.Fatal(err)
	}
	server.notifyHourQuotaIfNeeded(t.Context(), user)
	server.notifyHourQuotaIfNeeded(t.Context(), user)
	notices, err := store.NoticesForUser(t.Context(), user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var quotaNotices int
	for _, notice := range notices {
		if strings.HasPrefix(notice.Body, "Hour quota:") {
			quotaNotices++
		}
	}
	if quotaNotices != 1 {
		t.Fatalf("quota notices=%d, want 1", quotaNotices)
	}
}

func TestMailboxDismissAndAdminUnsend(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.UpsertUser(t.Context(), "customer@example.com", "Customer", "")
	if err != nil {
		t.Fatal(err)
	}
	notice, err := store.AddUserNotice(t.Context(), UserNotice{UserID: user.ID, Sender: "admin", Severity: "warning", Body: "Please reduce usage."})
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveNoticesForUser(t.Context(), user.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("active=%d, want 1", len(active))
	}
	dismissed, err := store.DismissUserNotice(t.Context(), user.ID, notice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dismissed.DismissedAt == "" || dismissed.ReadAt == "" {
		t.Fatalf("dismissed notice=%+v, want read and dismissed timestamps", dismissed)
	}
	active, err = store.ActiveNoticesForUser(t.Context(), user.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active=%d, want dismissed notice hidden", len(active))
	}
	if _, err := store.AddUserNotice(t.Context(), UserNotice{UserID: user.ID, Sender: "user", Body: "I need help."}); err != nil {
		t.Fatal(err)
	}
	history, err := store.NoticesForUser(t.Context(), user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history=%d, want admin and user messages", len(history))
	}
	if _, err := store.UnsendUserNotice(t.Context(), user.ID, notice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UserNotice(t.Context(), user.ID, notice.ID); err == nil {
		t.Fatal("unsent notice should not be visible to user history")
	}
	history, err = store.NoticesForUser(t.Context(), user.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Sender != "user" {
		t.Fatalf("history=%+v, want only user-to-admin message after unsend", history)
	}
}

func TestAnonymousRateLimitUsesIPAddress(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := &Server{
		store:  store,
		config: RuntimeConfig{BaseURL: "http://example.test"},
		http:   http.DefaultClient,
		limiter: newRateLimiter(rateLimitConfig{
			anonymousLimit:      2,
			anonymousWindow:     time.Minute,
			authenticatedLimit:  100,
			authenticatedWindow: time.Hour,
		}),
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.RemoteAddr = "203.0.113.10:45123"
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d returned %d, want 200; body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.RemoteAddr = "203.0.113.10:45123"
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("limited request returned %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" || rec.Header().Get("X-RateLimit-Limit") != "2" {
		t.Fatalf("rate headers missing: %+v", rec.Header())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.RemoteAddr = "203.0.113.11:45123"
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("different IP returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthenticatedRateLimitUsesUserID(t *testing.T) {
	store, err := store.Open(filepath.Join(t.TempDir(), "likeable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	userA, err := store.UpsertUser(t.Context(), "a@example.com", "A", "")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := store.UpsertUser(t.Context(), "b@example.com", "B", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), userA.ID, "token-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), userB.ID, "token-b", time.Hour); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		store:  store,
		config: RuntimeConfig{BaseURL: "http://example.test"},
		http:   http.DefaultClient,
		limiter: newRateLimiter(rateLimitConfig{
			anonymousLimit:      100,
			anonymousWindow:     time.Minute,
			authenticatedLimit:  2,
			authenticatedWindow: time.Hour,
		}),
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.RemoteAddr = "203.0.113.20:45123"
		req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
		rec := httptest.NewRecorder()
		server.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("authenticated request %d returned %d, want 200; body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.RemoteAddr = "203.0.113.20:45123"
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-a"})
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("limited authenticated request returned %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-RateLimit-Limit") != "2" {
		t.Fatalf("authenticated rate headers missing: %+v", rec.Header())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.RemoteAddr = "203.0.113.20:45123"
	req.AddCookie(&http.Cookie{Name: "likeable_session", Value: "token-b"})
	rec = httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("different authenticated user returned %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
