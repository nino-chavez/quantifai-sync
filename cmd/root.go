// Package cmd provides CLI subcommands for the quantifai-sync binary.
// The root command starts the agent pipeline: collector -> ingest
// batches -> sender, committing file offsets only after acknowledgment.  Subcommands (install, uninstall, version,
// healthcheck) are dispatched based on os.Args.
package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/quantifai/sync/internal/config"
	"github.com/quantifai/sync/internal/credentials"
	"github.com/quantifai/sync/internal/editor"
	gitpkg "github.com/quantifai/sync/internal/git"
	"github.com/quantifai/sync/internal/health"
	"github.com/quantifai/sync/internal/ingest"
	"github.com/quantifai/sync/internal/logger"
	"github.com/quantifai/sync/internal/parser"
	"github.com/quantifai/sync/internal/sender"
	"github.com/quantifai/sync/internal/state"
	"github.com/quantifai/sync/internal/updater"
)

// Execute is the main CLI entrypoint.  It parses os.Args to dispatch
// to the appropriate subcommand or runs the agent pipeline by default.
func Execute() int {
	if len(os.Args) < 2 {
		return runAgent()
	}

	switch os.Args[1] {
	case "version":
		return RunVersion()

	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		apiKey := fs.String("api-key", "", "API key for non-interactive install")
		fs.Parse(os.Args[2:])
		return RunInstall(*apiKey)

	case "uninstall":
		return RunUninstall()

	case "healthcheck":
		// Load config to get health port
		cfg, _ := config.Load("", "")
		return RunHealthcheck(cfg.HealthPort)

	case "run":
		// Explicit "run" subcommand (used by service managers)
		return runAgent()

	case "tray":
		return RunTray()

	case "git":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: quantifai-sync git [init|remove|list|hook-post-commit]\n")
			return 1
		}
		switch os.Args[2] {
		case "init":
			repoPath := "."
			if len(os.Args) > 3 {
				repoPath = os.Args[3]
			}
			return RunGitInit(repoPath)
		case "remove":
			repoPath := "."
			if len(os.Args) > 3 {
				repoPath = os.Args[3]
			}
			return RunGitRemove(repoPath)
		case "list":
			return RunGitList()
		case "hook-post-commit":
			return RunGitHookPostCommit()
		default:
			fmt.Fprintf(os.Stderr, "unknown git command: %s\n", os.Args[2])
			return 1
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		fmt.Fprintf(os.Stderr, "usage: quantifai-sync [version|install|uninstall|healthcheck|run|tray|git]\n")
		return 1
	}
}

// runAgent starts the full agent pipeline with graceful shutdown support.
func runAgent() int {
	// Load configuration
	cfg, err := config.Load("", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		return 1
	}

	if err := config.Validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		return 1
	}

	if !cfg.SyncEnabled {
		fmt.Fprintf(os.Stderr, "sync_enabled is false; exiting\n")
		return 0
	}

	// `install --api-key` stores the key in the OS keyring, so the agent
	// has to look there too; reading only the config left an installed
	// agent with no key and a 401 on every cycle.
	apiKey, keySource, err := credentials.NewManagerWithOSKeyring(cfg.APIKey).ResolveAPIKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no API key: run `quantifai-sync install --api-key <key>`, or set api_key in %s or QUANTIFAI_API_KEY\n", config.DefaultUserConfigPath())
		return 1
	}
	cfg.APIKey = apiKey

	// Initialize logger
	log, err := logger.New(logger.ParseLevel(cfg.LogLevel), cfg.LogFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer log.Close()

	log.Info("starting quantifai-sync", map[string]any{
		"version":        Version,
		"watch_dir":      cfg.WatchDir,
		"api_key_source": keySource,
	})

	// Start health server
	healthState := health.NewHealthState(Version)
	healthSrv := health.NewServer(cfg.HealthPort, healthState)
	// Register editor events endpoint for VS Code extension (queues locally)
	healthSrv.RegisterHandler("/api/v1/editor-events", editor.HandleEditorEvents(log))
	go healthSrv.ListenAndServe()
	defer healthSrv.Shutdown(context.Background())

	// Start background auto-updater
	u := updater.NewUpdater(cfg.AutoUpdate, Version, cfg.UpdateChannel, cfg.UpdateRepo, cfg.UpdateCheckInterval, log)

	// Initialize state manager
	stateMgr, err := state.NewManager(cfg.StateFile)
	if err != nil {
		log.Error("failed to load state", map[string]any{"error": err.Error()})
		return 1
	}
	pruned := stateMgr.Prune()
	if pruned > 0 {
		log.Info("pruned stale state entries", map[string]any{"count": pruned})
	}

	// Initialize sender
	snd, err := sender.New(cfg.APIURL, cfg.APIKey, log)
	if err != nil {
		log.Error("failed to create sender", map[string]any{"error": err.Error()})
		return 1
	}

	// Set up context with signal-based cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the updater background loop (no-op when auto_update=false)
	go u.Run(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	liteMode := parser.IsLiteKey(cfg.APIKey)
	scanInterval := time.Duration(cfg.FlushInterval) * time.Second
	collector := ingest.NewCollector(cfg.WatchDir, liteMode)

	// Degrade once three scan intervals pass without an acknowledged cycle,
	// but never sooner than the default.
	if stale := 3 * scanInterval; stale > health.DefaultStaleAfter {
		healthState.SetStaleAfter(stale)
	}

	log.Info("pipeline started", map[string]any{
		"watch_dir":     cfg.WatchDir,
		"api_url":       cfg.APIURL,
		"batch_size":    cfg.BatchSize,
		"scan_interval": cfg.FlushInterval,
		"health_port":   cfg.HealthPort,
		"lite_mode":     liteMode,
	})
	log.Info("editor events stay in the local queue: the server has no editor-events endpoint", nil)

	healthState.SetStatus(health.StatusOK)

	// Run first cycle immediately (it also reads every file once to rebuild
	// session totals in memory), then one cycle per scan interval.
	runCycle(ctx, cfg, collector, snd, stateMgr, healthState, liteMode, log)

	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runCycle(ctx, cfg, collector, snd, stateMgr, healthState, liteMode, log)

		case sig := <-sigCh:
			log.Info("received signal, shutting down", map[string]any{
				"signal": sig.String(),
			})
			cancel()
			return gracefulShutdown(stateMgr, log)

		case <-u.Applied():
			// Reached only between cycles, so no sync is cut off.
			if !restartsInPlace {
				log.Info("update installed; it takes effect when the agent next starts", nil)
				continue
			}
			log.Info("update installed, restarting into the new binary", nil)
			cancel()
			if code := gracefulShutdown(stateMgr, log); code != 0 {
				return code
			}
			err := restartInPlace()
			// A non-zero exit makes launchd (KeepAlive) and systemd
			// (Restart=on-failure) start the new binary instead.
			log.Error("restart into the new binary failed; exiting so the service manager restarts it", map[string]any{
				"error": err.Error(),
			})
			return 1
		}
	}
}

