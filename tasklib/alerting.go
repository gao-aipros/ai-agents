package tasklib

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// AlertType categorises the webhook alert.
type AlertType string

const (
	AlertTaskFailed    AlertType = "task_failed"
	AlertThreadStuck   AlertType = "thread_stuck"
	AlertWorkerLost    AlertType = "worker_lost"
)

// AlertPayload is the JSON body POSTed to the webhook URL.
type AlertPayload struct {
	Type      AlertType  `json:"type"`
	Timestamp string     `json:"timestamp"`
	Detail    any        `json:"detail"`
}

// AlertConfig is read from environment variables once at startup.
type AlertConfig struct {
	WebhookURL     string
	WebhookSecret  string
	OnFailed       bool
	OnStuckThread  bool
	OnWorkerLost   bool
	StuckThreshold time.Duration
	WorkerLostThreshold time.Duration
}

// LoadAlertConfig reads alerting configuration from environment variables.
func LoadAlertConfig() AlertConfig {
	cfg := AlertConfig{
		WebhookURL:          os.Getenv("ALERT_WEBHOOK_URL"),
		WebhookSecret:       os.Getenv("WEBHOOK_SECRET"),
		OnFailed:            envBool("ALERT_ON_FAILED", false),
		OnStuckThread:       envBool("ALERT_ON_STUCK_THREAD", false),
		OnWorkerLost:        envBool("ALERT_ON_WORKER_LOST", false),
		StuckThreshold:      30 * time.Minute,
		WorkerLostThreshold: 60 * time.Second,
	}
	if v := os.Getenv("ALERT_STUCK_THRESHOLD_MINUTES"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.StuckThreshold = time.Duration(n) * time.Minute
		}
	}
	return cfg
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(os.Getenv(key))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes"
}

// IsEnabled returns true when a webhook URL is configured.
func (c AlertConfig) IsEnabled() bool { return c.WebhookURL != "" }

// SendAlert dispatches a fire-and-forget POST to the configured webhook URL.
// Errors are logged but never returned — alerting is best-effort.
func (c AlertConfig) SendAlert(alertType AlertType, detail any) {
	if !c.IsEnabled() {
		return
	}
	payload := AlertPayload{
		Type:      alertType,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Detail:    detail,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("alert marshal error", "type", alertType, "error", err)
		return
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.WebhookURL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("alert request error", "type", alertType, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	if c.WebhookSecret != "" {
		mac := hmac.New(sha256.New, []byte(c.WebhookSecret))
		mac.Write(body)
		req.Header.Set("X-Webhook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("alert post failed", "type", alertType, "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Warn("alert rejected", "type", alertType, "status", resp.StatusCode)
	}
}
