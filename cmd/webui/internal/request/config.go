package request

import (
	"time"

	"github.com/noodle05/ai-agents/cmd/webui/internal/env"
	"github.com/noodle05/ai-agents/tasklib"
)

// PathsConfig holds filesystem paths shared across components.
type PathsConfig struct {
	WorkspaceDir      string
	ClaudeSessionsDir string
}

// Config holds configuration for the request handler.
type Config struct {
	ClaudePath     string
	Paths          *PathsConfig
	RequestTimeout time.Duration
	MaxConcurrent  int
	ShutdownGrace  time.Duration
	// OutputFormat controls the claude -p output mode: "text" (plain -p) or
	// "stream-json" (--output-format stream-json --verbose). Default "text".
	OutputFormat string
	// AgentName is the orchestrator's display name (default "master").
	AgentName string
	// AgentRole is the orchestrator's role (e.g., "designer").
	AgentRole string
	// TestNotify is an optional channel that receives the thread ID when
	// a background subprocess completes. Only used in tests.
	TestNotify chan string
}

// DefaultConfig returns a Config with defaults from environment variables.
func DefaultConfig() Config {
	return Config{
		ClaudePath: env.String("CLAUDE_PATH", "/usr/local/bin/claude"),
		Paths: &PathsConfig{
			WorkspaceDir:      env.String("WORKSPACE_DIR", "/workspace"),
			ClaudeSessionsDir: env.String("CLAUDE_SESSIONS_DIR", "/home/agent/.claude"),
		},
		RequestTimeout: time.Duration(env.Int("REQUEST_TIMEOUT", tasklib.DefaultRequestTimeout)) * time.Second,
		MaxConcurrent:  env.Int("MAX_CONCURRENT_REQUESTS", 5),
		ShutdownGrace:  time.Duration(env.Int("REQUEST_SHUTDOWN_GRACE", 60)) * time.Second,
		OutputFormat:   env.String("CLAUDE_OUTPUT_FORMAT", "text"),
		AgentName:      env.String("AGENT_NAME", "master"),
		AgentRole:      env.String("AGENT_ROLE", ""),
	}
}
