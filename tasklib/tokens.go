package tasklib

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"strconv"
	"os"
	"path/filepath"

	"github.com/redis/go-redis/v9"
)

// ── StatsProvider interface ──────────────────────────────────────────────────

// TokenStats holds extracted token usage counts for a single task or session.
type TokenStats struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// HasAny returns true if any token field is non-zero.
func (ts TokenStats) HasAny() bool {
	return ts.InputTokens > 0 || ts.OutputTokens > 0 ||
		ts.CacheReadTokens > 0 || ts.CacheWriteTokens > 0 ||
		ts.ReasoningTokens > 0
}

// StatsProvider extracts token usage from agent output. Each agent type
// (claude, codex, opencode, copilot) has its own implementation.
type StatsProvider interface {
	// Setup runs BEFORE the agent command. Returns extra CLI args to append
	// and an optional cleanup function.
	Setup(workspaceDir string) (extraArgs []string, cleanup func(), err error)

	// Process runs AFTER the agent command. Takes raw stdout, returns clean
	// content (no JSONL/NDJSON metadata) and extracted token stats.
	Process(workspaceDir string, stdout string) (content string, stats TokenStats, err error)
}

// ── NoopStatsProvider ────────────────────────────────────────────────────────

// NoopStatsProvider returns zero stats and raw stdout as-is. Used for
// unknown agent types.
type NoopStatsProvider struct{}

func (p *NoopStatsProvider) Setup(workspaceDir string) ([]string, func(), error) {
	return nil, nil, nil
}

func (p *NoopStatsProvider) Process(workspaceDir, stdout string) (string, TokenStats, error) {
	return stdout, TokenStats{}, nil
}

// ── ClaudeStatsProvider ──────────────────────────────────────────────────────

// ClaudeStatsProvider parses NDJSON output from claude --output-format stream-json --verbose.
// Used by master-agent and worker-claude.
type ClaudeStatsProvider struct{}

func (p *ClaudeStatsProvider) Setup(workspaceDir string) ([]string, func(), error) {
	// --output-format stream-json --verbose already in AGENT_CMD
	return nil, nil, nil
}

func (p *ClaudeStatsProvider) Process(workspaceDir, stdout string) (string, TokenStats, error) {
	return extractClaudeTokens(stdout)
}

// ── CodexStatsProvider ───────────────────────────────────────────────────────

// CodexStatsProvider parses JSONL output from codex --json.
type CodexStatsProvider struct{}

func (p *CodexStatsProvider) Setup(workspaceDir string) ([]string, func(), error) {
	// --json already in AGENT_CMD
	return nil, nil, nil
}

func (p *CodexStatsProvider) Process(workspaceDir, stdout string) (string, TokenStats, error) {
	return extractCodexTokens(stdout)
}

// ── OpenCodeStatsProvider ────────────────────────────────────────────────────

// OpenCodeStatsProvider parses JSONL output from opencode --format json.
type OpenCodeStatsProvider struct{}

func (p *OpenCodeStatsProvider) Setup(workspaceDir string) ([]string, func(), error) {
	// --format json already in AGENT_CMD
	return nil, nil, nil
}

func (p *OpenCodeStatsProvider) Process(workspaceDir, stdout string) (string, TokenStats, error) {
	return extractOpenCodeTokens(stdout)
}

// ── CopilotStatsProvider ─────────────────────────────────────────────────────

// copilotSessionDir is the base directory where Copilot stores per-session state.
// Overridable in tests.
var copilotSessionDir = "/home/agent/.copilot/session-state"

// CopilotStatsProvider generates a session UUID in Setup and reads the
// session events.jsonl sidecar file in Process.
type CopilotStatsProvider struct {
	sessionID string
}

func (p *CopilotStatsProvider) Setup(workspaceDir string) ([]string, func(), error) {
	id, err := NewUUID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate copilot session id: %w", err)
	}
	p.sessionID = id

	cleanup := func() {
		dir := filepath.Join(copilotSessionDir, p.sessionID)
		os.RemoveAll(dir)
	}

	return []string{"--session-id", id}, cleanup, nil
}

