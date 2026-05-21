package fibe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	platformCodeUnknown          = "UNKNOWN_ERROR"
	platformCodeCLINotConfigured = "FIBE_CLI_NOT_CONFIGURED"
	platformCodeCLINotFound      = "FIBE_CLI_NOT_FOUND"
)

type PlatformError struct {
	Code    string
	Status  int
	Message string
	Details map[string]any
	Stderr  string
	Cause   error
}

func (e *PlatformError) Error() string {
	message := strings.TrimSpace(e.Message)
	if message == "" {
		message = "platform command failed"
	}
	if e.Code != "" && e.Status > 0 {
		return fmt.Sprintf("platform command failed: %s (%d): %s", e.Code, e.Status, message)
	}
	if e.Code != "" {
		return fmt.Sprintf("platform command failed: %s: %s", e.Code, message)
	}
	return "platform command failed: " + message
}

func (e *PlatformError) Unwrap() error {
	return e.Cause
}

func (e *PlatformError) PublicProjectErrorKind() string {
	message := strings.ToLower(strings.TrimSpace(e.Message + "\n" + e.Stderr))
	switch {
	case e.Code == platformCodeCLINotConfigured || e.Code == platformCodeCLINotFound:
		return "configuration"
	case e.Status == 401 || e.Status == 403:
		return "configuration"
	case isProvisioningConfigurationPlatformError(e, message):
		return "configuration"
	default:
		return ""
	}
}

func IsRetryableProvisioningError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var platform *PlatformError
	if !errors.As(err, &platform) {
		return false
	}
	code := strings.ToUpper(strings.TrimSpace(platform.Code))
	message := strings.ToLower(strings.TrimSpace(platform.Message + "\n" + platform.Stderr))
	if isProvisioningConfigurationPlatformError(platform, message) {
		return false
	}
	if greenfieldDefaultUnavailableBecauseMirrors(platform) || systemTemplateMirrorUnavailable(platform, message) {
		return true
	}
	switch code {
	case platformCodeCLINotConfigured, platformCodeCLINotFound:
		return false
	case "UNAUTHORIZED", "FORBIDDEN", "VALIDATION_FAILED", "BAD_REQUEST", "INVALID_ARGUMENT", "NOT_FOUND":
		return false
	case "INTERNAL_ERROR", "SERVICE_UNAVAILABLE", "TIMEOUT", "RATE_LIMITED", "TOO_MANY_REQUESTS":
		return true
	}
	if platform.Status == 408 || platform.Status == 425 || platform.Status == 429 || platform.Status >= 500 {
		return true
	}
	if platform.Status == 409 {
		return containsAny(message, "locked", "busy", "in progress", "try again")
	}
	if platform.Status == 422 && containsAny(message, "internal_error", "internal error", "unexpected status", "temporarily", "unavailable") {
		return true
	}
	return containsAny(message,
		"connection refused",
		"connection reset",
		"deadline exceeded",
		"temporary",
		"temporarily",
		"timeout",
		"timed out",
		"unexpected eof",
		"unavailable",
	)
}

func isProvisioningConfigurationPlatformError(err *PlatformError, message string) bool {
	if err == nil {
		return false
	}
	if greenfieldDefaultUnavailableBecauseMirrors(err) {
		return false
	}
	code := strings.ToUpper(strings.TrimSpace(err.Code))
	switch code {
	case "GREENFIELD_DEFAULT_TEMPLATE_VERSION_MISSING",
		"TEMPLATE_VERSION_NOT_FOUND":
		return true
	case "GREENFIELD_DEFAULT_TEMPLATE_VERSION_UNAVAILABLE":
		return true
	}
	return containsAny(message,
		"greenfield_default_template_version_missing",
		"no default greenfield template version is configured",
		"default greenfield template version is configured but is not available",
	)
}

