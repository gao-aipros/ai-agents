package templates

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"html/template"
	"io"
	"io/fs"
	"path/filepath"
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
		"statusBadge":  statusBadge,
		"roleClass":    roleClass,
		"badgeForRole": badgeForRole,
		"or":           orDefault,
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

// baseData merges data with base template variables.
// Always allocates a new map to avoid mutating caller's data.
func (r *Renderer) baseData(data interface{}) map[string]interface{} {
	m := make(map[string]interface{})
	if existing, ok := data.(map[string]interface{}); ok {
		for k, v := range existing {
			m[k] = v
		}
	}
	m["Theme"] = r.Theme
	m["HtmxSrc"] = r.HtmxSrc
	m["PollDash"] = r.PollDash
	m["PollThread"] = r.PollThread
	m["PollWorkers"] = r.PollWorkers
	m["WorkerTypes"] = r.WorkerTypes
	m["CSRFToken"] = r.CSRFToken
	m["NowUnix"] = time.Now().Unix()
	// Safe defaults for template iteration — the "content" template from the
	// last-parsed file accesses these, so provide empty slices when not set.
	if _, ok := m["Threads"]; !ok {
		m["Threads"] = []interface{}{}
	}
	if _, ok := m["Tasks"]; !ok {
		m["Tasks"] = []interface{}{}
	}
	return m
}

// Page renders a full HTML page using base.html as the layout.
func (r *Renderer) Page(w io.Writer, data interface{}) error {
	return r.tmpl.ExecuteTemplate(w, "base.html", r.baseData(data))
}

// Partial renders a named template without the layout shell (for HTMX responses).
func (r *Renderer) Partial(w io.Writer, name string, data interface{}) error {
	return r.tmpl.ExecuteTemplate(w, name, r.baseData(data))
}

// ── template helper functions ─────────────────────────────────────────────

func statusBadge(status string) template.HTML {
	class := "badge"
	switch status {
	case "done", "complete":
		class += " badge-success"
	case "failed", "error":
		class += " badge-danger"
	case "running":
		class += " badge-warning"
	case "pending", "initiated":
		class += " badge-info"
	case "cancelled":
		class += " badge-primary"
	default:
		class += " badge-info"
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