func (p *CopilotStatsProvider) Process(workspaceDir, stdout string) (string, TokenStats, error) {
	// stdout is plain text — no filtering needed
	var stats TokenStats
	sessionFile := filepath.Join(copilotSessionDir, p.sessionID, "events.jsonl")
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		// File missing is not fatal — return zero stats
		return stdout, stats, nil
	}
	stats = parseCopilotSessionFile(string(data))
	return stdout, stats, nil
}

// ── NewStatsProvider selects the correct provider per agent type ─────────────

// NewStatsProvider returns the StatsProvider for the given agent type.
// Unknown types get a NoopStatsProvider.
func NewStatsProvider(agentType string) StatsProvider {
	switch agentType {
	case "claude":
		return &ClaudeStatsProvider{}
	case "codex":
		return &CodexStatsProvider{}
	case "opencode":
		return &OpenCodeStatsProvider{}
	case "copilot":
		return &CopilotStatsProvider{}
	default:
		return &NoopStatsProvider{}
	}
}

// ── Persistent counter keys ──────────────────────────────────────────────────

// StatsTotalKey returns the Redis key for the global aggregate token counter.
func StatsTotalKey() string { return "stats:total_tokens" }

// StatsWorkerKey returns the Redis key for a per-worker-type token counter.
func StatsWorkerKey(workerType string) string { return "stats:total_tokens:" + workerType }

// PersistTokenStats writes token counts to persistent global counters and
// per-task keys via an active Redis pipeline. The caller must Exec the pipeline.
func PersistTokenStats(ctx context.Context, pipe redis.Pipeliner, taskID, workerType string, stats TokenStats) {
	totalKey := StatsTotalKey()
	workerKey := StatsWorkerKey(workerType)

	pipe.HIncrBy(ctx, totalKey, "input_tokens", stats.InputTokens)
	pipe.HIncrBy(ctx, totalKey, "output_tokens", stats.OutputTokens)
	pipe.HIncrBy(ctx, totalKey, "cache_read", stats.CacheReadTokens)
	pipe.HIncrBy(ctx, totalKey, "cache_write", stats.CacheWriteTokens)
	pipe.HIncrBy(ctx, totalKey, "reasoning", stats.ReasoningTokens)
	pipe.HIncrBy(ctx, totalKey, "task_count", 1)

	pipe.HIncrBy(ctx, workerKey, "input_tokens", stats.InputTokens)
	pipe.HIncrBy(ctx, workerKey, "output_tokens", stats.OutputTokens)
	pipe.HIncrBy(ctx, workerKey, "cache_read", stats.CacheReadTokens)
	pipe.HIncrBy(ctx, workerKey, "cache_write", stats.CacheWriteTokens)
	pipe.HIncrBy(ctx, workerKey, "reasoning", stats.ReasoningTokens)
	pipe.HIncrBy(ctx, workerKey, "task_count", 1)

	// Per-task token fields (only non-zero to save space)
	if stats.InputTokens > 0 {
		pipe.Set(ctx, TaskKey(taskID, "input_tokens"), stats.InputTokens, TTLTask)
	}
	if stats.OutputTokens > 0 {
		pipe.Set(ctx, TaskKey(taskID, "output_tokens"), stats.OutputTokens, TTLTask)
	}
	if stats.CacheReadTokens > 0 {
		pipe.Set(ctx, TaskKey(taskID, "cache_read_tokens"), stats.CacheReadTokens, TTLTask)
	}
	if stats.CacheWriteTokens > 0 {
		pipe.Set(ctx, TaskKey(taskID, "cache_write_tokens"), stats.CacheWriteTokens, TTLTask)
	}
	if stats.ReasoningTokens > 0 {
		pipe.Set(ctx, TaskKey(taskID, "reasoning_tokens"), stats.ReasoningTokens, TTLTask)
	}
}

