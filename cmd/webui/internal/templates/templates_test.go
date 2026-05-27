package templates

import (
	"strings"
	"testing"

	"github.com/noodle05/ai-agents/tasklib"
)

func TestPrepareData_PreservesCallerMap(t *testing.T) {
	r := &Renderer{
		Theme:       "light",
		HtmxSrc:     "/static/htmx.min.js",
		PollDash:    "5",
		PollThread:  "3",
		PollWorkers: "5",
		WorkerTypes: []string{"claude", "copilot", "opencode", "codex"},
		CSRFToken:   "test-csrf",
	}

	// Pass a map with caller-owned keys.
	callerMap := map[string]interface{}{"custom": "value"}
	result := r.prepareData(callerMap).(map[string]interface{})

	// Caller's map should be unchanged.
	if _, ok := callerMap["Theme"]; ok {
		t.Error("prepareData mutated the caller's map — Theme key leaked into original")
	}
	if _, ok := callerMap["CSRFToken"]; ok {
		t.Error("prepareData mutated the caller's map — CSRFToken key leaked into original")
	}

	// Result should contain both the caller's keys and the base keys.
	if result["custom"] != "value" {
		t.Errorf("custom = %v, want 'value'", result["custom"])
	}
	if result["Theme"] != "light" {
		t.Errorf("Theme = %v, want 'light'", result["Theme"])
	}
}

func TestPrepareData_NilInput(t *testing.T) {
	r := &Renderer{
		Theme:       "dark",
		CSRFToken:   "csrf-nil-test",
		WorkerTypes: []string{},
	}

	result := r.prepareData(nil).(map[string]interface{})

	if result["Theme"] != "dark" {
		t.Errorf("Theme = %v, want 'dark'", result["Theme"])
	}
	if _, ok := result["CSRFToken"]; !ok {
		t.Error("CSRFToken missing from result")
	}
}

func TestPrepareData_NonMapInput(t *testing.T) {
	r := &Renderer{Theme: "light", CSRFToken: "tok", WorkerTypes: []string{}}

	// Passing a non-map type should store it under the "Data" key.
	result := r.prepareData("not a map").(map[string]interface{})

	if result["Theme"] != "light" {
		t.Errorf("Theme = %v, want 'light'", result["Theme"])
	}
	if result["Data"] != "not a map" {
		t.Errorf("Data = %v, want 'not a map'", result["Data"])
	}
}

func TestPrepareData_SubmitResult(t *testing.T) {
	r := &Renderer{Theme: "dark", CSRFToken: "tok", WorkerTypes: []string{}}

	// The request-submitted template receives a *request.SubmitResult struct.
	// Verify that its fields are accessible via .Data.
	type SubmitResult struct {
		ThreadID  string
		RequestID string
		Status    string
	}
	sr := &SubmitResult{
		ThreadID:  "thread-abc-123",
		RequestID: "req-xyz-456",
		Status:    "submitted",
	}
	result := r.prepareData(sr).(map[string]interface{})

	if result["Data"] != sr {
		t.Error("Data should be the SubmitResult pointer itself")
	}

	data, ok := result["Data"].(*SubmitResult)
	if !ok {
		t.Fatal("Data is not a *SubmitResult")
	}
	if data.ThreadID != "thread-abc-123" {
		t.Errorf("Data.ThreadID = %q, want 'thread-abc-123'", data.ThreadID)
	}
}

func TestPrepareData_IncludesNowUnix(t *testing.T) {
	r := &Renderer{Theme: "light", CSRFToken: "tok", WorkerTypes: []string{}}

	result := r.prepareData(nil).(map[string]interface{})

	nowUnix, ok := result["NowUnix"].(int64)
	if !ok {
		t.Fatal("NowUnix missing or not int64")
	}
	if nowUnix <= 0 {
		t.Errorf("NowUnix = %d, want positive timestamp", nowUnix)
	}
}

func TestPrepareData_CopiesAllCallerKeys(t *testing.T) {
	r := &Renderer{
		Theme:       "light",
		CSRFToken:   "tok",
		WorkerTypes: []string{"claude"},
	}

	callerMap := map[string]interface{}{
		"Thread":  "thread-value",
		"Running": true,
		"Task":    nil,
	}
	result := r.prepareData(callerMap).(map[string]interface{})

	for k, v := range callerMap {
		if result[k] != v {
			t.Errorf("result[%s] = %v, want %v", k, result[k], v)
		}
	}

	// Modifying result should not affect caller.
	result["Thread"] = "modified"
	if callerMap["Thread"] != "thread-value" {
		t.Error("modifying prepareData result affected caller's map")
	}
}

