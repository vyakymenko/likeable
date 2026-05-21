package likeable

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	fibegateway "github.com/fibegg/likeable/internal/fibe"
	"github.com/google/uuid"
)

const (
	promptImproveStart = "[[LIKEABLE_PROMPT_IMPROVE_START]]"
	promptImproveEnd   = "[[LIKEABLE_PROMPT_IMPROVE_END]]"
	promptImproveWait  = 55 * time.Second
)

func (s *Server) handleProjectPromptImprove(w http.ResponseWriter, r *http.Request, user *User, project *Project) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Text   string `json:"text"`
		Locale string `json:"locale"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	improved, err := s.improvePromptWithAgent(r.Context(), user, project, body.Text, body.Locale)
	if err != nil {
		log.Printf("agent prompt improve failed for project %s: %v", project.ID, err)
		writeJSON(w, http.StatusOK, map[string]any{
			"text":    fallbackImprovedPrompt(body.Text, project.Title, body.Locale),
			"source":  "fallback",
			"warning": "prompt improve agent is unavailable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": improved, "source": "agent"})
}

func (s *Server) improvePromptWithAgent(ctx context.Context, user *User, project *Project, draft, locale string) (string, error) {
	if user == nil || project == nil {
		return "", fmt.Errorf("project context is required")
	}
	client, err := s.fibeClientForProject(ctx, project, user.Email)
	if err != nil {
		return "", err
	}
	conversationID := "likeable-prompt-improve-" + uuid.NewString()
	ctx, cancel := context.WithTimeout(ctx, promptImproveWait)
	defer cancel()
	if err := client.EnsureConversation(ctx, conversationID, "Likeable prompt improve"); err != nil {
		return "", err
	}
	request := promptImproveRequest(project, draft, locale)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := client.DeleteConversation(cleanupCtx, conversationID); err != nil {
			log.Printf("delete prompt improve conversation %s: %v", conversationID, err)
		}
	}()
	if err := client.SendMessage(ctx, conversationID, request, nil, "reject"); err != nil {
		if fibegateway.IsAgentRuntimeUnavailableError(err) {
			if startErr := s.startProjectAgentChat(ctx, project, client, "prompt improve"); startErr != nil {
				return "", startErr
			}
			if retryErr := client.SendMessage(ctx, conversationID, request, nil, "reject"); retryErr == nil {
				goto poll
			} else {
				return "", retryErr
			}
		}
		return "", err
	}
poll:
	ticker := time.NewTicker(900 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			messages, err := client.Messages(ctx, conversationID)
			if err != nil {
				return "", err
			}
			if improved := extractPromptImprovement(messages); improved != "" {
				return improved, nil
			}
		}
	}
}

func promptImproveRequest(project *Project, draft, locale string) string {
	title := ""
	service := ""
	if project != nil {
		title = strings.TrimSpace(project.Title)
		service = strings.TrimSpace(project.SelectedService)
	}
	draft = strings.TrimSpace(draft)
	if draft == "" {
		draft = "Improve the current app."
	}
	preferredLanguage := promptImprovePreferredLanguage(locale)
	return fmt.Sprintf(`You are Likeable's prompt-improvement agent.

Task:
- Rewrite the user's draft into one stronger prompt for a coding/build agent.
- Do not edit files, do not run tools, do not build, and do not ask follow-up questions.
- Keep the solution universal: do not add domain-specific details unless they are present in the draft or current app context.
- Preserve the current app/domain unless the draft explicitly asks to replace it.
- Keep the user's language when it is clear; otherwise use the preferred UI language.
- Make the prompt specific about outcome, UX, responsive behavior, states, and verification.
- Return only the improved prompt wrapped exactly between:
%s
%s