// largeCycleMessages is the cycle size after which memory is returned to
// the OS (the first cycle reads every session file once).
const largeCycleMessages = 10_000

// runCycle collects every session that gained messages since the committed
// offsets, sends them, and commits the new offsets only when every batch
// was acknowledged with counts that match what was sent. A failed cycle
// commits nothing; the next cycle re-reads from the same offsets, which is
// safe because the server dedups messages and replaces session totals.
func runCycle(
	ctx context.Context,
	cfg config.Config,
	collector *ingest.Collector,
	snd *sender.Sender,
	stateMgr *state.Manager,
	healthState *health.HealthState,
	liteMode bool,
	log *logger.Logger,
) bool {
	ok, messages := syncOnce(ctx, cfg, collector, snd, stateMgr, healthState, liteMode, log)
	if messages >= largeCycleMessages {
		// A backfill decodes gigabytes of JSON. syncOnce has returned, so
		// that data is unreachable now; hand it back rather than holding it
		// for the life of a background agent, on failure as well.
		debug.FreeOSMemory()
	}
	return ok
}

// syncOnce does one cycle and reports whether it was fully acknowledged and
// how many messages it handled.
func syncOnce(
	ctx context.Context,
	cfg config.Config,
	collector *ingest.Collector,
	snd *sender.Sender,
	stateMgr *state.Manager,
	healthState *health.HealthState,
	liteMode bool,
	log *logger.Logger,
) (bool, int) {
	started := time.Now()
	cyc := collector.Collect(func(path string) int64 { return stateMgr.Get(path).ByteOffset })
	messages := cyc.Messages()
	healthState.SetFilesTracked(cyc.Files)
	healthState.SetRecordsBuffered(messages)
	for path, err := range cyc.ReadErrors {
		log.Warn("failed to read session file", map[string]any{"path": path, "error": err.Error()})
	}

	// Commit events are independent of session files, so a stuck session
	// batch must not hold them back.
	if cfg.GitEnabled && cfg.APIKey != "" {
		if n := gitpkg.FlushCommitQueue(ctx, "", snd.Send, liteMode, log); n > 0 {
			log.Info("commit events stored", map[string]any{"count": n})
		}
	}

	// A batch the server keeps rejecting stalls the cycle here on purpose:
	// nothing is committed, /health turns degraded, and the error is logged
	// every cycle. Stalling loudly is the alternative to losing data quietly.
	batches := ingest.BuildBatches(cyc.Groups, cfg.BatchSize)
	for i, b := range batches {
		if _, err := snd.Send(ctx, b); err != nil {
			log.Error("sync cycle failed; offsets not advanced", map[string]any{
				"batch":    i + 1,
				"batches":  len(batches),
				"sessions": len(b.Sessions),
				"messages": len(b.Messages),
				"error":    err.Error(),
			})
			return false, messages
		}
	}

	for path, off := range cyc.Offsets {
		stateMgr.Set(path, state.FileState{ByteOffset: off})
	}
	if len(cyc.Offsets) > 0 {
		if err := stateMgr.Save(); err != nil {
			log.Error("batches stored but offsets not saved; they will be re-sent", map[string]any{"error": err.Error()})
			return false, messages
		}
	}

	healthState.SetLastSyncTime(time.Now())
	healthState.SetRecordsBuffered(0)
	if len(batches) > 0 {
		log.Info("sync cycle complete", map[string]any{
			"batches":     len(batches),
			"sessions":    len(cyc.Groups),
			"messages":    messages,
			"duration_ms": time.Since(started).Milliseconds(),
		})
	}
	return true, messages
}

// gracefulShutdown persists committed offsets. Offsets only ever reach the
// state manager after the server acknowledged their batches, so nothing
// read but unsent can be marked done here.
func gracefulShutdown(stateMgr *state.Manager, log *logger.Logger) int {
	if err := stateMgr.Save(); err != nil {
		log.Error("failed to save state during shutdown", map[string]any{"error": err.Error()})
		return 1
	}
	log.Info("shutdown complete", nil)
	return 0
}
