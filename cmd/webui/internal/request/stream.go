package request

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/noodle05/ai-agents/tasklib"
)

// processStreamJSON reads and dispatches stream-json lines from stdout.
// It writes plan/tool_call messages for assistant output and response/error
// messages for the result. Returns true if a result message was processed.
func (h *Handler) processStreamJSON(ctx context.Context, threadID string, stdout io.Reader) (bool, tasklib.TokenStats) {
	completed := false
	lastWritten := ""
	var masterStats tasklib.TokenStats
	reader := bufio.NewReader(stdout)

	for {
		if h.isCancelled(ctx) {
			break
		}

		rawLine, readErr := reader.ReadString('\n')
		rawLine = strings.TrimRight(rawLine, "\r\n")
		line := []byte(rawLine)
		if len(line) == 0 {
			if readErr != nil {
				if readErr != io.EOF {
					h.logger.Info(fmt.Sprintf("thread=%s stdout reader error: %v", threadID, readErr))
				}
				break
			}
			continue
		}

		var msg streamMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			h.logger.Info(fmt.Sprintf("thread=%s unparseable stream-json line: %v", threadID, err))
			continue
		}

		switch msg.Type {
		case "system":
			continue

		case "user":
			continue

		case "assistant":
			if content := h.handleAssistantMessage(ctx, threadID, &msg); content != "" {
				lastWritten = content
			}

		case "result":
			completed = true
			if msg.Usage != nil {
				masterStats.InputTokens += msg.Usage.InputTokens
				masterStats.OutputTokens += msg.Usage.OutputTokens
				masterStats.CacheReadTokens += msg.Usage.CacheReadTokens
				masterStats.CacheWriteTokens += msg.Usage.CacheCreationTokens
			}
			if msg.IsError {
				errContent := msg.Result
				if errContent == "" {
					errContent = fmt.Sprintf("claude error: subtype=%s", msg.Subtype)
				}
				h.writeErrorMessage(ctx, threadID, errContent)
			} else if msg.Result == lastWritten {
				h.completeThread(ctx, threadID)
			} else {
				h.writeResponseMessage(ctx, threadID, msg.Result)
			}

		case "usage":
			if msg.Usage != nil {
				masterStats.InputTokens += msg.Usage.InputTokens
				masterStats.OutputTokens += msg.Usage.OutputTokens
				masterStats.CacheReadTokens += msg.Usage.CacheReadTokens
				masterStats.CacheWriteTokens += msg.Usage.CacheCreationTokens
			}
			continue
		}
	}
	return completed, masterStats
}

// processPlainText reads plain-text lines from stdout and accumulates them.
// Each non-empty line is written as a "plan" message for real-time UI updates.
// The caller handles the final response message after the process exits.
func (h *Handler) processPlainText(ctx context.Context, threadID string, stdout io.Reader, fullStdout *strings.Builder) {
	reader := bufio.NewReader(stdout)

	for {
		if h.isCancelled(ctx) {
			break
		}

		rawLine, readErr := reader.ReadString('\n')
		rawLine = strings.TrimRight(rawLine, "\r\n")
		if rawLine != "" {
			if fullStdout != nil {
				fullStdout.WriteString(rawLine)
				fullStdout.WriteByte('\n')
			}

			cleanCtx, cleanCancel := cleanupCtx()
			if err := h.history.AppendMessage(cleanCtx, threadID, tasklib.Message{
				Role:      h.cfg.AgentName,
				Type:      "plan",
				Content:   rawLine,
				Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			}); err != nil {
				h.logger.Warn(fmt.Sprintf("thread=%s AppendMessage error: %v", threadID, err))
			}
			cleanCancel()
		}
		if readErr != nil {
			if readErr != io.EOF {
				h.logger.Info(fmt.Sprintf("thread=%s stdout reader error: %v", threadID, readErr))
			}
			break
		}
	}
}

// handleAssistantMessage classifies assistant output as "plan" or "tool_call"
// and writes it to thread history for live progress display.
func (h *Handler) handleAssistantMessage(ctx context.Context, threadID string, msg *streamMessage) string {
	msgType := "plan"
	if msg.Message != nil && hasToolUse(msg.Message.Content) {
		msgType = "tool_call"
	}

	text := extractText(msg)
	if text == "" {
		return ""
	}

	cleanCtx, cleanCancel := cleanupCtx()
	defer cleanCancel()
	h.history.AppendMessage(cleanCtx, threadID, tasklib.Message{
		Role:      h.cfg.AgentName,
		Type:      msgType,
		Content:   text,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
	return text
}

// streamMessage represents a single JSON line from claude --output-format stream-json.
type streamMessage struct {
	Type    string               `json:"type"`
	Subtype string               `json:"subtype"`
	IsError bool                 `json:"is_error"`
	Result  string               `json:"result"`
	Message *streamAssistant     `json:"message"`
	Usage   *tasklib.ClaudeUsage `json:"usage,omitempty"`
}

type streamAssistant struct {
	Content []streamContentBlock `json:"content"`
}

type streamContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// hasToolUse returns true if any content block is a tool_use.
func hasToolUse(blocks []streamContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// extractText concatenates all text blocks from an assistant message.
func extractText(msg *streamMessage) string {
	if msg.Message == nil {
		return ""
	}
	var parts []string
	for _, b := range msg.Message.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
