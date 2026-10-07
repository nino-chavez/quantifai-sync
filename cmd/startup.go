package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/quantifai/sync/internal/config"
	"github.com/quantifai/sync/internal/credentials"
	"github.com/quantifai/sync/internal/health"
	"github.com/quantifai/sync/internal/logger"
)

// configRetryInterval is how often an agent that cannot start yet (bad
// config, no API key, sync turned off) checks its config again.
const configRetryInterval = 30 * time.Second

// startup is what the agent needs before it can sync.
type startup struct {
	cfg       config.Config
	keySource string
	log       *logger.Logger
}

// loadStartup loads and checks the config, resolves the API key and opens
// the logger. Every error is something the user fixes in settings.
func loadStartup() (startup, error) {
	cfg, err := config.Load("", "")
	if err != nil {
		return startup{}, fmt.Errorf("failed to load config: %w", err)
	}
	if err := config.Validate(cfg); err != nil {
		return startup{}, fmt.Errorf("invalid config: %w", err)
	}
	if !cfg.SyncEnabled {
		return startup{}, errors.New("sync_enabled is false")
	}
	// `install --api-key` stores the key in the OS keyring, so the agent
	// has to look there too; reading only the config left an installed
	// agent with no key and a 401 on every cycle.
	apiKey, keySource, err := credentials.NewManagerWithOSKeyring(cfg.APIKey).ResolveAPIKey()
	if err != nil {
		return startup{}, fmt.Errorf("no API key: run `quantifai-sync install --api-key <key>`, or set api_key in %s or QUANTIFAI_API_KEY", config.DefaultUserConfigPath())
	}
	cfg.APIKey = apiKey
	log, err := logger.New(logger.ParseLevel(cfg.LogLevel), cfg.LogFile)
	if err != nil {
		return startup{}, fmt.Errorf("failed to initialize logger: %w", err)
	}
	return startup{cfg: cfg, keySource: keySource, log: log}, nil
}

// waitFor calls load until it succeeds, every interval, and returns its
// result. It writes each new problem to out once rather than every try.
// It returns false if stop fires first.
//
// Exiting on a config problem made every service manager restart the
// agent into the same problem every 10 seconds: launchd's KeepAlive
// restarts on any exit, systemd's Restart=on-failure never reached its
// start limit, and the Windows supervisor restarts every error. Waiting
// stops that, and the agent starts by itself once the config is fixed.
func waitFor[T any](load func() (T, error), interval time.Duration, stop <-chan os.Signal, out io.Writer) (T, bool) {
	last := ""
	for {
		v, err := load()
		if err == nil {
			if last != "" {
				fmt.Fprintln(out, "config problem resolved; starting")
			}
			return v, true
		}
		if msg := err.Error(); msg != last {
			fmt.Fprintf(out, "%s; waiting for it to be fixed (checking every %s)\n", msg, interval)
			last = msg
		}
		select {
		case <-stop:
			var zero T
			return zero, false
		case <-time.After(interval):
		}
	}
}

// waitingHealth serves /health while the agent waits for a usable config,
// reporting the problem with status "error". Without it the health check
// and the tray saw a refused connection ("Not Running") for an agent that
// was running and waiting; service managers can only see that it runs.
type waitingHealth struct {
	state *health.HealthState
	srv   *health.Server
}

// report records the latest problem, starting the server on first use on
// the configured health port.
func (w *waitingHealth) report(err error) {
	if w.srv == nil {
		cfg, _ := config.Load("", "")
		w.state = health.NewHealthState(Version)
		w.srv = health.NewServer(cfg.HealthPort, w.state)
		go w.srv.ListenAndServe()
	}
	w.state.SetProblem(err.Error())
	notifyStatus("Waiting: " + err.Error())
}

// stop shuts the server down so the agent's own can take the port.
func (w *waitingHealth) stop() {
	if w.srv != nil {
		w.srv.Shutdown(context.Background())
	}
}

// load is loadStartup with each problem reported on /health.
func (w *waitingHealth) load() (startup, error) {
	st, err := loadStartup()
	if err != nil {
		w.report(err)
	}
	return st, err
}
