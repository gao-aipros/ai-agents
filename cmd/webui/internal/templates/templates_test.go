package templates

import (
	"strings"
	"testing"
)

func TestBaseData_AllocatesNewMap(t *testing.T) {
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
	result := r.baseData(callerMap)

	// Caller's map should be unchanged.
	if _, ok := callerMap["Theme"]; ok {
		t.Error("baseData mutated the caller's map — Theme key leaked into original")
	}
	if _, ok := callerMap["CSRFToken"]; ok {
		t.Error("baseData mutated the caller's map — CSRFToken key leaked into original")
	}

	// Result should contain both the caller's keys and the base keys.
	if result["custom"] != "value" {
		t.Errorf("custom = %v, want 'value'", result["custom"])
	}
	if result["Theme"] != "light" {
		t.Errorf("Theme = %v, want 'light'", result["Theme"])
	}
}

func TestBaseData_NilInput(t *testing.T) {
	r := &Renderer{
		Theme:       "dark",
		CSRFToken:   "csrf-nil-test",
		WorkerTypes: []string{},
	}

	result := r.baseData(nil)

	if result["Theme"] != "dark" {
		t.Errorf("Theme = %v, want 'dark'", result["Theme"])
	}
	if _, ok := result["CSRFToken"]; !ok {
		t.Error("CSRFToken missing from result")
	}
}

func TestBaseData_NonMapInput(t *testing.T) {
	r := &Renderer{Theme: "light", CSRFToken: "tok", WorkerTypes: []string{}}

	// Passing a non-map type should store it under the "Data" key.
	result := r.baseData("not a map")

	if result["Theme"] != "light" {
		t.Errorf("Theme = %v, want 'light'", result["Theme"])
	}
	if result["Data"] != "not a map" {
		t.Errorf("Data = %v, want 'not a map'", result["Data"])
	}
}

func TestBaseData_SubmitResult(t *testing.T) {
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
	result := r.baseData(sr)

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

func TestBaseData_IncludesNowUnix(t *testing.T) {
	r := &Renderer{Theme: "light", CSRFToken: "tok", WorkerTypes: []string{}}

	result := r.baseData(nil)

	nowUnix, ok := result["NowUnix"].(int64)
	if !ok {
		t.Fatal("NowUnix missing or not int64")
	}
	if nowUnix <= 0 {
		t.Errorf("NowUnix = %d, want positive timestamp", nowUnix)
	}
}

func TestBaseData_CopiesAllCallerKeys(t *testing.T) {
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
	result := r.baseData(callerMap)

	for k, v := range callerMap {
		if result[k] != v {
			t.Errorf("result[%s] = %v, want %v", k, result[k], v)
		}
	}

	// Modifying result should not affect caller.
	result["Thread"] = "modified"
	if callerMap["Thread"] != "thread-value" {
		t.Error("modifying baseData result affected caller's map")
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
	if err := r.Page(&buf, "page-dashboard", nil); err != nil {
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
		data     map[string]interface{}
		contains string
		excludes string
	}{
		{
			name:     "dashboard",
			template: "page-dashboard",
			data:     nil,
			contains: "Recent Tasks",
			excludes: "Thread not found",
		},
		{
			name:     "thread list",
			template: "page-thread-list",
			data:     map[string]interface{}{"Threads": []interface{}{}},
			contains: "Threads",
		},
		{
			name:     "thread detail not found",
			template: "page-thread-detail",
			data:     map[string]interface{}{"Thread": nil},
			contains: "Thread not found",
		},
		{
			name:     "task list",
			template: "page-task-list",
			data:     nil,
			contains: "Tasks",
		},
		{
			name:     "task detail not found",
			template: "page-task-detail",
			data:     map[string]interface{}{"Task": nil},
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
