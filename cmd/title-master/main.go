package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/userreksai/title-master/internal/master"
	"github.com/userreksai/title-master/internal/settings"
)

func main() {
	path := flag.String("config", "master.json", "JSON configuration path")
	once := flag.Bool("once", false, "run one round; persist notifications for the daemon to deliver")
	check := flag.Bool("check-config", false, "validate config and exit")
	printStatePath := flag.Bool("print-state-path", false, "validate config and print the absolute state file path")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := settings.ReadMaster(*path)
	if err != nil {
		logger.Error("configuration", "error", err)
		os.Exit(1)
	}
	if *printStatePath {
		statePath, err := filepath.Abs(cfg.StateFile)
		if err != nil {
			logger.Error("state path", "error", err)
			os.Exit(1)
		}
		fmt.Println(statePath)
		return
	}
	if *check {
		logger.Info("configuration valid")
		return
	}
	if len(cfg.Webhooks.DomainURLs) == 0 {
		logger.Warn("domain webhooks empty; changes are recorded without notifications")
	}
	if len(cfg.Webhooks.MachineURLs) == 0 {
		logger.Warn("machine webhooks empty; machine failures will not be notified")
	}
	repo, err := master.OpenRepository(cfg.StateFile)
	if err != nil {
		logger.Error("state", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = master.NewRunner(cfg, repo, logger).Run(ctx, *once)
	_ = repo.Close()
	if err != nil && ctx.Err() == nil {
		logger.Error("master stopped", "error", err)
		os.Exit(1)
	}
}
