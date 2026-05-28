package tasklib

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// ── FormatTokenCount tests ────────────────────────────────────────────────

func TestFormatTokenCount(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1K"},
		{1500, "1.5K"},
		{10000, "10K"},
		{1000000, "1M"},
		{1500000, "1.5M"},
		{12000000, "12M"},
	}
	for _, tt := range tests {
		got := FormatTokenCount(tt.n)
		if got != tt.want {
			t.Errorf("FormatTokenCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestTokenStats_HasAny(t *testing.T) {
	if (TokenStats{}).HasAny() {
		t.Error("zero TokenStats should have HasAny=false")
	}
	if !(TokenStats{InputTokens: 1}).HasAny() {
		t.Error("TokenStats with InputTokens should have HasAny=true")
	}
	if !(TokenStats{OutputTokens: 1}).HasAny() {
		t.Error("TokenStats with OutputTokens should have HasAny=true")
	}
}

// ── NewStatsProvider tests ────────────────────────────────────────────────

func TestNewStatsProvider(t *testing.T) {
	tests := []struct {
		agentType string
		wantType  string
	}{
		{"claude", "ClaudeStatsProvider"},
		{"codex", "CodexStatsProvider"},
		{"opencode", "OpenCodeStatsProvider"},
		{"copilot", "CopilotStatsProvider"},
		{"unknown", "NoopStatsProvider"},
		{"", "NoopStatsProvider"},
	}
	for _, tt := range tests {
		p := NewStatsProvider(tt.agentType)
		got := provTypeName(p)
		if got != tt.wantType {
			t.Errorf("NewStatsProvider(%q) = %s, want %s", tt.agentType, got, tt.wantType)
		}
	}
}

func provTypeName(v StatsProvider) string {
	s := "?"
	switch v.(type) {
	case *ClaudeStatsProvider:
		s = "ClaudeStatsProvider"
	case *CodexStatsProvider:
		s = "CodexStatsProvider"
	case *OpenCodeStatsProvider:
		s = "OpenCodeStatsProvider"
	case *CopilotStatsProvider:
		s = "CopilotStatsProvider"
	case *NoopStatsProvider:
		s = "NoopStatsProvider"
	}
	return s
}

// ── NoopStatsProvider ─────────────────────────────────────────────────────

func TestNoopStatsProvider(t *testing.T) {
	p := &NoopStatsProvider{}
	expandCmd, cleanup, err := p.Setup("/tmp")
	if err != nil || expandCmd != nil || cleanup != nil {
		t.Error("Setup should return nil,nil,nil")
	}
	content, stats, err := p.Process("/tmp", "hello")
	if err != nil || content != "hello" || stats.HasAny() {
		t.Error("Process should passthrough with zero stats")
	}
}

// ── ClaudeStatsProvider ───────────────────────────────────────────────────

func TestClaudeStatsProvider_Process(t *testing.T) {
	stdout := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}
{"type":"usage","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}}
{"type":"assistant","message":{"content":[{"type":"text","text":"More text"}]}}
{"type":"result","result":"Final answer","is_error":false}
`
	p := &ClaudeStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if stats.InputTokens != 100 || stats.OutputTokens != 50 ||
		stats.CacheReadTokens != 20 || stats.CacheWriteTokens != 10 {
		t.Errorf("stats = %+v, want Input=100 Output=50 CR=20 CW=10", stats)
	}
	if !strings.Contains(content, "Hello world") || !strings.Contains(content, "Final answer") {
		t.Error("content missing expected text")
	}
	if strings.Contains(content, "usage") || strings.Contains(content, "system") {
		t.Error("content should not contain metadata events")
	}
}

func TestClaudeStatsProvider_Process_Empty(t *testing.T) {
	p := &ClaudeStatsProvider{}
	content, stats, err := p.Process("/tmp", "")
	if err != nil || content != "" || stats.HasAny() {
		t.Error("empty stdout should yield empty content and zero stats")
	}
}

func TestClaudeStatsProvider_Process_MalformedLines(t *testing.T) {
	stdout := `not json at all
{"type":"usage","usage":{"input_tokens":50}}
`
	p := &ClaudeStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if stats.InputTokens != 50 {
		t.Errorf("should extract tokens from valid lines: got %d", stats.InputTokens)
	}
	if !strings.Contains(content, "not json at all") {
		t.Error("non-JSON lines should be kept as content")
	}
}

// ── CodexStatsProvider ────────────────────────────────────────────────────

func TestCodexStatsProvider_Process(t *testing.T) {
	stdout := `{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":30,"output_tokens":80,"reasoning_output_tokens":5}}
`
	p := &CodexStatsProvider{}
	_, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if stats.InputTokens != 200 || stats.OutputTokens != 80 ||
		stats.CacheReadTokens != 30 || stats.ReasoningTokens != 5 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestCodexStatsProvider_Process_NoUsage(t *testing.T) {
	stdout := `{"type":"turn.completed"}`
	p := &CodexStatsProvider{}
	_, stats, err := p.Process("/tmp", stdout)
	if err != nil || stats.HasAny() {
		t.Error("no usage should yield zero stats")
	}
}

// ── OpenCodeStatsProvider ─────────────────────────────────────────────────

func TestOpenCodeStatsProvider_Process(t *testing.T) {
	stdout := `{"type":"step_finish","part":{"tokens":{"input":300,"output":120,"reasoning":10,"cache":{"read":40,"write":15}}}}
`
	p := &OpenCodeStatsProvider{}
	_, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if stats.InputTokens != 300 || stats.OutputTokens != 120 ||
		stats.CacheReadTokens != 40 || stats.CacheWriteTokens != 15 ||
		stats.ReasoningTokens != 10 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestOpenCodeStatsProvider_Process_NoTokens(t *testing.T) {
	stdout := `{"type":"step_finish","part":{}}`
	p := &OpenCodeStatsProvider{}
	_, stats, err := p.Process("/tmp", stdout)
	if err != nil || stats.HasAny() {
		t.Error("no tokens should yield zero stats")
	}
}

// ── CopilotStatsProvider ──────────────────────────────────────────────────

func TestCopilotStatsProvider_Setup(t *testing.T) {
	p := &CopilotStatsProvider{}
	expandCmd, cleanup, err := p.Setup("/tmp")
	if err != nil {
		t.Fatalf("Setup error: %v", err)
	}
	if expandCmd == nil {
		t.Fatal("expandCmd should not be nil")
	}
	if cleanup == nil {
		t.Error("cleanup should not be nil")
	}

	// Verify the expander replaces __SESSION_ID__ with the UUID.
	expanded := expandCmd("copilot --yolo --session-id=__SESSION_ID__ -p")
	if expanded == "copilot --yolo --session-id=__SESSION_ID__ -p" {
		t.Error("__SESSION_ID__ was not replaced")
	}
	if !strings.Contains(expanded, "--session-id="+p.sessionID) {
		t.Errorf("expanded cmd = %s, want session-id to be %s", expanded, p.sessionID)
	}

	cleanup() // should not panic
}

func TestCopilotStatsProvider_Process_Passthrough(t *testing.T) {
	p := &CopilotStatsProvider{}
	content, stats, err := p.Process("/tmp", "plain text")
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if content != "plain text" || stats.HasAny() {
		t.Error("Process should passthrough stdout with zero stats")
	}
}

func TestParseCopilotStderr(t *testing.T) {
	stderr := "Tokens ↑ 42.2k (27.4k cached) • ↓ 574"
	stats := ParseCopilotStderr(stderr)
	if stats.InputTokens != 42200 {
		t.Errorf("InputTokens = %d, want 42200", stats.InputTokens)
	}
	if stats.CacheReadTokens != 27400 {
		t.Errorf("CacheReadTokens = %d, want 27400", stats.CacheReadTokens)
	}
	if stats.OutputTokens != 574 {
		t.Errorf("OutputTokens = %d, want 574", stats.OutputTokens)
	}
}

func TestParseCopilotStderr_MegaScale(t *testing.T) {
	stderr := "Tokens ↑ 1.5M (800K cached) • ↓ 12.3k"
	stats := ParseCopilotStderr(stderr)
	if stats.InputTokens != 1500000 {
		t.Errorf("InputTokens = %d, want 1500000", stats.InputTokens)
	}
	if stats.CacheReadTokens != 800000 {
		t.Errorf("CacheReadTokens = %d, want 800000", stats.CacheReadTokens)
	}
	if stats.OutputTokens != 12300 {
		t.Errorf("OutputTokens = %d, want 12300", stats.OutputTokens)
	}
}

func TestParseCopilotStderr_NoMatch(t *testing.T) {
	if ParseCopilotStderr("some random error message").HasAny() {
		t.Error("unrelated stderr should yield zero stats")
	}
}

func TestParseCopilotStderr_Empty(t *testing.T) {
	if ParseCopilotStderr("").HasAny() {
		t.Error("empty stderr should yield zero stats")
	}
}

// ── Persistence ───────────────────────────────────────────────────────────

func TestPersistTokenStats(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	stats := TokenStats{InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 100, CacheWriteTokens: 50, ReasoningTokens: 25}
	pipe := rdb.Pipeline()
	PersistTokenStats(ctx, pipe, "task-001", "claude", stats)
	pipe.Exec(ctx)

	// Global total
	total, _ := rdb.HGetAll(ctx, StatsTotalKey()).Result()
	if total["input_tokens"] != "1000" || total["task_count"] != "1" {
		t.Errorf("total = %v", total)
	}
	// Per-worker
	worker, _ := rdb.HGetAll(ctx, StatsAgentKey("claude")).Result()
	if worker["input_tokens"] != "1000" {
		t.Errorf("worker = %v", worker)
	}
	// Per-task
	if v, _ := rdb.Get(ctx, TaskKey("task-001", "input_tokens")).Int64(); v != 1000 {
		t.Errorf("task input = %d", v)
	}
	if v, _ := rdb.Get(ctx, TaskKey("task-001", "reasoning_tokens")).Int64(); v != 25 {
		t.Errorf("task reasoning = %d, want 25", v)
	}
}

func TestPersistTokenStats_ZeroFieldsNotStored(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	stats := TokenStats{} // all zero
	pipe := rdb.Pipeline()
	PersistTokenStats(ctx, pipe, "task-zero", "codex", stats)
	pipe.Exec(ctx)

	exists, _ := rdb.Exists(ctx, TaskKey("task-zero", "input_tokens")).Result()
	if exists > 0 {
		t.Error("zero input_tokens should not create a key")
	}
	// task_count should still increment
	total, _ := rdb.HGetAll(ctx, StatsTotalKey()).Result()
	if total["task_count"] != "1" {
		t.Errorf("task_count should increment: got %s", total["task_count"])
	}
}

func TestPersistMasterTokenStats(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	threadID := "th-master"
	rdb.HSet(ctx, ThreadStateKey(threadID), "status", "running")

	stats := TokenStats{InputTokens: 2000, OutputTokens: 800, CacheReadTokens: 300}
	pipe := rdb.Pipeline()
	PersistMasterTokenStats(ctx, pipe, threadID, stats)
	pipe.Exec(ctx)

	// Thread fields
	th, _ := rdb.HGetAll(ctx, ThreadStateKey(threadID)).Result()
	if th["master_input_tokens"] != "2000" || th["status"] != "running" {
		t.Errorf("thread fields = %v", th)
	}
	// Master counter
	m, _ := rdb.HGetAll(ctx, StatsAgentKey("master")).Result()
	if m["input_tokens"] != "2000" || m["task_count"] != "1" {
		t.Errorf("master counter = %v", m)
	}
}

func TestGetTokenStats(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := NewClient(rdb)
	ctx := context.Background()

	rdb.HSet(ctx, StatsTotalKey(), "input_tokens", "5000", "output_tokens", "2500")

	ts, _ := client.GetTokenStats(ctx, StatsTotalKey())
	if ts.InputTokens != 5000 || ts.OutputTokens != 2500 {
		t.Errorf("GetTokenStats = %+v", ts)
	}
}

func TestGetTokenStats_Empty(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := NewClient(rdb)

	ts, err := client.GetTokenStats(context.Background(), StatsTotalKey())
	if err != nil || ts.HasAny() {
		t.Error("empty hash should return zero stats")
	}
}

func TestGetMasterTokenStats(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := NewClient(rdb)
	ctx := context.Background()

	rdb.HSet(ctx, ThreadStateKey("th-gm"), "master_input_tokens", "3000")
	ts, _ := client.GetMasterTokenStats(ctx, "th-gm")
	if ts.InputTokens != 3000 {
		t.Errorf("MasterTokenStats Input = %d, want 3000", ts.InputTokens)
	}
}

func TestGetTaskTokenStats(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := NewClient(rdb)
	ctx := context.Background()

	rdb.Set(ctx, TaskKey("tt-1", "input_tokens"), 100, 0)
	rdb.Set(ctx, TaskKey("tt-1", "output_tokens"), 50, 0)

	ts, _ := client.GetTaskTokenStats(ctx, "tt-1")
	if ts.InputTokens != 100 || ts.OutputTokens != 50 {
		t.Errorf("GetTaskTokenStats = %+v", ts)
	}
}

func TestGetTokenStatsTaskCount(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := NewClient(rdb)
	ctx := context.Background()

	rdb.HSet(ctx, StatsTotalKey(), "task_count", 42)
	count, _ := client.GetTokenStatsTaskCount(ctx, StatsTotalKey())
	if count != 42 {
		t.Errorf("task_count = %d", count)
	}
}

func TestHasAnyTaskTokens(t *testing.T) {
	tasks := []*Task{{InputTokens: 0}, {OutputTokens: 0}}
	if HasAnyTaskTokens(tasks) {
		t.Error("all-zero tasks should return false")
	}
	tasks[0].InputTokens = 1
	if !HasAnyTaskTokens(tasks) {
		t.Error("task with tokens should return true")
	}
}

// ── Codex content extraction tests ─────────────────────────────────────

func TestCodexStatsProvider_Process_ContentExtraction(t *testing.T) {
	// Codex with --json outputs item.completed events with agent_message items.
	stdout := `{"type":"item.completed","item":{"type":"agent_message","text":"This is the agent response"}}
`
	p := &CodexStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "This is the agent response") {
		t.Errorf("content should contain agent response, got: %q", content)
	}
	if stats.HasAny() {
		t.Errorf("agent_message items should not carry tokens: got %+v", stats)
	}
}

func TestCodexStatsProvider_Process_MultipleTurnsAccumulate(t *testing.T) {
	stdout := `{"type":"item.completed","item":{"type":"agent_message","text":"First"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}
{"type":"item.completed","item":{"type":"agent_message","text":"Second"}}
{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":8}}
`
	p := &CodexStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "First") || !strings.Contains(content, "Second") {
		t.Errorf("content should contain both responses: %q", content)
	}
	if stats.InputTokens != 30 {
		t.Errorf("InputTokens accumulated = %d, want 30", stats.InputTokens)
	}
	if stats.OutputTokens != 13 {
		t.Errorf("OutputTokens accumulated = %d, want 13", stats.OutputTokens)
	}
}

func TestCodexStatsProvider_Process_MixedValidAndInvalid(t *testing.T) {
	stdout := `{"type":"item.completed","item":{"type":"agent_message","text":"Good"}}
plain text line
`
	p := &CodexStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "Good") {
		t.Error("missing JSONL content")
	}
	if !strings.Contains(content, "plain text line") {
		t.Error("missing plain text content")
	}
	if stats.HasAny() {
		t.Error("no turn.completed event, stats should be zero")
	}
}

func TestCodexStatsProvider_Process_SkipsItemUpdatedAndItemCompleted(t *testing.T) {
	// Only item.completed with agent_message type contributes text.
	// Other item types (command_execution, mcp_tool_call, etc.) are skipped.
	stdout := `{"type":"item.completed","item":{"type":"agent_message","text":"Final text"}}
{"type":"item.completed","item":{"type":"command_execution","command":"ls","aggregated_output":"file list"}}
{"type":"item.completed","item":{"type":"todo_list","items":[]}}
`
	p := &CodexStatsProvider{}
	content, _, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "Final text") {
		t.Errorf("content should contain agent_message text: %q", content)
	}
	if strings.Contains(content, "file list") {
		t.Error("content should NOT contain command_execution aggregated_output")
	}
}

func TestCodexStatsProvider_Process_IgnoresUsageOnNonTurnCompleted(t *testing.T) {
	stdout := `{"type":"item.completed","item":{"type":"agent_message","text":"Hello"}}
{"type":"item.completed","item":{"type":"mcp_tool_call"},"usage":{"input_tokens":999,"output_tokens":999}}
{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":50}}
`
	p := &CodexStatsProvider{}
	_, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if stats.InputTokens != 100 || stats.OutputTokens != 50 {
		t.Errorf("stats = %+v, want Input=100 Output=50 (non-turn usage ignored)", stats)
	}
}

// ── OpenCode content extraction tests ──────────────────────────────────

func TestOpenCodeStatsProvider_Process_ContentExtraction(t *testing.T) {
	// OpenCode with --format json outputs tool_use events with output in part.state.output
	stdout := `{"type":"tool_use","part":{"state":{"output":"Implementation complete"}}}
`
	p := &OpenCodeStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "Implementation complete") {
		t.Errorf("content should contain text event text: %q", content)
	}
	if stats.HasAny() {
		t.Errorf("text events should not carry tokens: got %+v", stats)
	}
}

func TestOpenCodeStatsProvider_Process_ToolUseContent(t *testing.T) {
	// Content is extracted from tool_use events at part.state.output
	stdout := `{"type":"tool_use","part":{"state":{"output":"Implementation complete"}}}
{"type":"step_finish","part":{"tokens":{"input":10,"output":5,"cache":{"read":1,"write":2}}}}
`
	p := &OpenCodeStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "Implementation complete") {
		t.Errorf("content should contain tool_use output: %q", content)
	}
	if stats.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", stats.InputTokens)
	}
	if stats.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", stats.OutputTokens)
	}
	if stats.CacheReadTokens != 1 {
		t.Errorf("CacheReadTokens = %d, want 1", stats.CacheReadTokens)
	}
	if stats.CacheWriteTokens != 2 {
		t.Errorf("CacheWriteTokens = %d, want 2", stats.CacheWriteTokens)
	}
}

func TestOpenCodeStatsProvider_Process_ToolUseNoTokens(t *testing.T) {
	stdout := `{"type":"tool_use","part":{"state":{"output":"content only"}}}`
	p := &OpenCodeStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "content only") {
		t.Errorf("content should contain tool_use output: %q", content)
	}
	if stats.HasAny() {
		t.Errorf("no step_finish event, stats should be zero: got %+v", stats)
	}
}

func TestOpenCodeStatsProvider_Process_MultipleStepsAccumulate(t *testing.T) {
	stdout := `{"type":"tool_use","part":{"state":{"output":"Step 1"}}}
{"type":"step_finish","part":{"tokens":{"input":10,"output":5,"cache":{"read":1,"write":2}}}}
{"type":"tool_use","part":{"state":{"output":"Step 2"}}}
{"type":"step_finish","part":{"tokens":{"input":20,"output":8,"cache":{"read":3,"write":4}}}}
`
	p := &OpenCodeStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "Step 1") || !strings.Contains(content, "Step 2") {
		t.Errorf("content should contain both steps: %q", content)
	}
	if stats.InputTokens != 30 {
		t.Errorf("InputTokens accumulated = %d, want 30", stats.InputTokens)
	}
	if stats.OutputTokens != 13 {
		t.Errorf("OutputTokens accumulated = %d, want 13", stats.OutputTokens)
	}
	if stats.CacheReadTokens != 4 {
		t.Errorf("CacheReadTokens accumulated = %d, want 4", stats.CacheReadTokens)
	}
	if stats.CacheWriteTokens != 6 {
		t.Errorf("CacheWriteTokens accumulated = %d, want 6", stats.CacheWriteTokens)
	}
}

func TestOpenCodeStatsProvider_Process_MixedValidAndInvalid(t *testing.T) {
	stdout := `{"type":"tool_use","part":{"state":{"output":"OK"}}}
plain line
`
	p := &OpenCodeStatsProvider{}
	content, _, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "OK") && !strings.Contains(content, "plain line") {
		t.Error("content missing")
	}
}

// ── Real Redis integration tests ─────────────────────────────────────────

func TestPersistTokenStats_RealRedis(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     "redis:6379",
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("real Redis not available: %v", err)
	}

	// Clean up any leftover keys from previous runs
	rdb.Del(ctx, StatsTotalKey(), StatsAgentKey("copilot"))
	rdb.Del(ctx, TaskKey("real-task-001", "input_tokens"))
	rdb.Del(ctx, TaskKey("real-task-001", "output_tokens"))
	rdb.Del(ctx, TaskKey("real-task-001", "cache_read_tokens"))
	rdb.Del(ctx, TaskKey("real-task-001", "reasoning_tokens"))

	stats := TokenStats{InputTokens: 42200, OutputTokens: 574, CacheReadTokens: 27400}
	pipe := rdb.Pipeline()
	PersistTokenStats(ctx, pipe, "real-task-001", "copilot", stats)
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("PersistTokenStats failed: %v", err)
	}

	// Verify global totals
	total, err := rdb.HGetAll(ctx, StatsTotalKey()).Result()
	if err != nil {
		t.Fatalf("HGetAll total failed: %v", err)
	}
	if total["input_tokens"] != "42200" {
		t.Errorf("total input_tokens = %q, want 42200", total["input_tokens"])
	}
	if total["cache_read"] != "27400" {
		t.Errorf("total cache_read = %q, want 27400", total["cache_read"])
	}
	if total["output_tokens"] != "574" {
		t.Errorf("total output_tokens = %q, want 574", total["output_tokens"])
	}
	if total["task_count"] != "1" {
		t.Errorf("total task_count = %q, want 1", total["task_count"])
	}

	// Verify per-worker
	worker, err := rdb.HGetAll(ctx, StatsAgentKey("copilot")).Result()
	if err != nil {
		t.Fatalf("HGetAll worker failed: %v", err)
	}
	if worker["input_tokens"] != "42200" {
		t.Errorf("worker input_tokens = %q, want 42200", worker["input_tokens"])
	}

	// Verify per-task keys
	inputV, _ := rdb.Get(ctx, TaskKey("real-task-001", "input_tokens")).Int64()
	if inputV != 42200 {
		t.Errorf("task input_tokens = %d, want 42200", inputV)
	}

	// Clean up
	rdb.Del(ctx, StatsTotalKey(), StatsAgentKey("copilot"))
	rdb.Del(ctx, TaskKey("real-task-001", "input_tokens"))
	rdb.Del(ctx, TaskKey("real-task-001", "output_tokens"))
	rdb.Del(ctx, TaskKey("real-task-001", "cache_read_tokens"))
	rdb.Del(ctx, TaskKey("real-task-001", "reasoning_tokens"))
}

func TestParseCopilotStderr_Integration(t *testing.T) {
	// End-to-end test: parse Copilot stderr and persist to real Redis
	stderr := "Tokens ↑ 42.2k (27.4k cached) • ↓ 574"
	stats := ParseCopilotStderr(stderr)
	if !stats.HasAny() {
		t.Fatal("expected non-zero stats from stderr")
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     "redis:6379",
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("real Redis not available: %v", err)
	}

	// Clean up
	rdb.Del(ctx, StatsTotalKey(), StatsAgentKey("copilot"))
	for _, f := range []string{"input_tokens", "output_tokens", "cache_read_tokens"} {
		rdb.Del(ctx, TaskKey("copilot-int-001", f))
	}

	pipe := rdb.Pipeline()
	PersistTokenStats(ctx, pipe, "copilot-int-001", "copilot", stats)
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("PersistTokenStats failed: %v", err)
	}

	// Verify stats persisted correctly from stderr parsing
	total, _ := rdb.HGetAll(ctx, StatsTotalKey()).Result()
	if total["input_tokens"] != "42200" || total["output_tokens"] != "574" || total["cache_read"] != "27400" {
		t.Errorf("total = %v, want Input=42200 Output=574 CacheRead=27400", total)
	}

	// Clean up
	rdb.Del(ctx, StatsTotalKey(), StatsAgentKey("copilot"))
	for _, f := range []string{"input_tokens", "output_tokens", "cache_read_tokens"} {
		rdb.Del(ctx, TaskKey("copilot-int-001", f))
	}
}