func TestNew_GeneratesCSRFToken(t *testing.T) {
	r1, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r2, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if r1.CSRFToken == "" {
		t.Error("CSRFToken should not be empty")
	}
	if r1.CSRFToken == r2.CSRFToken {
		t.Error("two renderers should have different CSRFTokens (random)")
	}
	if len(r1.CSRFToken) != 32 {
		t.Errorf("CSRFToken length = %d, want 32", len(r1.CSRFToken))
	}
}

func TestNew_LoadsTemplates(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Verify that page and partial templates are parsed and executable.
	var buf mockWriter
	if err := r.Page(&buf, "page-dashboard", &DashboardView{}); err != nil {
		t.Fatalf("Page returned error: %v", err)
	}
	output := string(buf.data)
	if !strings.Contains(output, "AI Agents") || !strings.Contains(output, "Dashboard") {
		t.Errorf("Page output missing expected content, got: %s", output)
	}

	buf.data = nil
	if err := r.Partial(&buf, "worker-cards", map[string]interface{}{
		"Workers": map[string]interface{}{
			"claude": map[string]int{"Online": 1, "Instances": 1, "TotalActive": 0},
		},
	}); err != nil {
		t.Fatalf("Partial returned error: %v", err)
	}
	output = string(buf.data)
	if output == "" {
		t.Error("Partial should produce output")
	}
	if !strings.Contains(output, "worker-card") {
		t.Errorf("Partial output missing 'worker-card', got: %s", output)
	}
}

func TestPage_RendersCorrectTemplate(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Each page template should render its own content, not overwrite others.
	tests := []struct {
		name     string
		template string
		data     ViewModel
		contains string
		excludes string
	}{
		{
			name:     "dashboard",
			template: "page-dashboard",
			data:     &DashboardView{},
			contains: "Recent Tasks",
			excludes: "Thread not found",
		},
		{
			name:     "thread list",
			template: "page-thread-list",
			data:     &ThreadListView{Threads: []*tasklib.Thread{}},
			contains: "Threads",
		},
		{
			name:     "thread detail not found",
			template: "page-thread-detail",
			data:     &ThreadDetailView{},
			contains: "Thread not found",
		},
		{
			name:     "task list",
			template: "page-task-list",
			data:     &TaskListView{},
			contains: "Tasks",
		},
		{
			name:     "task detail not found",
			template: "page-task-detail",
			data:     &TaskDetailView{},
			contains: "Task not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf mockWriter
			if err := r.Page(&buf, tt.template, tt.data); err != nil {
				t.Fatalf("Page(%s) returned error: %v", tt.name, err)
			}
			output := string(buf.data)
			if !strings.Contains(output, tt.contains) {
				t.Errorf("Page(%s) missing %q in output", tt.name, tt.contains)
			}
			if tt.excludes != "" && strings.Contains(output, tt.excludes) {
				t.Errorf("Page(%s) unexpectedly contains %q", tt.name, tt.excludes)
			}
		})
	}
}

func TestStartCollapsed(t *testing.T) {
	tests := []struct {
		msgType string
		want    bool
	}{
		{"plan", true},
		{"tool_call", true},
		{"request", false},
		{"response", false},
		{"error", false},
		{"delegate", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.msgType, func(t *testing.T) {
			if got := startCollapsed(tt.msgType); got != tt.want {
				t.Errorf("startCollapsed(%q) = %v, want %v", tt.msgType, got, tt.want)
			}
		})
	}
}

