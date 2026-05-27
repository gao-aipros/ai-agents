package templates

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"path/filepath"
	"reflect"
	"time"

	"github.com/noodle05/ai-agents/cmd/webui/internal/env"
	"github.com/noodle05/ai-agents/tasklib"
)

//go:embed *.html */*.html
var templateFS embed.FS

// Renderer parses and executes Go html/template files.
type Renderer struct {
	tmpl        *template.Template
	PollDash    string
	PollThread  string
	PollWorkers string
	HtmxSrc     string
	Theme       string
	WorkerTypes []string
	CSRFToken   string
}

// New creates a new Renderer with defaults from environment variables.
func New() (*Renderer, error) {
	csrf := make([]byte, 16)
	if _, err := rand.Read(csrf); err != nil {
		return nil, err
	}
	r := &Renderer{
		PollDash:    env.String("WEBUI_POLL_DASHBOARD", "5"),
		PollThread:  env.String("WEBUI_POLL_THREAD_DETAIL", "3"),
		PollWorkers: env.String("WEBUI_POLL_WORKERS", "5"),
		HtmxSrc:     env.String("WEBUI_HTMX_SRC", "/static/htmx.min.js"),
		Theme:       env.String("WEBUI_THEME", "light"),
		WorkerTypes: tasklib.WorkerTypes,
		CSRFToken:   hex.EncodeToString(csrf),
	}

	tmpl := template.New("").Funcs(template.FuncMap{
		"statusBadge":    statusBadge,
		"roleClass":      roleClass,
		"badgeForRole":   badgeForRole,
		"or":             orDefault,
		"startCollapsed": startCollapsed,
		"dict":           dict,
		"formatRuntime":   formatRuntime,
		"runtime":         runtimeForStatus,
		"formatTokenCount": tasklib.FormatTokenCount,
		"add":             func(a, b int) int { return a + b },
	})

	err := fs.WalkDir(templateFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".html" {
			return nil
		}
		content, err := templateFS.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = tmpl.Parse(string(content))
		return err
	})
	if err != nil {
		return nil, err
	}

	r.tmpl = tmpl
	return r, nil
}

// ── View Model Types ────────────────────────────────────────────────────────

// BaseView holds template variables common to every rendered page/partial.
// Page-specific view models embed this struct directly so Go html/template
// promotes its fields to the top level (e.g. {{.Theme}} works alongside
// {{.Threads}}).
type BaseView struct {
	// Renderer-config fields — copied from Renderer at render time.
	Theme       string
	HtmxSrc     string
	PollDash    string
	PollThread  string
	PollWorkers string
	WorkerTypes []string
	CSRFToken   string

	// Per-request field — set by fillBaseView for each render.
	NowUnix int64

	// Page-output field — set by Page() after rendering the content template.
	PageContent template.HTML
}

// ViewModel is implemented by all page view model types.
// The baseView() method lets the renderer find and populate the embedded
// BaseView without reflection.
type ViewModel interface {
	baseView() *BaseView
}

// DashboardView is the view model for the dashboard page.
type DashboardView struct {
	BaseView
	TokenStats *DashboardTokenStats
}

// DashboardTokenStats holds the aggregated token statistics for the dashboard.
type DashboardTokenStats struct {
	TotalIn   string
	TotalOut  string
	TaskCount int64
	Rows      []DashboardTokenStatsRow
}

// DashboardTokenStatsRow is a single row in the dashboard token table.
type DashboardTokenStatsRow struct {
	Agent  string
	Input  string
	Output string
}

func (v *DashboardView) baseView() *BaseView { return &v.BaseView }

// ThreadListView is the view model for the thread list page.
type ThreadListView struct {
	BaseView
	Threads  []*tasklib.Thread
	Children map[string][]*tasklib.Thread
	SortBy   string
	SortDir  string
}

func (v *ThreadListView) baseView() *BaseView { return &v.BaseView }

// ThreadDetailView is the view model for the thread detail page.
type ThreadDetailView struct {
	BaseView
	Thread    *tasklib.Thread
	Running   bool
	Complete  bool
	TokenRows []TokenRow
	Tasks     []*tasklib.Task
	Children  []*tasklib.Thread
}

// TokenRow represents a single row in the per-agent token usage table.
type TokenRow struct {
	Agent     string
	Input     string
	Output    string
	Cache     string
	Reasoning string
}

func (v *ThreadDetailView) baseView() *BaseView { return &v.BaseView }

// TaskListView is the view model for the task list page.
type TaskListView struct {
	BaseView
	SortBy  string
	SortDir string
}

func (v *TaskListView) baseView() *BaseView { return &v.BaseView }

// TaskDetailView is the view model for the task detail page.
type TaskDetailView struct {
	BaseView
	Task     *tasklib.Task
	TailInfo string
}

func (v *TaskDetailView) baseView() *BaseView { return &v.BaseView }

// WorkerView is the view model for the worker cards partial.
type WorkerView struct {
	BaseView
	Workers tasklib.WorkerStats
}

func (v *WorkerView) baseView() *BaseView { return &v.BaseView }

// fillBaseView populates the embedded BaseView fields from the Renderer.
// Guard: if vm is a typed nil (e.g. var vm ViewModel = (*DashboardView)(nil)),
// the call is silently ignored — no fields are populated and no panic occurs.
func (r *Renderer) fillBaseView(vm ViewModel) {
	if reflect.ValueOf(vm).IsNil() {
		return
	}
	bv := vm.baseView()
	bv.Theme = r.Theme
	bv.HtmxSrc = r.HtmxSrc
	bv.PollDash = r.PollDash
	bv.PollThread = r.PollThread
	bv.PollWorkers = r.PollWorkers
	bv.WorkerTypes = r.WorkerTypes
	bv.CSRFToken = r.CSRFToken
	bv.NowUnix = time.Now().Unix()
}

