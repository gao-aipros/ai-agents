package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"

	"github.com/noodle05/ai-agents/cmd/webui/internal/api"
	"github.com/noodle05/ai-agents/cmd/webui/internal/env"
	"github.com/noodle05/ai-agents/cmd/webui/internal/request"
	"github.com/noodle05/ai-agents/cmd/webui/internal/templates"
	"github.com/noodle05/ai-agents/tasklib"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[webui] ")

	cfg := request.DefaultConfig()
	port := env.String("WEBUI_PORT", "8000")

	// Redis connection
	redisHost := env.String("REDIS_HOST", "redis")
	redisPort := env.String("REDIS_PORT", "6379")
	rdb := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%s", redisHost, redisPort),
	})

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis connection failed: %v", err)
	}
	log.Printf("connected to redis at %s:%s", redisHost, redisPort)

	client := tasklib.NewClient(rdb)
	handler := request.New(client, cfg)

	// Template renderer
	renderer, err := templates.New()
	if err != nil {
		log.Fatalf("template init failed: %v", err)
	}
	log.Printf("templates loaded (theme=%s)", renderer.Theme)

	// Background context for rate limiter cleanup goroutines.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	// Build chi router with page routes, API endpoints, and static files
	router := api.NewRouter(client, handler, renderer, bgCtx)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("received signal %v, shutting down", sig)

		bgCancel() // stop rate limiter cleanup goroutines

		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()

		handler.Shutdown(shutdownCtx)
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on :%s (max concurrent requests: %d)", port, cfg.MaxConcurrent)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	log.Printf("server stopped")
}