func greenfieldDefaultUnavailableBecauseMirrors(err *PlatformError) bool {
	if err == nil || err.Details == nil {
		return false
	}
	if value, ok := err.Details["mirrors_ready"].(bool); ok && !value {
		return true
	}
	if _, ok := err.Details["missing_sources"]; ok {
		return true
	}
	return false
}

func systemTemplateMirrorUnavailable(err *PlatformError, message string) bool {
	if err == nil {
		return false
	}
	code := strings.ToUpper(strings.TrimSpace(err.Code))
	return code == "SYSTEM_TEMPLATE_MIRROR_UNAVAILABLE" ||
		containsAny(message, "system_template_mirror_unavailable", "system template source mirror is not available")
}

func IsIdempotentConversationCreateError(err error) bool {
	var platform *PlatformError
	if !errors.As(err, &platform) {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(platform.Message + "\n" + platform.Stderr))
	return (platform.Status == 409 || platform.Status == 422) && containsAny(message, "already exists", "conversation exists", "duplicate")
}

func IsPlaygroundAlreadyStoppedError(err error) bool {
	var platform *PlatformError
	if !errors.As(err, &platform) {
		return false
	}
	code := strings.ToUpper(strings.TrimSpace(platform.Code))
	message := strings.ToLower(strings.TrimSpace(platform.Message + "\n" + platform.Stderr))
	return code == "INVALID_STATE" && (platform.Status == 409 || platform.Status == 422) && containsAny(message,
		"cannot stop playground from current status",
		"already stopped",
		"current status stopped",
	)
}

func IsPlaygroundMissingError(err error) bool {
	var platform *PlatformError
	if !errors.As(err, &platform) {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(platform.Message + "\n" + platform.Stderr))
	if platform.Status == 404 {
		return !strings.Contains(message, "conversation") || strings.Contains(message, "playground")
	}
	return strings.Contains(message, "playground") && containsAny(message, "not found", "missing")
}

func (c *Client) runCLI(ctx context.Context, args []string, input any, out any) error {
	if strings.TrimSpace(c.cliPath) == "" {
		return &PlatformError{
			Code:    platformCodeCLINotConfigured,
			Message: "Fibe CLI path is not configured",
		}
	}
	fullArgs := append([]string{"--domain", c.cliDomain, "--api-key", c.apiKey, "--output", "json"}, args...)
	cmd := exec.CommandContext(ctx, c.cliPath, fullArgs...)
	cmd.Env = append(os.Environ(),
		"FIBE_DOMAIN="+c.cliDomain,
		"FIBE_API_KEY="+c.apiKey,
		"FIBE_OUTPUT=json",
		"NO_COLOR=1",
	)
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		cmd.Stdin = bytes.NewReader(data)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return parsePlatformError(stderr.String(), err)
	}
	if out == nil {
		return nil
	}
	data := bytes.TrimSpace(stdout.Bytes())
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

type cliErrorEnvelope struct {
	Error struct {
		Message string         `json:"message"`
		Code    string         `json:"code"`
		Status  int            `json:"status"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func parsePlatformError(stderr string, err error) error {
	clean := sanitizeCLIError(stderr, err)
	var payload cliErrorEnvelope
	if json.Unmarshal([]byte(clean), &payload) == nil && payload.Error.Message != "" {
		return &PlatformError{
			Code:    firstNonEmpty(payload.Error.Code, platformCodeUnknown),
			Status:  payload.Error.Status,
			Message: payload.Error.Message,
			Details: payload.Error.Details,
			Stderr:  clean,
			Cause:   err,
		}
	}

	code := platformCodeUnknown
	if errors.Is(err, exec.ErrNotFound) {
		code = platformCodeCLINotFound
	}
	return &PlatformError{
		Code:    code,
		Message: clean,
		Stderr:  clean,
		Cause:   err,
	}
}

func sanitizeCLIError(stderr string, err error) string {
	message := strings.TrimSpace(stderr)
	if message == "" {
		message = err.Error()
	}
	return strings.TrimSpace(strings.ReplaceAll(message, "\x00", ""))
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
