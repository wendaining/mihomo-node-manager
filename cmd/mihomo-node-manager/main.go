package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/local/mihomo-node-manager/internal/api"
	"github.com/local/mihomo-node-manager/internal/config"
	"github.com/local/mihomo-node-manager/internal/manager"
	"github.com/local/mihomo-node-manager/internal/mihomo"
	"github.com/local/mihomo-node-manager/internal/state"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "/etc/mihomo-node-manager/config.toml", "path to the TOML configuration")
		checkOnly   = flag.Bool("check-config", false, "validate configuration and exit")
		once        = flag.Bool("once", false, "run one probe and decision cycle, then exit")
		dryRun      = flag.Bool("dry-run", false, "do not switch nodes or persist state (requires --once)")
		clearFixed  = flag.Bool("clear-fixed", false, "clear Mihomo's fixed selection and exit")
		showVersion = flag.Bool("version", false, "print version information and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Printf("mihomo-node-manager %s commit=%s built=%s\n", version, commit, buildTime)
		return nil
	}
	if *dryRun && !*once {
		return errors.New("--dry-run requires --once")
	}
	if *clearFixed && (*once || *dryRun || *checkOnly) {
		return errors.New("--clear-fixed cannot be combined with --once, --dry-run or --check-config")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	secret, err := readSecret(cfg.Mihomo.SecretFile)
	if err != nil {
		return err
	}
	if *checkOnly {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Status          string `json:"status"`
			Config          string `json:"config"`
			AllowedNodes    int    `json:"allowed_nodes"`
			PingpongEnabled bool   `json:"pingpong_enabled"`
		}{Status: "ok", Config: *configPath, AllowedNodes: len(cfg.AllowedNodes), PingpongEnabled: cfg.Pingpong.Active()})
	}

	logger := newLogger(cfg.Logging.Level)
	client := mihomo.NewClient(cfg.Mihomo.BaseURL, secret, time.Duration(cfg.Mihomo.RequestTimeoutMS)*time.Millisecond)
	if *clearFixed {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Mihomo.RequestTimeoutMS)*time.Millisecond)
		defer cancel()
		if err := client.ClearFixed(ctx, cfg.Mihomo.Group); err != nil {
			return fmt.Errorf("clear fixed selection: %w", err)
		}
		logger.Info("fixed_selection_cleared", "group", cfg.Mihomo.Group)
		return nil
	}

	store := state.NewStore(cfg.StateFile)
	mgr := manager.New(cfg, client, store, logger, *dryRun)
	if *once {
		// Generous budget: with the ping-pong probe enabled, one cycle may
		// switch to and test several unproven candidates in turn.
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		cycleErr := mgr.RunCycle(ctx)
		if err := json.NewEncoder(os.Stdout).Encode(mgr.Snapshot()); err != nil {
			return err
		}
		return cycleErr
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := api.New(cfg.API.Listen, mgr, logger)
	errCh := make(chan error, 2)
	go func() { errCh <- mgr.Run(ctx) }()
	go func() { errCh <- server.ListenAndServe() }()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = err
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	logger.Info("service_stopped")
	return nil
}

func readSecret(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read mihomo.secret_file: %w", err)
	}
	secret := strings.TrimSpace(string(contents))
	if secret == "" {
		return "", errors.New("mihomo.secret_file is empty")
	}
	return secret, nil
}

func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slogLevel}))
}
