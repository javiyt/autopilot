package autopilot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func command(ctx context.Context, dir, name string, args ...string) (string, error) {
	out, _, err := commandCapped(ctx, dir, name, 2<<20, args...)
	return out, err
}

// commandWithEnv runs a command with an explicit environment (managed Pi
// variables) instead of the inherited one.
func commandWithEnv(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		c.Dir = dir
	}
	if env != nil {
		c.Env = env
	}
	stdout, stderr := &limitedBuffer{limit: 2 << 20}, &limitedBuffer{limit: 2 << 20}
	c.Stdout = stdout
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, truncate(stderr.String(), 1200))
	}
	return stdout.String(), nil
}

// commandCapped runs a command and captures stdout up to limit bytes. The
// returned truncated flag lets callers distinguish complete output from a
// capped capture instead of silently reviewing partial data.
func commandCapped(ctx context.Context, dir, name string, limit int, args ...string) (string, bool, error) {
	c := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		c.Dir = dir
	}
	stdout, stderr := &limitedBuffer{limit: limit}, &limitedBuffer{limit: 2 << 20}
	c.Stdout = stdout
	c.Stderr = stderr
	runErr := c.Run()
	if runErr != nil {
		return stdout.String(), stdout.hit, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), runErr, truncate(stderr.String(), 1200))
	}
	return stdout.String(), stdout.hit, nil
}

// commandFull captures the complete stdout of a command (bounded at 64 MiB)
// for callers whose output legitimately exceeds the 2 MiB cap of command,
// such as a git diff over a multi-megabyte generated file. A hit limit is
// reported as truncation so the caller fails loudly.
func commandFull(ctx context.Context, dir, name string, args ...string) (string, bool, error) {
	return commandCapped(ctx, dir, name, 64<<20, args...)
}

// Prevent a noisy test command from exhausting the controller's own memory.
type limitedBuffer struct {
	data  bytes.Buffer
	limit int
	hit   bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.data.Len() < b.limit {
		keep := b.limit - b.data.Len()
		if len(p) < keep {
			keep = len(p)
		}
		_, _ = b.data.Write(p[:keep])
	} else {
		b.hit = true
	}
	return n, nil
}

func (b *limitedBuffer) String() string { return b.data.String() }

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "... [truncated]"
}

var memoryErrors = regexp.MustCompile(`(?i)(out.of.memory|\boom\b|memory pressure|cannot allocate memory|metal allocation|kv.cache|context.length|context.window|context.size|too.many.tokens|maximum.context|token.limit|failed.to.compact|compaction.failed|prefill.guard|exceeds.*context|length.*exceeded)`)

// ClassifyFailure inspects failure text and returns a failure kind. This
// regex is the seam to extend when a new Pi/provider error signature for a
// real connection or memory failure appears: unknown text classifies as
// "execution", which keeps today's retry→block behavior (a stage can fail
// but cannot stall forever).
func ClassifyFailure(s string) string {
	if memoryErrors.MatchString(s) {
		return "memory/context"
	}
	if strings.Contains(strings.ToLower(s), "signal: killed") {
		return "process-killed (possible OOM)"
	}
	transient := regexp.MustCompile(`(?i)(timeout|timed.out|fetch.failed|ECONNREFUSED|EHOSTUNREACH|ECONNRESET|connection.reset|connection.refused|503|502|504|429|overloaded|socket.hang.up)`)
	if transient.MatchString(s) {
		return "connection/transient"
	}
	return "execution"
}

type messageBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// loopTurnKey is the loop-detection key of one assistant turn: its text plus
// the name and arguments of every tool call. A repeating preamble over
// different reads produces different keys (progress); a turn is part of a
// loop when turnCycle finds its fixed sequence repeating. A text-less,
// call-less turn has an empty key and never counts.
func loopTurnKey(text string, blocks []messageBlock) string {
	key := text
	for _, b := range blocks {
		if b.Type == "toolCall" {
			key += "\x00" + b.Name + " " + string(b.Arguments)
		}
	}
	if key == "" {
		return ""
	}
	return key
}