// PersistMasterTokenStats writes master agent token counts to thread-level
// fields AND global counters via an active Redis pipeline.
func PersistMasterTokenStats(ctx context.Context, pipe redis.Pipeliner, threadID string, stats TokenStats) {
	totalKey := StatsTotalKey()
	masterKey := StatsWorkerKey("master")

	pipe.HIncrBy(ctx, totalKey, "input_tokens", stats.InputTokens)
	pipe.HIncrBy(ctx, totalKey, "output_tokens", stats.OutputTokens)
	pipe.HIncrBy(ctx, totalKey, "cache_read", stats.CacheReadTokens)
	pipe.HIncrBy(ctx, totalKey, "cache_write", stats.CacheWriteTokens)
	pipe.HIncrBy(ctx, totalKey, "reasoning", stats.ReasoningTokens)
	pipe.HIncrBy(ctx, totalKey, "task_count", 1)

	pipe.HIncrBy(ctx, masterKey, "input_tokens", stats.InputTokens)
	pipe.HIncrBy(ctx, masterKey, "output_tokens", stats.OutputTokens)
	pipe.HIncrBy(ctx, masterKey, "cache_read", stats.CacheReadTokens)
	pipe.HIncrBy(ctx, masterKey, "cache_write", stats.CacheWriteTokens)
	pipe.HIncrBy(ctx, masterKey, "reasoning", stats.ReasoningTokens)
	pipe.HIncrBy(ctx, masterKey, "task_count", 1)

	// Thread-level master token fields
	key := ThreadStateKey(threadID)
	pipe.HIncrBy(ctx, key, "master_input_tokens", stats.InputTokens)
	pipe.HIncrBy(ctx, key, "master_output_tokens", stats.OutputTokens)
	pipe.HIncrBy(ctx, key, "master_cache_read_tokens", stats.CacheReadTokens)
	pipe.HIncrBy(ctx, key, "master_cache_write_tokens", stats.CacheWriteTokens)
	pipe.HIncrBy(ctx, key, "master_reasoning_tokens", stats.ReasoningTokens)
}

// GetTokenStats reads the global token stats hash (total or per-worker).
func (c *Client) GetTokenStats(ctx context.Context, key string) (*TokenStats, error) {
	fields, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return &TokenStats{}, nil
	}
	return &TokenStats{
		InputTokens:      parseFieldInt(fields, "input_tokens"),
		OutputTokens:     parseFieldInt(fields, "output_tokens"),
		CacheReadTokens:  parseFieldInt(fields, "cache_read"),
		CacheWriteTokens: parseFieldInt(fields, "cache_write"),
		ReasoningTokens:  parseFieldInt(fields, "reasoning"),
	}, nil
}

// GetTokenStatsTaskCount reads the task_count field from a token stats hash.
func (c *Client) GetTokenStatsTaskCount(ctx context.Context, key string) (int64, error) {
	v, err := c.rdb.HGet(ctx, key, "task_count").Int64()
	if err != nil {
		return 0, nil
	}
	return v, nil
}

// GetMasterTokenStats reads the master agent token fields from a thread.
func (c *Client) GetMasterTokenStats(ctx context.Context, threadID string) (TokenStats, error) {
	fields, err := c.rdb.HGetAll(ctx, ThreadStateKey(threadID)).Result()
	if err != nil {
		return TokenStats{}, err
	}
	return TokenStats{
		InputTokens:      parseFieldInt(fields, "master_input_tokens"),
		OutputTokens:     parseFieldInt(fields, "master_output_tokens"),
		CacheReadTokens:  parseFieldInt(fields, "master_cache_read_tokens"),
		CacheWriteTokens: parseFieldInt(fields, "master_cache_write_tokens"),
		ReasoningTokens:  parseFieldInt(fields, "master_reasoning_tokens"),
	}, nil
}

