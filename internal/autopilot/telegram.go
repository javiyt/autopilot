package autopilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Telegram struct {
	token  string
	chatID int64
	client *http.Client
}

func NewTelegram(token string, chatID int64) *Telegram {
	return &Telegram{token: token, chatID: chatID, client: &http.Client{
		Timeout:       50 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (t *Telegram) Redact(s string) string { return strings.ReplaceAll(s, t.token, "[token hidden]") }

func (t *Telegram) request(ctx context.Context, method string, payload any, dest any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := "https://api.telegram.org/bot" + t.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("telegram request: %s", t.Redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram transport: %s", t.Redact(err.Error()))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram HTTP %d: %s", response.StatusCode, truncate(string(body), 500))
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return fmt.Errorf("telegram error: %s", envelope.Description)
	}
	if dest != nil {
		return json.Unmarshal(envelope.Result, dest)
	}
	return nil
}

func (t *Telegram) Send(ctx context.Context, message string) error {
	for len(message) > 0 {
		chunk := message
		if len(chunk) > 3500 {
			chunk = chunk[:3500]
			for !utf8.ValidString(chunk) && len(chunk) > 0 {
				chunk = chunk[:len(chunk)-1]
			}
		}
		if err := t.request(ctx, "sendMessage", map[string]any{"chat_id": t.chatID, "text": chunk, "disable_web_page_preview": true}, nil); err != nil {
			return err
		}
		message = message[len(chunk):]
	}
	return nil
}

type telegramUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		From struct {
			UserID int64 `json:"id"`
			IsBot  bool  `json:"is_bot"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

func (t *Telegram) Poll(ctx context.Context, app *App) {
	for ctx.Err() == nil {
		offset := app.snapshot().TelegramOffset
		pollCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		var updates []telegramUpdate
		err := t.request(pollCtx, "getUpdates", map[string]any{
			"offset": offset, "timeout": 30, "allowed_updates": []string{"message"},
		}, &updates)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("Telegram polling: %s", t.Redact(err.Error()))
			_ = wait(ctx, 5*time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID < offset {
				continue
			}
			// Consume even unauthorized messages, but NEVER execute or reply to them.
			if u.Message.Chat.ID == t.chatID && !u.Message.From.IsBot && strings.TrimSpace(u.Message.Text) != "" {
				app.HandleTelegram(ctx, u.Message.From.UserID, u.Message.Text)
			}
			if err := app.update(func(s *State) { s.TelegramOffset = u.UpdateID + 1 }); err != nil {
				log.Printf("Unable to persist Telegram offset: %v", err)
				break
			}
			offset = u.UpdateID + 1
		}
	}
}

func (t *Telegram) DiscoverChatIDs(ctx context.Context) ([]int64, error) {
	var updates []telegramUpdate
	if err := t.request(ctx, "getUpdates", map[string]any{"timeout": 0, "allowed_updates": []string{"message"}}, &updates); err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, u := range updates {
		id := u.Message.Chat.ID
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

func (a *App) HandleTelegram(ctx context.Context, userID int64, incoming string) {
	text := strings.TrimSpace(incoming)
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return
	}

	// Check if user is authorized
	if a.cfg.AuthorizedUserID > 0 && userID != a.cfg.AuthorizedUserID {
		_ = a.telegram.Send(ctx, "Access denied. Only authorized users can control this bot.")
		return
	}

	cmd := strings.SplitN(parts[0], "@", 2)[0]
	rest := strings.TrimSpace(strings.TrimPrefix(text, parts[0]))
	reply := func(s string) { a.notify(s) }
	switch cmd {
	case "/begin", "/start":
		if len(parts) < 2 {
			reply("Usage: /begin <repo> [milestone]")
			return
		}
		repo := parts[1]
		milestone := ""
		if len(parts) >= 3 {
			milestone = parts[2]
		}
		if err := a.startManualIssue(ctx, repo, milestone); err != nil {
			reply(err.Error())
			return
		}
		reply(fmt.Sprintf("Starting manual work on repo=%s, milestone=%s. The next issue(s) with label=%q will be processed.", repo, milestone, a.cfg.Label))
	case "/help":
		reply("Commands:\n/begin <repo> [milestone]: start manual work on specific repo/milestone\n/status: current issue\n/models: available models\n/use <investigate|plan|verify|implement|review|chat|triage> <number>: change model\n/chat: pause and enter read-only diagnostic chat (subsequent text is a question)\n/ask <question>: one read-only answer\n/exit: exit chat mode\n/fix <prompt>: send recovery instruction and retry blocked issue\n/retry: retry blocked phase\n/pause, /resume: pause/resume between stages\n/skip: skip current blocked issue")
	case "/status":
		reply(a.Status())
	case "/models":
		lines := []string{"Available models (use /use implement 2 or /use chat 3):"}
		for i, model := range a.cfg.AllowedModels {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, model))
		}
		reply(strings.Join(lines, "\n"))
	case "/use":
		if len(parts) < 3 {
			reply("Usage: /use <investigate|plan|implement|review|chat> <model number>")
			return
		}
		role := parts[1]
		if role != "investigate" && role != "plan" && role != "verify" && role != "implement" && role != "review" && role != "chat" && role != "triage" {
			reply("Unknown model role. Use investigate, plan, verify, implement, review, chat or triage. Non-LLM stages do not select a model.")
			return
		}
		index, err := strconv.Atoi(parts[2])
		if err != nil || index < 1 || index > len(a.cfg.AllowedModels) {
			reply("Invalid model number. Use /models.")
			return
		}
		model := a.cfg.AllowedModels[index-1]
		if err := a.update(func(s *State) { s.RoleModels[role] = model }); err != nil {
			reply(err.Error())
			return
		}
		reply(fmt.Sprintf("Model for %s set to %s. Applies to the next Pi session.", role, model))
	case "/pause":
		_ = a.update(func(s *State) { s.Paused = true })
		reply("Paused between stages. If Pi is currently running, it can finish its current stage.")
	case "/resume":
		_ = a.update(func(s *State) { s.Paused, s.ChatMode = false, false })
		reply("Resumed. A blocked phase still needs /retry or /fix.")
	case "/chat":
		_ = a.update(func(s *State) { s.Paused, s.ChatMode = true, true })
		reply("Diagnostic chat enabled. Once the current Pi stage has finished, send plain text to discuss the failure with your chosen chat model. Use /use chat <number>, /fix <prompt>, or /exit.")
	case "/exit":
		_ = a.update(func(s *State) { s.ChatMode = false })
		reply("Exited diagnostic chat. Use /resume to restart the workflow, or /fix if blocked.")
	case "/ask":
		if rest == "" {
			reply("Usage: /ask <question>")
			return
		}
		a.ask(ctx, rest)
	case "/retry":
		s := a.snapshot()
		if s.Phase != "blocked" {
			reply("No blocked stage to retry.")
			return
		}
		if err := a.update(func(s *State) {
			reblocked := s.BlockedPhase
			s.Phase = reblocked
			s.BlockedPhase = ""
			s.Attempts[reblocked] = 0
			s.OOMCount = 0
			s.Paused = false
			s.ChatMode = false
		}); err != nil {
			reply(err.Error())
			return
		}
		reply("Retrying the blocked phase. If it needs a model, a fresh Pi session will be launched; deterministic gates remain enforced.")
	case "/fix":
		if rest == "" {
			reply("Usage: /fix <your recovery instruction>")
			return
		}
		s := a.snapshot()
		if s.Phase != "blocked" {
			reply("Recovery prompts can be applied after the workflow is blocked. Use /pause and /chat to discuss while running.")
			return
		}
		switch s.BlockedPhase {
		case "repository_setup", "local_verify", "publish", "ci", "merge", "report":
			reply("This stage runs without an LLM. A prompt cannot override Git, tests, CI, merge authority or protection gates. Use /chat for diagnosis, correct the underlying issue or YAML, then /retry. For a code fix, return to implementation only through a verified workflow rework transition.")
			return
		}
		if err := a.update(func(s *State) {
			reblocked := s.BlockedPhase
			s.RecoveryPrompt = truncate(rest, 3800)
			s.Phase = reblocked
			s.BlockedPhase = ""
			s.Attempts[reblocked] = 0
			s.OOMCount = 0
			s.Paused, s.ChatMode = false, false
		}); err != nil {
			reply(err.Error())
			return
		}
		reply("Recovery instruction accepted. Restarting stage using the chosen model and existing files.")
	case "/skip":
		s := a.snapshot()
		if s.Phase != "blocked" || s.Issue.Number == 0 {
			reply("For safety, /skip is only available on a blocked issue.")
			return
		}
		if a.stageRunning.Load() {
			reply("Wait for the diagnostic chat to finish before skipping.")
			return
		}
		if err := a.skipIssue(s.Issue.Number); err != nil {
			reply(err.Error())
			return
		}
		if err := a.update(func(s *State) {
			roleModels, offset := s.RoleModels, s.TelegramOffset
			// Like complete(), the triage baseline outlives the skipped issue.
			triagedRepo, triagedSeen, triagedOrder := s.TriagedRepo, s.TriagedSeen, s.TriagedOrder
			*s = DefaultState(a.cfg)
			s.RoleModels, s.TelegramOffset = roleModels, offset
			s.TriagedRepo, s.TriagedSeen, s.TriagedOrder = triagedRepo, triagedSeen, triagedOrder
		}); err != nil {
			reply(err.Error())
			return
		}
		reply(fmt.Sprintf("Issue #%d skipped. Changes remain saved in the local checkout and logs.", s.Issue.Number))
	case "/reset":
		if err := a.resetManualConfig(); err != nil {
			reply(err.Error())
			return
		}
		reply(fmt.Sprintf("Manual config cleared. The app will resume auto-discover mode and look for issues with label=%q.", a.cfg.Label))
	default:
		if strings.HasPrefix(cmd, "/") {
			reply("Unknown command. Use /help.")
			return
		}
		if a.snapshot().ChatMode {
			a.ask(ctx, text)
			return
		}
		reply("Use /chat to ask diagnostic questions, or /fix <prompt> to recover a blocked issue.")
	}
}

func (a *App) ask(ctx context.Context, question string) {
	s := a.snapshot()
	if a.stageRunning.Load() {
		a.notify("The current Pi session is still running. Please wait for it to finish before starting a diagnostic chat.")
		return
	}
	if !s.Paused && s.Phase != "blocked" {
		a.notify("Pause the workflow first (/pause or /chat) to avoid two local models competing for memory.")
		return
	}
	if s.Issue.Number == 0 {
		a.notify("No active issue to discuss.")
		return
	}
	if len(question) > 3000 {
		a.notify("Question too long (max 3000 characters).")
		return
	}
	if !a.chatMu.TryLock() {
		a.notify("Still working on the previous question. Please wait.")
		return
	}
	a.notify("Thinking with " + a.chatModel(s) + " ...")
	go func() {
		defer a.chatMu.Unlock()
		chatCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		s.RoleModels["chat"] = a.chatModel(s)
		summary := a.readArtifact(s.Issue.Number, "plan.md")
		history := strings.Join(s.ChatHistory, "\n")
		prompt := "You are a READ-ONLY recovery assistant for a software engineering task. Do not edit or execute code. You may read files in the checkout to give accurate answers. Discuss diagnosis and suggest a concise recovery prompt for /fix if useful. Issue: " + s.Issue.URL + "\nPhase: " + s.Phase + "\nLast error: " + s.LastError + "\nPlan: " + truncate(summary, 6000) + "\nPrevious diagnostic chat:\n" + truncate(history, 6000) + "\nUser: " + question
		answer, err := a.runStage(chatCtx, s, "chat", prompt, true)
		if err != nil {
			a.notify("Diagnostic chat failed: " + truncate(err.Error(), 600))
			return
		}
		_ = a.update(func(st *State) {
			st.ChatHistory = append(st.ChatHistory, "User: "+question, "Assistant: "+truncate(answer, 1500))
			if len(st.ChatHistory) > 10 {
				st.ChatHistory = st.ChatHistory[len(st.ChatHistory)-10:]
			}
		})
		a.notify("💬 " + truncate(answer, 3400))
	}()
}
