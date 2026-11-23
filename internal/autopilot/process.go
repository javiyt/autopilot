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
	c := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		c.Dir = dir
	}
	stdout, stderr := &limitedBuffer{limit: 2 << 20}, &limitedBuffer{limit: 2 << 20}
	c.Stdout = stdout
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, truncate(stderr.String(), 1200))
	}
	return stdout.String(), nil
}

// Prevent a noisy test command from exhausting the controller's own memory.
type limitedBuffer struct {
	data  bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.data.Len() < b.limit {
		keep := b.limit - b.data.Len()
		if len(p) < keep {
			keep = len(p)
		}
		_, _ = b.data.Write(p[:keep])
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

func ClassifyFailure(s string) string {
	if memoryErrors.MatchString(s) {
		return "memory/context"
	}
	if strings.Contains(strings.ToLower(s), "signal: killed") {
		return "process-killed (possible OOM)"
	}
	transient := regexp.MustCompile(`(?i)(timeout|timed.out|fetch.failed|ECONNREFUSED|EHOSTUNREACH|ECONNRESET|connection.reset|503|502|504|429|overloaded|socket.hang.up)`)
	if transient.MatchString(s) {
		return "connection/transient"
	}
	return "execution"
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
		Role       string `json:"role"`
		StopReason string `json:"stopReason"`
		Error      string `json:"errorMessage"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

type PiResult struct {
	Text        string
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
	var finalError string
	var stopReason string
	var sawSettled bool
	var wasAborted bool
	var readErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		reader := bufio.NewReader(stdout)
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
								}
							}
							if result.Text != "" {
								log.Printf("[pi %s] assistant: %s", role, truncate(strings.ReplaceAll(result.Text, "\n", " "), 160))
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
	mu.Lock()
	stderrText := stderrBuf.String()
	mu.Unlock()
	if idleExceeded {
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
	} else if strings.TrimSpace(result.Text) == "" {
		result.Err = errors.New("Pi returned no final assistant text")
	}
	if result.Err != nil {
		result.FailureKind = ClassifyFailure(result.Err.Error() + " " + stderrText + " " + finalError)
		result.Err = fmt.Errorf("%w (%s): %s", result.Err, result.FailureKind, truncate(stderrText+" "+finalError, 1300))
	}
	return result
}