func TestNew_DefaultsFromEnv(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if r.PollDash == "" {
		t.Error("PollDash should have a default")
	}
	if r.PollThread == "" {
		t.Error("PollThread should have a default")
	}
	if r.PollWorkers == "" {
		t.Error("PollWorkers should have a default")
	}
	if r.HtmxSrc == "" {
		t.Error("HtmxSrc should have a default")
	}
	if r.Theme == "" {
		t.Error("Theme should have a default")
	}
	if len(r.WorkerTypes) == 0 {
		t.Error("WorkerTypes should not be empty")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

type mockWriter struct {
	data []byte
}

func (w *mockWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

// ── dict helper function ──────────────────────────────────────────────────

func TestDict_BasicPairs(t *testing.T) {
	m := dict("ThreadID", "th-1", "Running", true)
	if len(m) != 2 {
		t.Fatalf("length = %d, want 2", len(m))
	}
	if m["ThreadID"] != "th-1" {
		t.Errorf("ThreadID = %v, want 'th-1'", m["ThreadID"])
	}
	if m["Running"] != true {
		t.Errorf("Running = %v, want true", m["Running"])
	}
}

func TestDict_MixedTypes(t *testing.T) {
	m := dict("Str", "hello", "Int", 42, "Bool", false, "Float", 3.14)
	if m["Str"] != "hello" {
		t.Errorf("Str = %v", m["Str"])
	}
	if m["Int"] != 42 {
		t.Errorf("Int = %v", m["Int"])
	}
	if m["Bool"] != false {
		t.Errorf("Bool = %v", m["Bool"])
	}
	if m["Float"] != 3.14 {
		t.Errorf("Float = %v", m["Float"])
	}
}

func TestDict_Empty(t *testing.T) {
	m := dict()
	if len(m) != 0 {
		t.Errorf("length = %d, want 0", len(m))
	}
}

func TestDict_OddArgs(t *testing.T) {
	// Should ignore the trailing unpaired value without panicking.
	m := dict("key1", "val1", "orphan")
	if len(m) != 1 {
		t.Fatalf("length = %d, want 1", len(m))
	}
	if m["key1"] != "val1" {
		t.Errorf("key1 = %v, want 'val1'", m["key1"])
	}
}

func TestDict_KeyIsStringified(t *testing.T) {
	m := dict(42, "answer") // key is int, should become "42"
	if _, ok := m["42"]; !ok {
		t.Error("int key 42 should be stringified to '42'")
	}
	if m["42"] != "answer" {
		t.Errorf("m['42'] = %v, want 'answer'", m["42"])
	}
}

func TestDict_AllocatesNewMap(t *testing.T) {
	m1 := dict("a", 1)
	m2 := dict("a", 1)
	m2["b"] = 2
	if _, ok := m1["b"]; ok {
		t.Error("m1 should not leak keys from m2")
	}
}

// ── followup-form template ───────────────────────────────────────────────

func TestFollowupForm_NotRunning(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var buf mockWriter
	// Pass a map with ThreadID and Running=false, matching what the
	// template now expects via the dict helper.
	data := map[string]interface{}{
		"ThreadID": "thread-abc",
		"Running":  false,
	}
	if err := r.Partial(&buf, "followup-form", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	// Should NOT have disabled class or attribute.
	if strings.Contains(output, "followup-disabled") {
		t.Error("output should not contain followup-disabled when Running=false")
	}
	if strings.Contains(output, "disabled") {
		t.Error("output should not contain disabled attribute when Running=false")
	}
	// Should have the normal placeholder.
	if !strings.Contains(output, "Ask a follow-up question...") {
		t.Error("output should contain 'Ask a follow-up question...'")
	}
	// Should contain the thread ID in the hidden input.
	if !strings.Contains(output, `value="thread-abc"`) {
		t.Error("output should contain thread-abc as hidden input value")
	}
	// The button should be present and not disabled.
	if !strings.Contains(output, "Send") {
		t.Error("output should contain Send button")
	}
}

func TestFollowupForm_Running(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var buf mockWriter
	data := map[string]interface{}{
		"ThreadID": "thread-running",
		"Running":  true,
	}
	if err := r.Partial(&buf, "followup-form", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	// Should have the disabled class.
	if !strings.Contains(output, "followup-disabled") {
		t.Error("output should contain followup-disabled when Running=true")
	}
	// Textarea should have disabled attribute.
	if !strings.Contains(output, "disabled") {
		t.Error("output should contain disabled attribute when Running=true")
	}
	// Should have the "Agent is working..." placeholder.
	if !strings.Contains(output, "Agent is working...") {
		t.Error("output should contain 'Agent is working...'")
	}
	// The hidden input should still carry the thread ID.
	if !strings.Contains(output, `value="thread-running"`) {
		t.Error("output should contain thread-running as hidden input value")
	}
}

func TestFollowupForm_ThreadIDEscaped(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var buf mockWriter
	data := map[string]interface{}{
		"ThreadID": `th-<script>alert(1)</script>`,
		"Running":  false,
	}
	if err := r.Partial(&buf, "followup-form", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	// The raw script tag should not appear (html/template escapes it).
	if strings.Contains(output, "<script>") {
		t.Error("output should not contain raw <script> tag")
	}
	// The escaped version should appear.
	if !strings.Contains(output, "&lt;script&gt;") {
		t.Error("output should contain escaped script tag")
	}
}

// ── thread-history-poll template (OOB swap on completion) ─────────────────

func TestHistoryPoll_Running(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var buf mockWriter
	data := map[string]interface{}{
		"ThreadID":   "th-poll",
		"Running":    true,
		"NextOffset": 5,
		"Messages":   []interface{}{}, // no new messages
	}
	if err := r.Partial(&buf, "thread-history-poll", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	// Should contain the history-poller for continued polling.
	if !strings.Contains(output, `id="history-poller"`) {
		t.Error("output should contain history-poller when Running=true")
	}
	if !strings.Contains(output, `hx-get="/api/threads/th-poll/history?offset=5&poll=1"`) {
		t.Error("output should contain poll URL when Running=true")
	}
	// Should NOT contain the followup section.
	if strings.Contains(output, `id="followup-section"`) {
		t.Error("output should not contain followup-section when Running=true")
	}
}

func TestHistoryPoll_Completed(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var buf mockWriter
	data := map[string]interface{}{
		"ThreadID":   "th-done",
		"Running":    false,
		"NextOffset": 10,
		"Messages":   []interface{}{},
	}
	if err := r.Partial(&buf, "thread-history-poll", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	// Should contain the followup section with sticky class.
	if !strings.Contains(output, `class="followup-sticky"`) {
		t.Error("output should contain followup-sticky class when Running=false")
	}
	if !strings.Contains(output, `id="followup-section"`) {
		t.Error("output should contain followup-section when Running=false")
	}
	// The history-poller should be swapped OOB to clear the poller.
	if !strings.Contains(output, `hx-swap-oob="true"`) {
		t.Error("output should contain hx-swap-oob='true' on the history-poller cleanup")
	}
	// Should contain the followup form template output.
	if !strings.Contains(output, `Follow-up`) {
		t.Error("output should contain Follow-up heading when Running=false")
	}
	// Should have the enabled form (Running=false in template call).
	if !strings.Contains(output, "Ask a follow-up question...") {
		t.Error("output should contain 'Ask a follow-up question...'")
	}
}

// ── <time datetime> rendering ─────────────────────────────────────────────

func TestTimeElement_TaskTable(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	task := map[string]interface{}{
		"TaskID":     "task-1",
		"Worker":     "claude",
		"Status":     "done",
		"EnqueuedAt": "2026-05-19T14:30:00Z",
		"CompletedAt": "2026-05-19T14:35:00Z",
	}
	data := map[string]interface{}{"Tasks": []interface{}{task}}

	var buf mockWriter
	if err := r.Partial(&buf, "task-table", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	if !strings.Contains(output, `<time datetime="2026-05-19T14:30:00Z">`) {
		t.Error("task-table should contain <time datetime=...> for EnqueuedAt")
	}
}

func TestTimeElement_TaskTable_Empty(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	task := map[string]interface{}{"TaskID": "task-1", "Worker": "claude", "Status": "done", "EnqueuedAt": "", "CompletedAt": ""}
	data := map[string]interface{}{"Tasks": []interface{}{task}}

	var buf mockWriter
	if err := r.Partial(&buf, "task-table", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	if strings.Contains(output, "<time") {
		t.Error("task-table should not contain <time> when EnqueuedAt is empty")
	}
	if !strings.Contains(output, ">-<") {
		t.Error("task-table should show '-' fallback when EnqueuedAt is empty")
	}
}

func TestTimeElement_TaskDetail(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	task := map[string]interface{}{
		"TaskID":      "task-1",
		"Worker":      "claude",
		"Status":      "done",
		"EnqueuedAt":   "2026-05-19T14:30:00Z",
		"ExitCode":    "0",
		"CompletedAt": "2026-05-19T14:35:00Z",
		"Instruction":  "test",
	}
	data := map[string]interface{}{"Task": task}

	var buf mockWriter
	if err := r.Partial(&buf, "page-task-detail", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	if !strings.Contains(output, `<time datetime="2026-05-19T14:30:00Z">`) {
		t.Error("task detail should contain <time> for EnqueuedAt")
	}
	if !strings.Contains(output, `<time datetime="2026-05-19T14:35:00Z">`) {
		t.Error("task detail should contain <time> for CompletedAt")
	}
}

func TestTimeElement_ThreadState(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	thread := map[string]interface{}{
		"ThreadID":  "th-1",
		"Status":    "complete",
		"GHRepo":    "owner/repo",
		"CreatedAt": "2026-05-19T14:20:00Z",
		"UpdatedAt": "2026-05-19T14:30:00Z",
		"GHPRNumber": "-",
		"LastDesign": "-",
	}
	data := map[string]interface{}{"Thread": thread}

	var buf mockWriter
	if err := r.Partial(&buf, "thread-state", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	if !strings.Contains(output, `<time datetime="2026-05-19T14:30:00Z">`) {
		t.Error("thread-state should contain <time> for UpdatedAt")
	}
}

func TestTimeElement_ThreadList(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	thread := map[string]interface{}{
		"ThreadID":  "th-1",
		"Status":    "complete",
		"GHRepo":    "owner/repo",
		"GHPRNumber": "-",
		"CreatedAt": "2026-05-19T14:20:00Z",
		"UpdatedAt": "2026-05-19T14:30:00Z",
	}
	data := map[string]interface{}{
		"Threads":  []interface{}{thread},
		"Children": make(map[string]interface{}),
	}

	var buf mockWriter
	if err := r.Partial(&buf, "thread-table", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	if !strings.Contains(output, `<time datetime="2026-05-19T14:30:00Z">`) {
		t.Error("thread-table should contain <time> for UpdatedAt")
	}
}

func TestTimeElement_ThreadHistory(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	msg := map[string]interface{}{
		"Role":      "user",
		"Type":      "request",
		"Content":    "hello",
		"Timestamp": "2026-05-19T14:30:00Z",
	}
	data := map[string]interface{}{
		"Messages": []interface{}{msg},
		"ThreadID": "th-1",
		"Running":  false,
		"MsgCount":  1,
	}

	var buf mockWriter
	if err := r.Partial(&buf, "thread-history", data); err != nil {
		t.Fatalf("Partial: %v", err)
	}
	output := string(buf.data)

	if !strings.Contains(output, `<time datetime="2026-05-19T14:30:00Z">`) {
		t.Error("thread-history should contain <time> for Timestamp")
	}
}

func TestTimeElement_BasePage_IncludesLocalizeTimestamps(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var buf mockWriter
	if err := r.Page(&buf, "page-dashboard", &DashboardView{}); err != nil {
		t.Fatalf("Page: %v", err)
	}
	output := string(buf.data)

	if !strings.Contains(output, "function localizeTimestamps") {
		t.Error("base page should contain localizeTimestamps function")
	}
	if !strings.Contains(output, `time[datetime]`) {
		t.Error("base page should contain time[datetime] selector")
	}
	if !strings.Contains(output, `el.dateTime`) {
		t.Error("base page should use el.dateTime DOM property")
	}
	if !strings.Contains(output, `toLocaleString`) {
		t.Error("base page should use toLocaleString for formatting")
	}
	if !strings.Contains(output, "htmx:afterSwap', localizeTimestamps") {
		t.Error("base page should hook htmx:afterSwap for localizeTimestamps")
	}
}

// ── formatRuntime ──────────────────────────────────────────────────────────

func TestFormatRuntime_EmptyStart(t *testing.T) {
	if got := formatRuntime("", "2026-05-19T14:35:00Z"); got != "-" {
		t.Errorf("formatRuntime(empty, end) = %q, want \"-\"", got)
	}
}

func TestFormatRuntime_DashStart(t *testing.T) {
	if got := formatRuntime("-", "2026-05-19T14:35:00Z"); got != "-" {
		t.Errorf("formatRuntime(\"-\", end) = %q, want \"-\"", got)
	}
}

func TestFormatRuntime_StartParseFailure(t *testing.T) {
	if got := formatRuntime("not-a-timestamp", "2026-05-19T14:35:00Z"); got != "-" {
		t.Errorf("formatRuntime(bad start, end) = %q, want \"-\"", got)
	}
}

func TestFormatRuntime_EndDashSentinel(t *testing.T) {
	if got := formatRuntime("2026-05-19T14:30:00Z", "-"); got != "-" {
		t.Errorf("formatRuntime(start, \"-\") = %q, want \"-\"", got)
	}
}

func TestFormatRuntime_EndParseFailure(t *testing.T) {
	if got := formatRuntime("2026-05-19T14:30:00Z", "garbage"); got != "-" {
		t.Errorf("formatRuntime(start, bad end) = %q, want \"-\"", got)
	}
}

func TestFormatRuntime_EndEmptyUsesNow(t *testing.T) {
	// With a past start and empty end, should produce a positive duration (not "-").
	got := formatRuntime("2026-05-19T14:30:00Z", "")
	if got == "-" {
		t.Error("formatRuntime(start, \"\") = \"-\", want a non-negative duration using now")
	}
	// Should contain at least a number and unit suffix.
	if len(got) < 2 {
		t.Errorf("formatRuntime(start, \"\") = %q, too short for a duration", got)
	}
}

func TestFormatRuntime_NegativeDuration(t *testing.T) {
	if got := formatRuntime("2026-05-19T14:35:00Z", "2026-05-19T14:30:00Z"); got != "-" {
		t.Errorf("formatRuntime(later, earlier) = %q, want \"-\"", got)
	}
}

func TestFormatRuntime_ZeroDuration(t *testing.T) {
	got := formatRuntime("2026-05-19T14:30:00Z", "2026-05-19T14:30:00Z")
	if got != "0s" {
		t.Errorf("formatRuntime(same, same) = %q, want \"0s\"", got)
	}
}

func TestFormatRuntime_SubMinute(t *testing.T) {
	got := formatRuntime("2026-05-19T14:30:00Z", "2026-05-19T14:30:30Z")
	if got != "30s" {
		t.Errorf("formatRuntime(30s) = %q, want \"30s\"", got)
	}
}

func TestFormatRuntime_MinuteBoundary(t *testing.T) {
	// 59s should still be in seconds.
	got := formatRuntime("2026-05-19T14:30:00Z", "2026-05-19T14:30:59Z")
	if got != "59s" {
		t.Errorf("formatRuntime(59s) = %q, want \"59s\"", got)
	}
	// 60s should transition to minutes.
	got = formatRuntime("2026-05-19T14:30:00Z", "2026-05-19T14:31:00Z")
	if got != "1m 0s" {
		t.Errorf("formatRuntime(60s) = %q, want \"1m 0s\"", got)
	}
}

func TestFormatRuntime_MinutesAndSeconds(t *testing.T) {
	got := formatRuntime("2026-05-19T14:30:00Z", "2026-05-19T14:35:30Z")
	if got != "5m 30s" {
		t.Errorf("formatRuntime(5m 30s) = %q, want \"5m 30s\"", got)
	}
}

func TestFormatRuntime_HourBoundary(t *testing.T) {
	// 59m 59s should stay in minutes.
	got := formatRuntime("2026-05-19T14:30:00Z", "2026-05-19T15:29:59Z")
	if got != "59m 59s" {
		t.Errorf("formatRuntime(59m 59s) = %q, want \"59m 59s\"", got)
	}
	// 60m should transition to hours.
	got = formatRuntime("2026-05-19T14:30:00Z", "2026-05-19T15:30:00Z")
	if got != "1h 0m" {
		t.Errorf("formatRuntime(60m) = %q, want \"1h 0m\"", got)
	}
}

func TestFormatRuntime_HoursAndMinutes(t *testing.T) {
	got := formatRuntime("2026-05-19T14:30:00Z", "2026-05-19T17:45:00Z")
	if got != "3h 15m" {
		t.Errorf("formatRuntime(3h 15m) = %q, want \"3h 15m\"", got)
	}
}

func TestFormatRuntime_DayBoundary(t *testing.T) {
	// 23h 59m should stay in hours.
	got := formatRuntime("2026-05-19T14:30:00Z", "2026-05-20T14:29:00Z")
	if got != "23h 59m" {
		t.Errorf("formatRuntime(23h 59m) = %q, want \"23h 59m\"", got)
	}
	// 24h should transition to days.
	got = formatRuntime("2026-05-19T14:30:00Z", "2026-05-20T14:30:00Z")
	if got != "1d 0h" {
		t.Errorf("formatRuntime(24h) = %q, want \"1d 0h\"", got)
	}
}

func TestFormatRuntime_MultiDay(t *testing.T) {
	got := formatRuntime("2026-05-19T14:30:00Z", "2026-05-21T18:45:00Z")
	if got != "2d 4h" {
		t.Errorf("formatRuntime(2d 4h) = %q, want \"2d 4h\"", got)
	}
}

// ── runtimeForStatus ───────────────────────────────────────────────────────

func TestRuntimeForStatus_InProgressRunning(t *testing.T) {
	// For running status, end should be ignored and now used.
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T14:35:00Z", "running")
	if got == "5m 0s" {
		t.Error("runtimeForStatus(running) should ignore end and use now, got fixed 5m duration")
	}
	if got == "-" {
		t.Error("runtimeForStatus(running) should produce a positive duration, not \"-\"")
	}
}

func TestRuntimeForStatus_InProgressPending(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T14:35:00Z", "pending")
	if got == "5m 0s" {
		t.Error("runtimeForStatus(pending) should ignore end and use now")
	}
	if got == "-" {
		t.Error("runtimeForStatus(pending) should produce a positive duration")
	}
}

func TestRuntimeForStatus_InProgressInitiated(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T14:35:00Z", "initiated")
	if got == "5m 0s" {
		t.Error("runtimeForStatus(initiated) should ignore end and use now")
	}
	if got == "-" {
		t.Error("runtimeForStatus(initiated) should produce a positive duration")
	}
}

func TestRuntimeForStatus_InProgressReviewing(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T14:35:00Z", "reviewing")
	if got == "5m 0s" {
		t.Error("runtimeForStatus(reviewing) should ignore end and use now, got fixed 5m duration")
	}
	if got == "-" {
		t.Error("runtimeForStatus(reviewing) should produce a positive duration")
	}
}

func TestRuntimeForStatus_TerminalDone(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T14:35:00Z", "done")
	if got != "5m 0s" {
		t.Errorf("runtimeForStatus(done, 14:30→14:35) = %q, want \"5m 0s\"", got)
	}
}

func TestRuntimeForStatus_TerminalFailed(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T14:31:00Z", "failed")
	if got != "1m 0s" {
		t.Errorf("runtimeForStatus(failed, 14:30→14:31) = %q, want \"1m 0s\"", got)
	}
}

func TestRuntimeForStatus_TerminalCancelled(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T14:45:00Z", "cancelled")
	if got != "15m 0s" {
		t.Errorf("runtimeForStatus(cancelled, 14:30→14:45) = %q, want \"15m 0s\"", got)
	}
}

func TestRuntimeForStatus_TerminalComplete(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T15:00:00Z", "complete")
	if got != "30m 0s" {
		t.Errorf("runtimeForStatus(complete, 14:30→15:00) = %q, want \"30m 0s\"", got)
	}
}

func TestRuntimeForStatus_TerminalError(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:30:00Z", "2026-05-19T14:32:00Z", "error")
	if got != "2m 0s" {
		t.Errorf("runtimeForStatus(error, 14:30→14:32) = %q, want \"2m 0s\"", got)
	}
}

func TestRuntimeForStatus_TerminalMissingEnd(t *testing.T) {
	// Terminal status with "-" sentinel end (no completed_at in Redis) → "-".
	got := runtimeForStatus("2026-05-19T14:30:00Z", "-", "done")
	if got != "-" {
		t.Errorf("runtimeForStatus(done, sentinel \"-\") = %q, want \"-\"", got)
	}
}

func TestRuntimeForStatus_EmptyStart(t *testing.T) {
	got := runtimeForStatus("", "2026-05-19T14:35:00Z", "running")
	if got != "-" {
		t.Errorf("runtimeForStatus(empty start) = %q, want \"-\"", got)
	}
}

func TestRuntimeForStatus_Negative(t *testing.T) {
	got := runtimeForStatus("2026-05-19T14:35:00Z", "2026-05-19T14:30:00Z", "done")
	if got != "-" {
		t.Errorf("runtimeForStatus(negative duration) = %q, want \"-\"", got)
	}
}

// ── statusBadge ────────────────────────────────────────────────────────────

func TestStatusBadgeReviewing(t *testing.T) {
	got := statusBadge("reviewing")
	if !strings.Contains(string(got), "badge-warning") {
		t.Errorf("statusBadge(reviewing) should contain badge-warning, got: %s", got)
	}
}

func TestStatusBadgeUnknown(t *testing.T) {
	got := statusBadge("some-future-status")
	if strings.Contains(string(got), "badge-info") {
		t.Errorf("statusBadge(unknown) should not contain badge-info, got: %s", got)
	}
	if !strings.Contains(string(got), "badge-secondary") {
		t.Errorf("statusBadge(unknown) should contain badge-secondary, got: %s", got)
	}
}

func TestStatusBadgeReviewingWarningNotInfo(t *testing.T) {
	got := statusBadge("reviewing")
	if strings.Contains(string(got), "badge-info") {
		t.Errorf("statusBadge(reviewing) should not contain badge-info, got: %s", got)
	}
}

// ── prepareData ViewModel tests ────────────────────────────────────────────

func TestPrepareData_FillsViewModel(t *testing.T) {
	r := &Renderer{
		Theme:       "dark",
		HtmxSrc:     "/static/htmx.js",
		PollDash:    "10",
		PollThread:  "5",
		PollWorkers: "8",
		WorkerTypes: []string{"claude", "codex"},
		CSRFToken:   "test-token",
	}

	vm := &DashboardView{}
	r.prepareData(vm)

	bv := vm.baseView()
	if bv.Theme != "dark" {
		t.Errorf("Theme = %q, want \"dark\"", bv.Theme)
	}
	if bv.HtmxSrc != "/static/htmx.js" {
		t.Errorf("HtmxSrc = %q, want \"/static/htmx.js\"", bv.HtmxSrc)
	}
	if bv.PollDash != "10" {
		t.Errorf("PollDash = %q, want \"10\"", bv.PollDash)
	}
	if bv.PollThread != "5" {
		t.Errorf("PollThread = %q, want \"5\"", bv.PollThread)
	}
	if bv.PollWorkers != "8" {
		t.Errorf("PollWorkers = %q, want \"8\"", bv.PollWorkers)
	}
	if len(bv.WorkerTypes) != 2 || bv.WorkerTypes[0] != "claude" {
		t.Errorf("WorkerTypes = %v, want [claude codex]", bv.WorkerTypes)
	}
	if bv.CSRFToken != "test-token" {
		t.Errorf("CSRFToken = %q, want \"test-token\"", bv.CSRFToken)
	}
	if bv.NowUnix <= 0 {
		t.Errorf("NowUnix = %d, want positive timestamp", bv.NowUnix)
	}
}

func TestPage_TypoInFieldName(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var buf mockWriter
	// page-thread-list references {{.Threads}}, which does not exist on
	// DashboardView. This should return an execution-time error.
	err = r.Page(&buf, "page-thread-list", &DashboardView{})
	if err == nil {
		t.Error("Page should error when template references a field that doesn't exist on the struct")
	}
}

func TestPrepareData_NoAllocForViewModel(t *testing.T) {
	r := &Renderer{
		Theme:       "dark",
		CSRFToken:   "tok",
		WorkerTypes: []string{"claude"},
	}
	vm := &DashboardView{}
	n := testing.AllocsPerRun(100, func() {
		r.prepareData(vm)
	})
	if n > 0 {
		t.Errorf("prepareData(ViewModel) allocated %.0f times, want 0", n)
	}
}

func TestPrepareData_NilViewModel(t *testing.T) {
	r := &Renderer{
		Theme:       "dark",
		CSRFToken:   "tok",
		WorkerTypes: []string{},
	}
	// Typed nil — fillBaseView returns early, no panic.
	var vm ViewModel = (*DashboardView)(nil)
	result := r.prepareData(vm)
	// The nil ViewModel passes through unchanged (fillBaseView guards but
	// prepareData's ViewModel path still returns the vm as-is).
	if result != vm {
		t.Errorf("expected nil ViewModel to pass through, got %v", result)
	}
}

func TestFillBaseView_NilViewModel(t *testing.T) {
	r := &Renderer{Theme: "dark", CSRFToken: "tok", WorkerTypes: []string{}}
	var vm ViewModel = (*DashboardView)(nil)
	// Should not panic.
	r.fillBaseView(vm)
}