// GetTaskTokenStats reads per-task token fields from Redis.
func (c *Client) GetTaskTokenStats(ctx context.Context, taskID string) (TokenStats, error) {
	pipe := c.rdb.Pipeline()
	input := pipe.Get(ctx, TaskKey(taskID, "input_tokens"))
	output := pipe.Get(ctx, TaskKey(taskID, "output_tokens"))
	cacheRead := pipe.Get(ctx, TaskKey(taskID, "cache_read_tokens"))
	cacheWrite := pipe.Get(ctx, TaskKey(taskID, "cache_write_tokens"))
	reasoning := pipe.Get(ctx, TaskKey(taskID, "reasoning_tokens"))
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		// Ignore individual key misses
	}

	ts := TokenStats{}
	if v, err := input.Int64(); err == nil {
		ts.InputTokens = v
	}
	if v, err := output.Int64(); err == nil {
		ts.OutputTokens = v
	}
	if v, err := cacheRead.Int64(); err == nil {
		ts.CacheReadTokens = v
	}
	if v, err := cacheWrite.Int64(); err == nil {
		ts.CacheWriteTokens = v
	}
	if v, err := reasoning.Int64(); err == nil {
		ts.ReasoningTokens = v
	}
	return ts, nil
}

// HasAnyTaskTokens returns true if any task in the given list has non-zero
// token counts.
func HasAnyTaskTokens(tasks []*Task) bool {
	for _, t := range tasks {
		if t.InputTokens > 0 || t.OutputTokens > 0 ||
			t.CacheReadTokens > 0 || t.CacheWriteTokens > 0 ||
			t.ReasoningTokens > 0 {
			return true
		}
	}
	return false
}

// ── agent-specific parsers ─────────────────────────────────────────────────

type ClaudeUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
}

type claudeStreamEvent struct {
	Type  string       `json:"type"`
	Usage *ClaudeUsage `json:"usage"`
}

func extractClaudeTokens(stdout string) (string, TokenStats, error) {
	var stats TokenStats
	var contentLines []string

	lines := splitGenericLines(stdout)
	for _, line := range lines {
		var ev claudeStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			contentLines = append(contentLines, line)
			continue
		}

		switch ev.Type {
		case "assistant":
			var msg struct {
				Message struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(line), &msg); err == nil {
				for _, block := range msg.Message.Content {
					if block.Type == "text" && block.Text != "" {
						contentLines = append(contentLines, block.Text)
					}
				}
			}
		case "result":
			var res struct {
				Result  string `json:"result"`
				IsError bool   `json:"is_error"`
			}
			if err := json.Unmarshal([]byte(line), &res); err == nil && res.Result != "" {
				contentLines = append(contentLines, res.Result)
			}
		case "usage":
			if ev.Usage != nil {
				stats.InputTokens = ev.Usage.InputTokens
				stats.OutputTokens = ev.Usage.OutputTokens
				stats.CacheReadTokens = ev.Usage.CacheReadTokens
				stats.CacheWriteTokens = ev.Usage.CacheCreationTokens
			}
		}
	}

	return joinContentLines(contentLines), stats, nil
}

// ── Codex (JSONL) ─────────────────────────────────────────────────────────

type codexUsageEvent struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

type codexTurnCompleted struct {
	Usage  *codexUsageEvent `json:"usage,omitempty"`
	Result string           `json:"result,omitempty"`
}

type codexTurnEvent struct {
	Completed *codexTurnCompleted `json:"completed,omitempty"`
}

type codexStreamEvent struct {
	Type string          `json:"type"`
	Turn *codexTurnEvent `json:"turn,omitempty"`
}

func extractCodexTokens(stdout string) (string, TokenStats, error) {
	var stats TokenStats
	var contentLines []string

	lines := splitGenericLines(stdout)
	for _, line := range lines {
		var ev codexStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			contentLines = append(contentLines, line)
			continue
		}
		// Extract text content from completed turns
		if ev.Turn != nil && ev.Turn.Completed != nil {
			if ev.Turn.Completed.Result != "" {
				contentLines = append(contentLines, ev.Turn.Completed.Result)
			}
		}
		// Accumulate token stats across multiple turns
		if ev.Turn != nil && ev.Turn.Completed != nil && ev.Turn.Completed.Usage != nil {
			u := ev.Turn.Completed.Usage
			stats.InputTokens += u.InputTokens
			stats.OutputTokens += u.OutputTokens
			stats.CacheReadTokens += u.CachedInputTokens
			stats.ReasoningTokens += u.ReasoningOutputTokens
		}
	}

	return joinContentLines(contentLines), stats, nil
}

