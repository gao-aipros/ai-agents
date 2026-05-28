package tasklib

import (
	"testing"
)

func TestParseHeartbeatWorkerName_NewFormat(t *testing.T) {
	name := ParseHeartbeatWorkerName("worker:claude:heartbeat")
	if name != "claude" {
		t.Errorf("expected 'claude', got '%s'", name)
	}
}

func TestParseHeartbeatWorkerName_OldFormat(t *testing.T) {
	name := ParseHeartbeatWorkerName("worker:claude:host-a:heartbeat")
	if name != "claude" {
		t.Errorf("expected 'claude', got '%s'", name)
	}
}

func TestParseHeartbeatWorkerName_OldFormatCopilot(t *testing.T) {
	name := ParseHeartbeatWorkerName("worker:copilot:host-b:heartbeat")
	if name != "copilot" {
		t.Errorf("expected 'copilot', got '%s'", name)
	}
}

func TestParseHeartbeatWorkerName_Unrecognized(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"worker:claude", ""},
		{"worker:claude:heartbeat:extra", ""},
		{"tasks:queue:claude", ""},
		{"worker:", ""},
		{"", ""},
		{"worker", ""},
	}
	for _, tc := range tests {
		got := ParseHeartbeatWorkerName(tc.key)
		if got != tc.want {
			t.Errorf("ParseHeartbeatWorkerName(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestParseHeartbeatWorkerName_BothFormatsTableDriven(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		// New format: worker:<name>:heartbeat
		{"worker:claude:heartbeat", "claude"},
		{"worker:copilot:heartbeat", "copilot"},
		{"worker:opencode:heartbeat", "opencode"},
		{"worker:codex:heartbeat", "codex"},
		{"worker:master:heartbeat", "master"},
		// Old format: worker:<type>:<hostname>:heartbeat
		{"worker:claude:host-a:heartbeat", "claude"},
		{"worker:copilot:host-b:heartbeat", "copilot"},
		{"worker:opencode:host-c:heartbeat", "opencode"},
	}
	for _, tc := range tests {
		got := ParseHeartbeatWorkerName(tc.key)
		if got != tc.want {
			t.Errorf("ParseHeartbeatWorkerName(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
