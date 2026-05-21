package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/cmd/webui/internal/api"
	"github.com/noodle05/ai-agents/cmd/webui/internal/env"
	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

func main() {
	logLevelFlag := flag.String("log-level", "", "log level (debug, info, warn, error)")
	logAccessFlag := flag.Bool("log-access", false, "log HTTP requests to stderr")
	flag.Parse()

	levelStr := *logLevelFlag
	if levelStr == "" {
		levelStr = os.Getenv("LOG_LEVEL")
	}

	logLevel := new(slog.LevelVar)
	switch strings.ToLower(levelStr) {
	case "debug":
		logLevel.Set(slog.LevelDebug)
	case "warn", "warning":
		logLevel.Set(slog.LevelWarn)
	case "error":
		logLevel.Set(slog.LevelError)
	default:
		logLevel.Set(slog.LevelInfo)
	}
	replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.LevelKey {
			return slog.String("level", strings.ToLower(a.Value.String()))
		}
		if a.Key == slog.TimeKey {
			return slog.String("ts", a.Value.Time().UTC().Format(time.RFC3339))
		}
		return a
	}
	handlerOpts := &slog.HandlerOptions{
		Level:       logLevel,
		ReplaceAttr: replaceAttr,
	}
	appHandler := slog.NewJSONHandler(os.Stderr, handlerOpts)
	log := slog.New(appHandler).With("component", "webui")
	slog.SetDefault(log)

	accessEnabled := *logAccessFlag || strings.ToLower(os.Getenv("LOG_ACCESS")) == "true"

	newAccessLogger := func() *slog.Logger {
		handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})
		return slog.New(handler)
	}

	var accessLog atomic.Pointer[slog.Logger]
	if accessEnabled {
		accessLog.Store(newAccessLogger())
	}

	adminAPIKey := env.String("ADMIN_API_KEY", os.Getenv("WEBUI_API_KEY"))
	api.SetAdminKey(adminAPIKey)

	cfg := request.DefaultConfig()
	port := env.String("WEBUI_PORT", "8000")

	// Redis connection
	redisHost := env.String("REDIS_HOST", "redis")
	redisPort := env.String("REDIS_PORT", "6379")
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", redisHost, redisPort),
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Error("redis connection failed", "error", err)
		os.Exit(1)
	}
	log.Info("connected to redis", "host", redisHost, "port", redisPort)

	client := tasklib.NewClient(rdb)
	reqHandler := request.New(client, cfg)

	// Template renderer
	renderer, err := templates.New()
	if err != nil {
		log.Error("template init failed", "error", err)
		os.Exit(1)
	}
	log.Info("templates loaded", "theme", renderer.Theme)

	// Background context for rate limiter cleanup goroutines.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	// Background alert monitor for stuck threads and lost heartbeats
	alertCfg := tasklib.LoadAlertConfig()
	if alertCfg.IsEnabled() {
		go runAlertMonitor(bgCtx, rdb, alertCfg)
	}

	// Build chi router with page routes, API endpoints, and static files
	router := api.NewRouter(client, reqHandler, renderer, bgCtx, &accessLog, adminAPIKey, newAccessLogger)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig)

		bgCancel() // stop rate limiter cleanup goroutines

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()

		reqHandler.Shutdown(shutdownCtx)
		srv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "port", port, "max_concurrent", cfg.MaxConcurrent)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
	log.Info("server stopped")
}
