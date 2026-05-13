package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/noodle05/ai-agents/cmd/webui/internal/env"
	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
)

// ── filesystem helpers ────────────────────────────────────────────────────

// These must stay in sync with request.DefaultConfig() defaults.
// See cmd/webui/internal/request/handler.go for the canonical values.
var (
	workspaceDir      = env.String("WORKSPACE_DIR", "/workspace")
	claudeSessionsDir = env.String("CLAUDE_SESSIONS_DIR", "/home/agent/.claude")
)

// workspacePath returns the workspace directory path for a thread.
// Rejects thread IDs containing path traversal sequences or colons
// (colons break Redis key parsing in ListThreads).
// Uses request.ValidThreadID which is the single source of truth for ID validation.
func workspacePath(threadID string) string {
	if !request.ValidThreadID(threadID) {
		return ""
	}
	return filepath.Join(workspaceDir, threadID)
}

// removeWorkspace removes the workspace directory for a thread.
func removeWorkspace(path string) error {
	if path == "" || !strings.HasPrefix(path, workspaceDir) {
		return fmt.Errorf("invalid workspace path: %s", path)
	}
	return os.RemoveAll(path)
}

// removeSessionFile deletes the Claude session file for the given session UUID.
// It scans the projects directory for the session JSON file and removes it.
func removeSessionFile(sessionID string) {
	projectsDir := filepath.Join(claudeSessionsDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		log.Printf("[webui] removeSessionFile ReadDir error: %v", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionFile := filepath.Join(projectsDir, entry.Name(), sessionID+".json")
		// Remove directly — avoid TOCTOU between Stat and Remove.
		// Only return when the file was actually found and removed;
		// continue to other directories on ENOENT.
		if err := os.Remove(sessionFile); err == nil {
			return
		}
	}
}

// ── error helpers ──────────────────────────────────────────────────────────

// serverError logs the real error and returns a sanitized 500 response.
// Prevents internal details (Redis addresses, filesystem paths, etc.)
// from leaking to API consumers.
func serverError(w http.ResponseWriter, msg string, err error) {
	log.Printf("[webui] %s: %v", msg, err)
	Error(w, http.StatusInternalServerError, msg)
}

// cleanupContext returns a context for deferred Redis operations that must
// complete regardless of the HTTP request lifecycle (lock release, etc.).
// Uses context.Background() directly — ReleaseRequestLock is a Redis DEL
// which is near-instant. A timeout would require a cancel function that
// cannot be deferred cleanly in a one-liner.
func cleanupContext() context.Context {
	return context.Background()
}