// turnCycle reports a fixed repetition inside the recent turn keys: the
// smallest period p (1..4) for which the last max(8, 3*p) keys all repeat
// with period p. p=1 is the identical action repeated 8 consecutive times;
// p=2..4 catches cycles such as three different greps repeating in order,
// which a consecutive-identical counter keeps resetting on. A window shorter
// than the requirement yields no match, so a short session never trips the
// detector.
func turnCycle(recent []string) (period, window int) {
	for p := 1; p <= 4; p++ {
		w := 8
		if 3*p > w {
			w = 3 * p
		}
		if len(recent) < w {
			continue
		}
		start := len(recent) - w
		cycle := true
		for i := start + p; i < len(recent); i++ {
			if recent[i] != recent[i-p] {
				cycle = false
				break
			}
		}
		if cycle {
			return p, w
		}
	}
	return 0, 0
}

type piEvent struct {
	Type         string          `json:"type"`
	Aborted      bool            `json:"aborted"`
	ErrorMessage string          `json:"errorMessage"`
	FinalError   string          `json:"finalError"`
	Success      *bool           `json:"success"`
	ToolName     string          `json:"toolName"`
	Args         json.RawMessage `json:"args"`
	Result       struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Message struct {
		Role       string         `json:"role"`
		StopReason string         `json:"stopReason"`
		Error      string         `json:"errorMessage"`
		Content    []messageBlock `json:"content"`
	} `json:"message"`
}

type PiResult struct {
	// Text is the last assistant message: the stage's answer.
	Text string
	// FullText is every assistant text block of the session in order:
	// the complete record a stage produced across its tool turns.
	FullText    string
	Err         error
	FailureKind string
	LogPath     string
}

