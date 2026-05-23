package tasklib

import (
	"context"
	"os"
	"path/filepath"
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
	case *ClaudeStatsProvider: s = "ClaudeStatsProvider"
	case *CodexStatsProvider: s = "CodexStatsProvider"
	case *OpenCodeStatsProvider: s = "OpenCodeStatsProvider"
	case *CopilotStatsProvider: s = "CopilotStatsProvider"
	case *NoopStatsProvider: s = "NoopStatsProvider"
	}
	return s
}

// ── NoopStatsProvider ─────────────────────────────────────────────────────

func TestNoopStatsProvider(t *testing.T) {
	p := &NoopStatsProvider{}
	args, cleanup, err := p.Setup("/tmp")
	if err != nil || args != nil || cleanup != nil {
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
	stdout := `{"type":"TurnCompleted","usage":{"input_tokens":200,"cached_input_tokens":30,"output_tokens":80,"reasoning_output_tokens":5}}
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
	stdout := `{"type":"TurnCompleted"}`
	p := &CodexStatsProvider{}
	_, stats, err := p.Process("/tmp", stdout)
	if err != nil || stats.HasAny() {
		t.Error("no usage should yield zero stats")
	}
}

// ── OpenCodeStatsProvider ─────────────────────────────────────────────────

func TestOpenCodeStatsProvider_Process(t *testing.T) {
	stdout := `{"type":"step_finish","step_finish":{"part":{"tokens":{"input":300,"output":120,"reasoning":10,"cache":{"read":40,"write":15}}}}}
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
	stdout := `{"type":"step_finish","step_finish":{"part":{}}}`
	p := &OpenCodeStatsProvider{}
	_, stats, err := p.Process("/tmp", stdout)
	if err != nil || stats.HasAny() {
		t.Error("no tokens should yield zero stats")
	}
}

// ── CopilotStatsProvider ──────────────────────────────────────────────────

func TestCopilotStatsProvider_Setup(t *testing.T) {
	p := &CopilotStatsProvider{}
	args, cleanup, err := p.Setup("/tmp")
	if err != nil {
		t.Fatalf("Setup error: %v", err)
	}
	if len(args) != 1 || !strings.HasPrefix(args[0], "--session-id=") {
		t.Errorf("args = %v, want [--session-id=<uuid>]", args)
	}
	if cleanup == nil {
		t.Error("cleanup should not be nil")
	}
	cleanup() // should not panic
}

func TestCopilotStatsProvider_Process_NoSessionFile(t *testing.T) {
	// Use a real fresh provider
	p2 := &CopilotStatsProvider{}
	content, stats, err := p2.Process("/tmp", "plain text")
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if content != "plain text" || stats.HasAny() {
		t.Error("missing session file should passthrough with zero stats")
	}
}

func TestCopilotStatsProvider_Process_WithSessionFile(t *testing.T) {
	origDir := copilotSessionDir
	tmpDir := t.TempDir()
	copilotSessionDir = tmpDir
	defer func() { copilotSessionDir = origDir }()

	sessionID := "test-session-001"
	sessionPath := filepath.Join(tmpDir, sessionID)
	os.MkdirAll(sessionPath, 0755)
	os.WriteFile(filepath.Join(sessionPath, "events.jsonl"),
		[]byte(`{"type":"session.shutdown","inputTokens":500,"outputTokens":200,"cacheReadTokens":50,"cacheWriteTokens":25,"reasoningTokens":10}`), 0644)

	p := &CopilotStatsProvider{}
	p.sessionID = sessionID
	_, stats, err := p.Process("/tmp", "output")
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if stats.InputTokens != 500 || stats.OutputTokens != 200 ||
		stats.CacheReadTokens != 50 || stats.CacheWriteTokens != 25 ||
		stats.ReasoningTokens != 10 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestCopilotStatsProvider_LastShutdownWins(t *testing.T) {
	origDir := copilotSessionDir
	tmpDir := t.TempDir()
	copilotSessionDir = tmpDir
	defer func() { copilotSessionDir = origDir }()

	sessionID := "multi-shutdown"
	sessionPath := filepath.Join(tmpDir, sessionID)
	os.MkdirAll(sessionPath, 0755)
	os.WriteFile(filepath.Join(sessionPath, "events.jsonl"),
		[]byte(`{"type":"session.shutdown","inputTokens":100}
{"type":"session.shutdown","inputTokens":999}`), 0644)

	p := &CopilotStatsProvider{}
	p.sessionID = sessionID
	_, stats, _ := p.Process("/tmp", "out")
	if stats.InputTokens != 999 {
		t.Errorf("last shutdown should win: got %d", stats.InputTokens)
	}
}

func TestParseCopilotSessionFile_Empty(t *testing.T) {
	if parseCopilotSessionFile("").HasAny() {
		t.Error("empty file should yield zero stats")
	}
}

func TestParseCopilotSessionFile_NoShutdown(t *testing.T) {
	if parseCopilotSessionFile(`{"type":"other"}`).HasAny() {
		t.Error("no shutdown event should yield zero stats")
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
	worker, _ := rdb.HGetAll(ctx, StatsWorkerKey("claude")).Result()
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
	m, _ := rdb.HGetAll(ctx, StatsWorkerKey("master")).Result()
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
	// Codex with --json outputs ItemStarted events with inlined AgentMessage details
	stdout := `{"type":"ItemStarted","id":"msg_1","details":{"AgentMessage":{"text":"This is the agent response"}}}
`
	p := &CodexStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "This is the agent response") {
		t.Errorf("content should contain agent response, got: %q", content)
	}
	// AgentMessage events don't carry usage — stats come from TurnCompleted events
	if stats.HasAny() {
		t.Errorf("AgentMessage events should not carry tokens: got %+v", stats)
	}
}

func TestCodexStatsProvider_Process_MultipleTurnsAccumulate(t *testing.T) {
	stdout := `{"type":"ItemStarted","id":"m1","details":{"AgentMessage":{"text":"First"}}}
{"type":"TurnCompleted","usage":{"input_tokens":10,"output_tokens":5}}
{"type":"ItemStarted","id":"m2","details":{"AgentMessage":{"text":"Second"}}}
{"type":"TurnCompleted","usage":{"input_tokens":20,"output_tokens":8}}
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
	stdout := `{"type":"ItemStarted","id":"m1","details":{"AgentMessage":{"text":"Good"}}}
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
		t.Error("no TurnCompleted event, stats should be zero")
	}
}

func TestCodexStatsProvider_Process_SkipsItemUpdatedAndItemCompleted(t *testing.T) {
	// Only ItemStarted events should contribute text — ItemUpdated and
	// ItemCompleted may carry incremental or duplicate text.
	stdout := `{"type":"ItemStarted","id":"m1","details":{"AgentMessage":{"text":"Final text"}}}
{"type":"ItemUpdated","id":"m1","details":{"AgentMessage":{"text":"Duplicate"}}}
{"type":"ItemCompleted","id":"m1","details":{"AgentMessage":{"text":"Duplicate 2"}}}
`
	p := &CodexStatsProvider{}
	content, _, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "Final text") {
		t.Errorf("content should contain ItemStarted text: %q", content)
	}
	if strings.Contains(content, "Duplicate") {
		t.Error("content should NOT contain ItemUpdated/ItemCompleted text")
	}
}

