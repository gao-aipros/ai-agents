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

	// Passing a non-map type should still produce a working map.
	result := r.baseData("not a map")

	if result["Theme"] != "light" {
		t.Errorf("Theme = %v, want 'light'", result["Theme"])
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