// ── OpenCode (JSONL) ──────────────────────────────────────────────────────

type opencodeTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cache.read"`
	CacheWrite int64 `json:"cache.write"`
}

type opencodeStepPart struct {
	Tokens *opencodeTokens `json:"tokens,omitempty"`
	Text   string          `json:"text,omitempty"`
}

type opencodeStepFinish struct {
	Part *opencodeStepPart `json:"part,omitempty"`
}

type opencodeStreamEvent struct {
	Type       string              `json:"type"`
	StepFinish *opencodeStepFinish `json:"step_finish,omitempty"`
}

func extractOpenCodeTokens(stdout string) (string, TokenStats, error) {
	var stats TokenStats
	var contentLines []string

	lines := splitGenericLines(stdout)
	for _, line := range lines {
		var ev opencodeStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			contentLines = append(contentLines, line)
			continue
		}
		// Extract text content from step finish events
		if ev.StepFinish != nil && ev.StepFinish.Part != nil && ev.StepFinish.Part.Text != "" {
			contentLines = append(contentLines, ev.StepFinish.Part.Text)
		}
		// Accumulate token stats across multiple steps
		if ev.StepFinish != nil && ev.StepFinish.Part != nil &&
			ev.StepFinish.Part.Tokens != nil {
			t := ev.StepFinish.Part.Tokens
			stats.InputTokens += t.Input
			stats.OutputTokens += t.Output
			stats.ReasoningTokens += t.Reasoning
			stats.CacheReadTokens += t.CacheRead
			stats.CacheWriteTokens += t.CacheWrite
		}
	}

	return joinContentLines(contentLines), stats, nil
}

// ── Copilot session event ─────────────────────────────────────────────────

type copilotSessionEvent struct {
	Type             string `json:"type"`
	InputTokens      int    `json:"inputTokens"`
	OutputTokens     int    `json:"outputTokens"`
	CacheReadTokens  int    `json:"cacheReadTokens"`
	CacheWriteTokens int    `json:"cacheWriteTokens"`
	ReasoningTokens  int    `json:"reasoningTokens"`
}

func parseCopilotSessionFile(fileContent string) TokenStats {
	var stats TokenStats
	lines := splitGenericLines(fileContent)
	for _, line := range lines {
		var ev copilotSessionEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type == "session.shutdown" {
			stats = TokenStats{
				InputTokens:      clampInt64Val(ev.InputTokens),
				OutputTokens:     clampInt64Val(ev.OutputTokens),
				CacheReadTokens:  clampInt64Val(ev.CacheReadTokens),
				CacheWriteTokens: clampInt64Val(ev.CacheWriteTokens),
				ReasoningTokens:  clampInt64Val(ev.ReasoningTokens),
			}
		}
	}
	return stats
}

// ── Shared helpers ────────────────────────────────────────────────────────

func splitGenericLines(s string) []string {
	raw := strings.Split(s, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func joinContentLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	result := lines[0]
	for i := 1; i < len(lines); i++ {
		result += "\n" + lines[i]
	}
	return result
}

func clampInt64Val(v int) int64 {
	if v < 0 {
		return 0
	}
	return int64(v)
}

func parseFieldInt(fields map[string]string, name string) int64 {
	if v, ok := fields[name]; ok {
		n, _ := parseInt64(v)
		return n
	}
	return 0
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// FormatTokenCount formats a token count for human display.
// >= 1M → "1.2M", >= 1K → "45K", else raw number.
func FormatTokenCount(n int64) string {
	if n == 0 {
		return "0"
	}
	abs := n
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= 1_000_000:
		v := float64(abs) / 1_000_000.0
		return formatFloatStr(v) + "M"
	case abs >= 1_000:
		if abs%1000 == 0 {
			return fmt.Sprintf("%dK", abs/1000)
		}
		v := float64(abs) / 1_000.0
		return formatFloatStr(v) + "K"
	default:
		return fmt.Sprintf("%d", abs)
	}
}

func formatFloatStr(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		return s[:len(s)-2]
	}
	return s
}

// Ensure math package is used (clampInt64Val uses int64 conversion, not math)