func TestCodexStatsProvider_Process_IgnoresUsageOnNonTurnCompleted(t *testing.T) {
	stdout := `{"type":"ItemStarted","id":"m1","details":{"AgentMessage":{"text":"Hello"}}}
{"type":"ItemCompleted","usage":{"input_tokens":999,"output_tokens":999}}
{"type":"TurnCompleted","usage":{"input_tokens":100,"output_tokens":50}}
`
	p := &CodexStatsProvider{}
	_, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if stats.InputTokens != 100 || stats.OutputTokens != 50 {
		t.Errorf("stats = %+v, want Input=100 Output=50 (ItemCompleted usage ignored)", stats)
	}
}

// ── OpenCode content extraction tests ──────────────────────────────────

func TestOpenCodeStatsProvider_Process_ContentExtraction(t *testing.T) {
	// OpenCode with --format json outputs text events with text content
	stdout := `{"type":"text","part":{"text":"Implementation complete"}}
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

func TestOpenCodeStatsProvider_Process_TextFromStepFinish(t *testing.T) {
	// step_finish.part.text is still extracted as a fallback
	stdout := `{"type":"step_finish","step_finish":{"part":{"text":"from step finish","tokens":{"input":10,"output":5,"cache":{"read":1,"write":2}}}}}
`
	p := &OpenCodeStatsProvider{}
	content, stats, err := p.Process("/tmp", stdout)
	if err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !strings.Contains(content, "from step finish") {
		t.Errorf("content should contain step_finish text: %q", content)
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

func TestOpenCodeStatsProvider_Process_MultipleStepsAccumulate(t *testing.T) {
	stdout := `{"type":"step_finish","step_finish":{"part":{"text":"Step 1","tokens":{"input":10,"output":5,"cache":{"read":1,"write":2}}}}}
{"type":"step_finish","step_finish":{"part":{"text":"Step 2","tokens":{"input":20,"output":8,"cache":{"read":3,"write":4}}}}}
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
	stdout := `{"type":"step_finish","step_finish":{"part":{"text":"OK"}}}
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