// RunPi consumes JSONL continuously, including during large tool outputs. A run is
// successful only after agent_settled, not merely agent_end (which can retry).
func RunPi(parent context.Context, cfg Config, cwd, role, provider, model, thinking, prompt, logPath string, readOnly bool) PiResult {
	result := PiResult{LogPath: logPath}
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		result.Err = err
		return result
	}
	sess, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		result.Err = err
		return result
	}
	defer sess.Close()

	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.StageTimeoutMinutes)*time.Minute)
	defer cancel()
	// Disable globally installed extensions/MCP to avoid plan-mode state bleed and
	// unexpected write-capable tools in the read-only reviewer/chat sessions.
	args := []string{"--mode", "json", "--no-session", "--no-extensions", "--no-mcp", "--no-skills", "--no-prompt-templates", "--provider", provider, "--model", model, "--thinking", thinking}
	if readOnly {
		args = append(args, "--tools", "read,grep,find,ls")
	} else {
		args = append(args, "--tools", "read,grep,find,ls,bash,edit,write")
	}
	args = append(args, "--", prompt)
	log.Printf("Pi session starting: role=%s provider=%s model=%s thinking=%s (log: %s)", role, provider, model, thinking, logPath)
	c := exec.CommandContext(ctx, "pi", args...)
	c.Dir = cwd
	// Run against the managed agent/session directories from config.yaml; the
	// user's global ~/.pi/agent is never touched. Managed variables win over
	// inherited environment values.
	c.Env = piChildEnv(cfg)
	stdout, err := c.StdoutPipe()
	if err != nil {
		result.Err = err
		return result
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		result.Err = err
		return result
	}
	if err := c.Start(); err != nil {
		result.Err = err
		return result
	}
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	var mu sync.Mutex
	var stderrBuf bytes.Buffer
	var fullText strings.Builder
	var finalError string
	var stopReason string
	var sawSettled bool
	var wasAborted bool
	var readErr error
	var loopDetected atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader := bufio.NewReader(stdout)
		var recentTurns []string
		abortLoop := func(desc string) {
			if !loopDetected.Load() {
				loopDetected.Store(true)
				log.Printf("[pi %s] repetition loop detected (%s); aborting session", role, desc)
				cancel()
			}
		}
		for {
			line, e := reader.ReadBytes('\n')
			if len(line) > 0 {
				lastProgress.Store(time.Now().UnixNano())
				_, _ = sess.Write(line)
				var event piEvent
				if json.Unmarshal(bytes.TrimSpace(line), &event) == nil {
					switch event.Type {
					case "tool_execution_start":
						log.Printf("[pi %s] -> tool %s %s", role, event.ToolName, truncate(strings.ReplaceAll(string(event.Args), "\n", " "), 140))
					case "tool_execution_end":
						n := 0
						for _, block := range event.Result.Content {
							n += len(block.Text)
						}
						log.Printf("[pi %s] <- tool %s finished (%d bytes)", role, event.ToolName, n)
					case "message_end":
						if event.Message.Role == "assistant" {
							stopReason = event.Message.StopReason
							if event.Message.Error != "" {
								finalError = event.Message.Error
							}
							for _, block := range event.Message.Content {
								if block.Type == "text" && block.Text != "" {
									result.Text = block.Text
									fullText.WriteString(block.Text)
									fullText.WriteString("\n")
								}
							}
							if result.Text != "" {
								log.Printf("[pi %s] assistant: %s", role, truncate(strings.ReplaceAll(result.Text, "\n", " "), 160))
							}
							// A loop is a repeated ACTION SEQUENCE: the same text plus the
							// same tool calls, repeated one by one (identical turns) or as
							// a fixed cycle of up to 4 turns (different reads repeating in
							// order). Repeating a preamble sentence across different reads
							// is normal batching, not a loop; a bare thinking-only turn
							// carries no key and never counts.
							action := loopTurnKey(result.Text, event.Message.Content)
							if action != "" {
								recentTurns = append(recentTurns, action)
								if len(recentTurns) > 12 {
									recentTurns = recentTurns[len(recentTurns)-12:]
								}
								if p, w := turnCycle(recentTurns); p != 0 {
									abortLoop(fmt.Sprintf("%d assistant turns repeating with period %d", w, p))
								}
							}
						}
					case "compaction_end":
						if event.ErrorMessage != "" {
							finalError = event.ErrorMessage
						}
					case "auto_retry_end":
						if event.Success != nil && !*event.Success {
							finalError = event.FinalError
						}
					case "agent_settled":
						sawSettled, wasAborted = true, event.Aborted
					}
				}
			}
			if e != nil {
				// file already closed is expected when process exits cleanly after emitting events;
				// io.EOF is the normal stream end once the child closes the write side of the pipe.
				if errors.Is(e, io.EOF) || strings.Contains(e.Error(), "file already closed") {
					return
				}
				readErr = e
				return
			}
		}
	}()
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		r := bufio.NewReader(stderr)
		for {
			line, e := r.ReadBytes('\n')
			if len(line) > 0 {
				lastProgress.Store(time.Now().UnixNano())
				mu.Lock()
				if stderrBuf.Len() < 16000 {
					stderrBuf.Write(line)
				}
				mu.Unlock()
			}
			if e != nil {
				return
			}
		}
	}()
	idleExceeded := false
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastProgress.Load())) > time.Duration(cfg.IdleTimeoutMinutes)*time.Minute {
					idleExceeded = true
					cancel()
					return
				}
			}
		}
	}()
	waitErr := c.Wait()
	<-done
	<-stderrDone
	<-monitorDone
	result.FullText = fullText.String()
	mu.Lock()
	stderrText := stderrBuf.String()
	mu.Unlock()
	if loopDetected.Load() {
		result.Err = fmt.Errorf("repetition loop detected: the assistant repeated a fixed sequence of turns; session aborted (log: %s)", logPath)
	} else if idleExceeded {
		result.Err = errors.New("Pi did not emit output within the idle timeout")
	} else if ctx.Err() != nil {
		result.Err = fmt.Errorf("Pi stopped: %w", ctx.Err())
	} else if waitErr != nil {
		result.Err = fmt.Errorf("Pi process failed: %w", waitErr)
	} else if readErr != nil {
		result.Err = fmt.Errorf("Pi JSON stream failed: %w", readErr)
	} else if !sawSettled || wasAborted {
		result.Err = errors.New("Pi ended without a successful agent_settled event")
	} else if stopReason == "error" || stopReason == "aborted" {
		result.Err = errors.New("Pi final assistant message reported a failure")
	} else if stopReason == "length" {
		// A final message cut off at the output token limit is incomplete
		// stage output; accepting it would let a truncated report or a
		// missing required marker pass as a finished stage.
		result.Err = errors.New("Pi final assistant message was cut off at the output token limit (stopReason=length); the stage output is incomplete")
	} else if strings.TrimSpace(result.Text) == "" {
		result.Err = errors.New("Pi returned no final assistant text")
	}
	if result.Err != nil {
		kind := result.FailureKind
		if kind == "" {
			kind = ClassifyFailure(result.Err.Error() + " " + stderrText + " " + finalError)
		}
		result.FailureKind = kind
		result.Err = fmt.Errorf("%w (%s): %s", result.Err, result.FailureKind, truncate(stderrText+" "+finalError, 1300))
	}
	return result
}
