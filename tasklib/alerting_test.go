package tasklib

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAlertConfigDefaults(t *testing.T) {
	// Clear env vars to test defaults using t.Setenv for parallel-safety
	for _, k := range []string{"ALERT_WEBHOOK_URL", "WEBHOOK_SECRET", "ALERT_ON_FAILED", "ALERT_ON_STUCK_THREAD", "ALERT_ON_WORKER_LOST"} {
		t.Setenv(k, "")
	}
	cfg := LoadAlertConfig()
	if cfg.IsEnabled() {
		t.Error("AlertConfig should be disabled when ALERT_WEBHOOK_URL is not set")
	}
	if cfg.WebhookURL != "" {
		t.Error("WebhookURL should be empty by default")
	}
}

func TestAlertConfigEnabled(t *testing.T) {
	t.Setenv("ALERT_WEBHOOK_URL", "https://hooks.example.com/alerts")
	t.Setenv("ALERT_ON_FAILED", "true")

	cfg := LoadAlertConfig()
	if !cfg.IsEnabled() {
		t.Error("AlertConfig should be enabled when ALERT_WEBHOOK_URL is set")
	}
	if !cfg.OnFailed {
		t.Error("OnFailed should be true when ALERT_ON_FAILED=true")
	}
	if cfg.OnStuckThread {
		t.Error("OnStuckThread should be false by default")
	}
}

func TestAlertConfigBoolVariants(t *testing.T) {
	t.Setenv("ALERT_WEBHOOK_URL", "https://hooks.example.com/alerts")

	tests := []struct {
		val    string
		expect bool
	}{
		{"1", true},
		{"true", true},
		{"yes", true},
		{"0", false},
		{"false", false},
		{"no", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Setenv("ALERT_ON_FAILED", tc.val)
		cfg := LoadAlertConfig()
		if cfg.OnFailed != tc.expect {
			t.Errorf("ALERT_ON_FAILED=%q -> OnFailed=%v, want %v", tc.val, cfg.OnFailed, tc.expect)
		}
		// Reset for next iteration
		os.Unsetenv("ALERT_ON_FAILED")
	}
}

func TestDefaultStuckThreshold(t *testing.T) {
	if DefaultStuckThreshold != 30*60*1e9 { // 30 minutes in nanoseconds
		t.Errorf("DefaultStuckThreshold mismatch: %v", DefaultStuckThreshold)
	}
}

func TestSendAlert_Disabled(t *testing.T) {
	// No env var set -> disabled, should be a no-op
	cfg := AlertConfig{}
	cfg.SendAlert(context.Background(), AlertTaskFailed, map[string]any{"test": true})
	// If it doesn't panic, it succeeded
}

func TestSendAlert_Enabled(t *testing.T) {
	// Start a local HTTP server to receive the webhook
	received := make(chan AlertPayload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}
		var p AlertPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("failed to decode payload: %v", err)
			return
		}
		received <- p
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := AlertConfig{
		WebhookURL: srv.URL,
		OnFailed:   true,
	}
	cfg.SendAlert(context.Background(), AlertTaskFailed, map[string]any{"task_id": "test-123"})

	select {
	case p := <-received:
		if p.Type != AlertTaskFailed {
			t.Errorf("expected type %s, got %s", AlertTaskFailed, p.Type)
		}
	default:
		t.Error("no webhook received")
	}
}

func TestSendAlert_WithSecret(t *testing.T) {
	receivedSig := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedSig <- r.Header.Get("X-Webhook-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := AlertConfig{
		WebhookURL:    srv.URL,
		WebhookSecret: "my-secret-key",
		OnFailed:      true,
	}
	cfg.SendAlert(context.Background(), AlertTaskFailed, map[string]any{})

	select {
	case sig := <-receivedSig:
		if sig == "" {
			t.Error("expected X-Webhook-Signature header")
		}
		if len(sig) < 7 || sig[:7] != "sha256=" {
			t.Errorf("expected sha256= prefix, got %q", sig)
		}
	default:
		t.Error("no webhook received")
	}
}
