package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/buildinfo"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/containeropsagent"
)

func main() {
	addr := env("CPAMP_AGENT_ADDR", "0.0.0.0:18417")
	version := env("VERSION", buildinfo.Version)
	log.Printf("cpamp-agent version=%s commit=%s builtAt=%s", version, buildinfo.Commit, buildinfo.BuildDate)
	serverApp, err := containeropsagent.NewServer(containeropsagent.ServerOptions{
		ServiceID:  "cpamp-agent",
		Version:    version,
		DockerHost: env("DOCKER_HOST", "unix:///var/run/docker.sock"),
		Token:      env("CPAMP_AGENT_TOKEN", ""),
		StackRoot:  env("CPAMP_STACK_ROOT", "/opt/cpamp/stacks/cpa"),
		BackupRoot: env("CPAMP_BACKUP_ROOT", "/opt/cpamp/backups"),
	})
	if err != nil {
		log.Fatalf("initialize agent: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:              addr,
		Handler:           serverApp.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("cpamp-agent listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func env(key string, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}