// prepareData returns template-ready data.
// For ViewModel: delegates to fillBaseView (mutates in-place, zero allocs).
// For map[string]interface{}: creates merged map with base keys (one alloc).
// For nil/other: creates base-only map (one alloc).
func (r *Renderer) prepareData(data interface{}) interface{} {
	if vm, ok := data.(ViewModel); ok {
		r.fillBaseView(vm)
		return vm
	}
	m := make(map[string]interface{})
	if existing, ok := data.(map[string]interface{}); ok {
		for k, v := range existing {
			m[k] = v
		}
	} else if data != nil {
		m["Data"] = data
	}
	m["Theme"] = r.Theme
	m["HtmxSrc"] = r.HtmxSrc
	m["PollDash"] = r.PollDash
	m["PollThread"] = r.PollThread
	m["PollWorkers"] = r.PollWorkers
	m["WorkerTypes"] = r.WorkerTypes
	m["CSRFToken"] = r.CSRFToken
	m["NowUnix"] = time.Now().Unix()
	return m
}

// Page renders a full HTML page. The content template is rendered first and
// passed as PageContent to base.html, which injects it via {{.PageContent}}.
func (r *Renderer) Page(w io.Writer, contentTemplate string, vm ViewModel) error {
	r.fillBaseView(vm)
	var content bytes.Buffer
	if err := r.tmpl.ExecuteTemplate(&content, contentTemplate, vm); err != nil {
		return err
	}
	vm.baseView().PageContent = template.HTML(content.String())
	return r.tmpl.ExecuteTemplate(w, "base.html", vm)
}

// Partial renders a named template without the layout shell (for HTMX responses).
func (r *Renderer) Partial(w io.Writer, name string, data interface{}) error {
	return r.tmpl.ExecuteTemplate(w, name, r.prepareData(data))
}

// ── template helper functions ─────────────────────────────────────────────

func statusBadge(status string) template.HTML {
	class := "badge"
	switch status {
	case "done", "complete":
		class += " badge-success"
	case "failed", "error":
		class += " badge-danger"
	case "running", "reviewing":
		class += " badge-warning"
	case "pending", "initiated", "queued":
		class += " badge-info"
	case "cancelled":
		class += " badge-primary"
	default:
		class += " badge-secondary"
	}
	return template.HTML(`<span class="` + class + `">` + template.HTMLEscapeString(status) + `</span>`)
}

func roleClass(role, msgType string) string {
	if msgType == "response" {
		return "role-master type-response"
	}
	if msgType == "error" {
		return "role-master type-error"
	}
	return "role-" + role + " type-" + msgType
}

func badgeForRole(role, msgType string) template.HTML {
	switch {
	case msgType == "response":
		return template.HTML(`<span class="badge badge-success">response</span>`)
	case msgType == "error":
		return template.HTML(`<span class="badge badge-danger">error</span>`)
	case role == "user":
		return template.HTML(`<span class="badge badge-primary">user</span>`)
	case role == "master":
		return template.HTML(`<span class="badge" style="background:var(--color-master-bg);color:var(--color-master)">master</span>`)
	case role == "worker", role == "claude", role == "copilot", role == "opencode":
		return template.HTML(`<span class="badge badge-info">` + template.HTMLEscapeString(role) + `</span>`)
	default:
		return template.HTML(`<span class="badge">` + template.HTMLEscapeString(role) + `</span>`)
	}
}

func orDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

// startCollapsed returns true if messages of this type should start collapsed.
// Plan and tool_call messages (intermediate thinking) are verbose, so collapse them.
func startCollapsed(msgType string) bool {
	return msgType == "plan" || msgType == "tool_call"
}

// dict creates a map from alternating key/value pairs. Used by templates
// to pass multiple named arguments to a sub-template. Ignores trailing
// unpaired value if called with an odd number of arguments.
func dict(values ...interface{}) map[string]interface{} {
	n := len(values) / 2 * 2 // ignore trailing unpaired value
	m := make(map[string]interface{}, n/2)
	for i := 0; i < n; i += 2 {
		key := fmt.Sprint(values[i])
		m[key] = values[i+1]
	}
	return m
}

// runtimeForStatus formats a duration based on task/thread status.
// For in-progress statuses (running, pending, initiated), end is ignored and now is used.
// For terminal statuses, end is used directly.
func runtimeForStatus(startRaw, endRaw, status string) string {
	if status == "running" || status == "pending" || status == "initiated" || status == "reviewing" {
		return formatRuntime(startRaw, "")
	}
	return formatRuntime(startRaw, endRaw)
}

// formatRuntime computes and formats a duration between two RFC 3339 timestamps.
// If end is empty, uses the current time (for in-progress items).
// If end is "-", start is empty/"-", or either parse fails, returns "-".
// Returns "-" for negative durations.
func formatRuntime(startRaw, endRaw string) string {
	if startRaw == "" || startRaw == "-" {
		return "-"
	}
	start, err := time.Parse(time.RFC3339, startRaw)
	if err != nil {
		return "-"
	}
	var end time.Time
	if endRaw == "" {
		end = time.Now().UTC()
	} else if endRaw == "-" {
		return "-"
	} else {
		end, err = time.Parse(time.RFC3339, endRaw)
		if err != nil {
			return "-"
		}
	}
	d := end.Sub(start)
	if d < 0 {
		return "-"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		days := int(d.Hours() / 24)
		h := int(d.Hours()) % 24
		return fmt.Sprintf("%dd %dh", days, h)
	}
}
