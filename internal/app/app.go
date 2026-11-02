// Package app wires the daemon together.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alex/disk_resource_scheduler/internal/config"
	"github.com/alex/disk_resource_scheduler/internal/disk"
	"github.com/alex/disk_resource_scheduler/internal/scheduler"
	"github.com/alex/disk_resource_scheduler/internal/web"
	"github.com/robfig/cron/v3"
)

// App is the running daemon.
type App struct {
	cfgPath string
	cfg     *config.Store
	log     *slog.Logger
	probe   disk.Probe
	sched   *scheduler.Scheduler
	srv     *web.Server
	history *scheduler.History

	cron *cron.Cron
	ln   net.Listener
	http *http.Server
	// cancel stops the goroutines the daemon started.
	cancel context.CancelFunc

	poll time.Duration
	mu   sync.Mutex
}

// httpShutdownTimeout is how long the web interface may take to finish the
// requests in flight when the daemon stops.
const httpShutdownTimeout = 2 * time.Second

// Option changes a part of the daemon before it starts.
type Option func(*App)

// WithProbe replaces the way the volumes are measured.
func WithProbe(probe disk.Probe) Option {
	return func(a *App) { a.probe = probe }
}

// WithPollInterval sets how often the config file is checked for changes.
func WithPollInterval(d time.Duration) Option {
	return func(a *App) { a.poll = d }
}

// New builds the daemon from a config file.
func New(cfgPath string, opts ...Option) (*App, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	logger, err := newLogger(cfg.Log)
	if err != nil {
		return nil, err
	}

	daemon := &App{
		cfgPath: cfgPath,
		cfg:     config.NewStore(cfg),
		log:     logger,
		probe:   disk.StatfsProbe{},
		poll:    10 * time.Second,
	}
	for _, opt := range opts {
		opt(daemon)
	}

	daemon.history = scheduler.NewHistory(cfg.State.Dir)
	daemon.sched = scheduler.New(daemon.cfg, logger, daemon.probe,
		scheduler.NewStateStore(cfg.State.Dir), daemon.history)
	daemon.srv, err = web.New(daemon.cfg, logger, daemon.sched, daemon.history)
	if err != nil {
		return nil, err
	}
	return daemon, nil
}

// Start brings the daemon up.
func (a *App) Start(ctx context.Context) error {
	if err := a.sched.Start(ctx); err != nil {
		return err
	}

	listen := a.cfg.Load().Server.Listen
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", listen, err)
	}
	a.ln = listener
	a.http = &http.Server{
		Handler:           a.srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := a.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("the web server stopped", "error", err)
		}
	}()

	if err := a.startCron(a.cfg.Load().Scan.Schedule); err != nil {
		return err
	}
	base, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	go a.watchConfig(base)

	a.log.Info("the daemon is up", "listen", listener.Addr().String(), "folders", len(a.cfg.Load().Folders))
	return nil
}

// Stop shuts the daemon down.
//
// The stop timeout has to be longer than the longest copy: a copy that is cut
// in the middle leaves the source where it was, but the daemon should not be
// killed while it is still writing.
func (a *App) Stop(ctx context.Context) error {
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Lock()
	if a.cron != nil {
		a.cron.Stop()
		a.cron = nil
	}
	a.mu.Unlock()

	if a.http != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, httpShutdownTimeout)
		defer cancel()
		if err := a.http.Shutdown(shutdownCtx); err != nil {
			// A client that left a connection open keeps the shutdown waiting
			// for it; the daemon is going away anyway.
			a.http.Close()
		}
	}
	return a.sched.Stop(ctx)
}

// Addr is the address the web interface listens on.
func (a *App) Addr() string {
	if a.ln == nil {
		return ""
	}
	return a.ln.Addr().String()
}

func (a *App) startCron(expr string) error {
	c := cron.New()
	if _, err := c.AddFunc(expr, func() {
		if err := a.sched.Scan(context.Background()); err != nil {
			a.log.Error("the scan failed", "error", err)
		}
	}); err != nil {
		return fmt.Errorf("scan.schedule: want a cron expression, got %q", expr)
	}
	c.Start()

	a.mu.Lock()
	a.cron = c
	a.mu.Unlock()
	return nil
}

// watchConfig reloads the config when the file changes.
//
// The file is polled rather than watched: a config file changes a few times
// a year, and fsnotify would be a dependency for a timer.
func (a *App) watchConfig(ctx context.Context) {
	ticker := time.NewTicker(a.poll)
	defer ticker.Stop()

	stamp := a.configStamp()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := a.configStamp()
			if current == stamp {
				continue
			}
			stamp = current
			a.reload()
		}
	}
}

// configStamp tells one version of the config file from another.
func (a *App) configStamp() string {
	info, err := os.Stat(a.cfgPath)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

// reload reads the config again and puts it in force.
//
// A config that does not parse leaves the running one alone: a broken file
// must not take the daemon down.
func (a *App) reload() {
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		a.log.Warn("the config is not usable, keeping the old one", "error", err)
		return
	}

	old := a.cfg.Load()
	a.cfg.Store(cfg)
	a.log.Info("the config was reloaded", "folders", len(cfg.Folders))

	if cfg.Server.Listen != old.Server.Listen {
		a.log.Warn("the listen address changed, the daemon has to be restarted",
			"was", old.Server.Listen, "now", cfg.Server.Listen)
	}
	if cfg.State.Dir != old.State.Dir {
		a.log.Warn("the state directory changed, the daemon has to be restarted",
			"was", old.State.Dir, "now", cfg.State.Dir)
	}

	if _, err := cron.ParseStandard(cfg.Scan.Schedule); err != nil {
		a.log.Warn("the new schedule is not usable, keeping the old one", "error", err)
		return
	}
	a.mu.Lock()
	if a.cron != nil {
		a.cron.Stop()
		a.cron = nil
	}
	a.mu.Unlock()
	if err := a.startCron(cfg.Scan.Schedule); err != nil {
		a.log.Warn("the new schedule is not usable, keeping the old one", "error", err)
	}
}

// newLogger builds the logger the daemon writes with.
func newLogger(cfg config.Log) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("log.level: want debug, info, warn or error, got %q", cfg.Level)
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("log.format: want text or json, got %q", cfg.Format)
	}
	return slog.New(handler), nil
}