Preferred UI language: %s
Current app title: %s
Selected service: %s
User draft:
%s`, promptImproveStart, promptImproveEnd, preferredLanguage, title, service, draft)
}

func promptImprovePreferredLanguage(locale string) string {
	locale = strings.ToLower(strings.TrimSpace(locale))
	switch {
	case strings.HasPrefix(locale, "uk"):
		return "Ukrainian"
	case strings.HasPrefix(locale, "ru"):
		return "Russian"
	default:
		return "English"
	}
}

func extractPromptImprovement(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(fmt.Sprint(message["role"])))
		if role != "assistant" {
			continue
		}
		body := strings.TrimSpace(fmt.Sprint(message["body"]))
		if body == "" || body == "<nil>" {
			continue
		}
		if extracted := betweenMarkers(body, promptImproveStart, promptImproveEnd); extracted != "" {
			return extracted
		}
	}
	return ""
}

func betweenMarkers(value, start, end string) string {
	startIndex := strings.Index(value, start)
	if startIndex < 0 {
		return ""
	}
	contentStart := startIndex + len(start)
	endIndex := strings.Index(value[contentStart:], end)
	if endIndex < 0 {
		return ""
	}
	return strings.TrimSpace(value[contentStart : contentStart+endIndex])
}

func fallbackImprovedPrompt(draft, projectTitle string, locale ...string) string {
	draft = strings.TrimSpace(strings.Join(strings.Fields(draft), " "))
	context := "current app"
	if title := strings.TrimSpace(projectTitle); title != "" {
		context = fmt.Sprintf("current %q app", title)
	}
	language := fallbackPromptLanguage(draft, firstNonEmpty(locale...))
	if language == "uk" {
		ukContext := "поточний застосунок"
		if title := strings.TrimSpace(projectTitle); title != "" {
			ukContext = fmt.Sprintf("поточний застосунок %q", title)
		}
		if draft == "" {
			return fmt.Sprintf("Покращ %s. Збережи наявний продукт і домен, посиль основний користувацький сценарій, відполіруй адаптивний UI та виправ очевидні візуальні або інтеракційні проблеми. Не замінюй застосунок на непов'язаний продукт.", ukContext)
		}
		return strings.Join([]string{
			fmt.Sprintf("Покращ %s.", ukContext),
			"Запитана зміна: " + draft + ".",
			"Залишайся в тому самому продукті й домені та розвивай наявний застосунок; не замінюй його на непов'язаний продукт.",
			"Доведи результат до запускової якості: чітка ієрархія, адаптивні стани, корисні empty/loading/error стани, без накладання тексту або контролів.",
			"Збережи робочу функціональність, якщо запит явно не вимагає іншого, потім запусти build/app і виправ видимі проблеми перед завершенням.",
		}, "\n")
	}
	if language == "ru" {
		ruContext := "текущее приложение"
		if title := strings.TrimSpace(projectTitle); title != "" {
			ruContext = fmt.Sprintf("текущее приложение %q", title)
		}
		if draft == "" {
			return fmt.Sprintf("Улучши %s. Сохрани текущий продукт и домен, усили основной пользовательский сценарий, отполируй адаптивный UI и исправь очевидные визуальные или интеракционные проблемы. Не заменяй приложение на несвязанный продукт.", ruContext)
		}
		return strings.Join([]string{
			fmt.Sprintf("Улучши %s.", ruContext),
			"Запрошенное изменение: " + draft + ".",
			"Оставайся в том же продукте и домене, развивай существующее приложение; не заменяй его на несвязанный продукт.",
			"Доведи результат до запускового качества: четкая иерархия, адаптивные состояния, полезные empty/loading/error состояния, без наложения текста или контролов.",
			"Сохрани рабочую функциональность, если запрос явно не требует другого, затем запусти build/app и исправь видимые проблемы перед завершением.",
		}, "\n")
	}
	if draft == "" {
		return fmt.Sprintf("Improve the %s. Keep the existing product/domain intact, tighten the main user flow, polish the responsive UI, and fix any obvious visual or interaction issues. Do not replace it with an unrelated app.", context)
	}
	return strings.Join([]string{
		fmt.Sprintf("Improve the %s.", context),
		"Requested change: " + draft + ".",
		"Stay in the same product/domain and build on the existing app; do not replace it with an unrelated app.",
		"Make the result production-polished: clear layout hierarchy, responsive states, useful empty/loading/error states, and no overlapping text or controls.",
		"Preserve existing working functionality unless the requested change explicitly says otherwise, then run the app/build and fix visible issues before finishing.",
	}, "\n")
}

func fallbackPromptLanguage(draft, locale string) string {
	locale = strings.ToLower(strings.TrimSpace(locale))
	if strings.HasPrefix(locale, "uk") {
		return "uk"
	}
	if containsCyrillic(draft) {
		return "ru"
	}
	return "en"
}

func containsCyrillic(value string) bool {
	for _, r := range value {
		if r >= '\u0400' && r <= '\u04ff' {
			return true
		}
	}
	return false
}
