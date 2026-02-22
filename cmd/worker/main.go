package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"codenite/worker/internal/agent"
	"codenite/worker/internal/config"
	"codenite/worker/internal/providers/ai"
	"codenite/worker/internal/providers/tasksource"
	"codenite/worker/internal/providers/vcs"
)

func main() {
	configPath := flag.String("config", "config.json", "Path to config JSON")
	once := flag.Bool("once", false, "Run once and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	taskSource := tasksource.NewTodoistSource(cfg.TaskSource.Todoist.Token, cfg.TaskSource.Todoist.Label)
	aiProvider := ai.NewCodexProvider(cfg.AI.Command, cfg.AI.Env)
	vcsProvider := vcs.NewGitHubProvider(cfg.VCS.GitHub.Token, cfg.Worker.WorkRoot)

	worker := agent.NewWorker(cfg, taskSource, aiProvider, vcsProvider)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		if err := worker.RunOnce(ctx); err != nil {
			log.Fatalf("worker run failed: %v", err)
		}
		return
	}

	interval := time.Duration(cfg.Worker.PollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("worker started interval=%s", interval)
	for {
		if err := worker.RunOnce(ctx); err != nil {
			log.Printf("cycle failed: %v", err)
		}

		select {
		case <-ctx.Done():
			log.Println("worker stopping")
			return
		case <-ticker.C:
		}
	}
}
