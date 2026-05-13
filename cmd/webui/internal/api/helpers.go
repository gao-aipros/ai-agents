package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noodle05/ai-agents/cmd/webui/internal/env"
	"github.com/noodle05/ai-agents/tasklib"
)

// ── filesystem helpers ────────────────────────────────────────────────────

var (
	workspaceDir      = env.String("WORKSPACE_DIR", "/workspace")
	claudeSessionsDir = env.String("CLAUDE_SESSIONS_DIR", "/home/agent/.claude")
)

// workspacePath returns the workspace directory path for a thread.
// Rejects thread IDs containing path traversal sequences.
func workspacePath(threadID string) string {
	if strings.Contains(threadID, "..") || strings.ContainsAny(threadID, "/\\") {
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
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionFile := filepath.Join(projectsDir, entry.Name(), sessionID+".json")
		if _, err := os.Stat(sessionFile); err == nil {
			os.Remove(sessionFile)
			return
		}
	}
}

// simpleUUID generates a UUID string for auto-generated thread IDs.
func simpleUUID() string {
	id, err := tasklib.NewUUID()
	if err != nil {
		return fmt.Sprintf("%08x", os.Getpid())
	}
	return id
}
