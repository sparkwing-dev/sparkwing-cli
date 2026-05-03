// Package localws is the single-process local dev server: one HTTP
// server, one SQLite file, one port. Composes the controller,
// logs-service, and web handlers on the same mux so `wing` and the
// dashboard read from the same state without four cooperating
// daemons.
//
// Two callers:
//   - cmd/sparkwing-local-ws — standalone binary bootstrap
//   - cmd/sparkwing (dashboard.go) — the supervisor child spawned by
//     'sparkwing dashboard start' calls Run directly so the admin CLI
//     doesn't have to exec a sibling binary.
//
// Cluster-mode topology (sparkwing-controller / sparkwing-logs /
// sparkwing-web as distinct pods) is unchanged; this package only
// replaces the laptop four-process dance with one process.
package localws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/koreyGambill/sparkwing/pkg/controller"
	"github.com/sparkwing-dev/sparkwing-sdk/logs"
	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator"
	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator/store"
	"github.com/sparkwing-dev/sparkwing-cli/internal/web"
)

// Options configures the local dev server. Addr defaults to
// 127.0.0.1:4343; Home defaults to $SPARKWING_HOME or ~/.sparkwing.
// Ctx controls lifecycle; cancelling it initiates a graceful shutdown.
type Options struct {
	Addr string
	Home string
}

// Run starts the local dev server and blocks until ctx is cancelled
// or the HTTP server returns. Installs its own SIGINT/SIGTERM
// handler so standalone use (cmd/sparkwing-local-ws) has the same
// Ctrl-C behavior as a sparkwing-embedded caller.
//
// When called from the 'sparkwing dashboard' supervisor, the parent
// already wires signal handling and passes a ctx that cancels on
// SIGTERM; the inner NotifyContext just shadows that, no double-fire.
func Run(ctx context.Context, opts Options) error {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:4343"
	}

	// Resolve SPARKWING_HOME before anything opens files so the
	// dashboard supervisor and any cooperating laptop processes share
	// the same state dir as this server.
	if opts.Home != "" {
		if err := os.Setenv("SPARKWING_HOME", opts.Home); err != nil {
			return err
		}
	}
	paths, err := orchestrator.DefaultPaths()
	if err != nil {
		return fmt.Errorf("resolve sparkwing home: %w", err)
	}
	if err := paths.EnsureRoot(); err != nil {
		return fmt.Errorf("ensure %s: %w", paths.Root, err)
	}

	st, err := store.Open(paths.StateDB())
	if err != nil {
		return fmt.Errorf("open %s: %w", paths.StateDB(), err)
	}
	defer st.Close()

	// Logs rooted at paths.Root so the HTTP POST path and the direct
	// file reader used by web.diskLogSource point at identical files.
	logsSrv, err := logs.New(paths.Root, nil)
	if err != nil {
		return fmt.Errorf("logs server: %w", err)
	}

	// Auth: pass-through. controller.Server's authMiddleware is
	// disabled when no tokens are configured; logs.Server's is
	// disabled when controllerURL is empty. Both hold for local.
	ctrl := controller.New(st, nil)

	webOpts := web.HandlerOptions{
		Reader:    storeReader{st: st},
		Paths:     paths,
		LogSource: nil, // HandlerFromOptions defaults to diskLogSource
	}
	webHandler := web.HandlerFromOptions(webOpts)

	// Root mux composes the three handlers on one port. Go's
	// ServeMux picks the longest pattern, so:
	//   /api/v1/health/services -> web (aggregate across services)
	//   /api/v1/logs/...        -> logs-service
	//   /api/v1/...             -> controller (runs, triggers, health)
	//   /webhooks/...           -> controller (GitHub HMAC intake)
	//   /                       -> web (SPA, /api/runs)
	root := http.NewServeMux()
	root.Handle("/api/v1/health/services", webHandler)
	root.Handle("/api/v1/logs/", logsSrv.Handler())
	root.Handle("/api/v1/", ctrl.Handler())
	root.Handle("/webhooks/", ctrl.Handler())
	root.Handle("/", webHandler)

	// Export the base URL so cooperating processes (notably
	// 'sparkwing dashboard status' and laptop workers) can find us
	// without a hardcoded port.
	baseURL := "http://" + opts.Addr
	if err := writeDevEnv(paths.Root, baseURL); err != nil {
		return fmt.Errorf("write dev.env: %w", err)
	}

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// SSE log-stream endpoint holds writes open for the lifetime
		// of a run; a low WriteTimeout would cut tailing mid-run.
		IdleTimeout: 2 * time.Minute,
	}

	// Shadow the parent ctx with a NotifyContext so the standalone
	// bootstrap still catches SIGINT/SIGTERM cleanly. When called
	// from sparkwing, the parent ctx usually already cancels on
	// signal, but an extra handler is harmless.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// writeDevEnv records the base URL at $SPARKWING_HOME/dev.env so
// cooperating processes (notably the opt-in worker) can reach us.
// Written fresh each startup; stale files from a previous run on a
// different port get overwritten.
func writeDevEnv(root, baseURL string) error {
	body := fmt.Sprintf("SPARKWING_CONTROLLER_URL=%s\nSPARKWING_LOGS_URL=%s\n", baseURL, baseURL)
	return os.WriteFile(filepath.Join(root, "dev.env"), []byte(body), 0o644)
}

// storeReader adapts *store.Store to web.Reader for direct
// in-process reads. Mirrors the unexported adapter in the web
// package; kept local so that package's public surface doesn't have
// to widen just for this one consumer.
type storeReader struct{ st *store.Store }

func (r storeReader) ListRuns(ctx context.Context, f store.RunFilter) ([]*store.Run, error) {
	return r.st.ListRuns(ctx, f)
}
func (r storeReader) GetRun(ctx context.Context, id string) (*store.Run, error) {
	return r.st.GetRun(ctx, id)
}
func (r storeReader) ListNodes(ctx context.Context, runID string) ([]*store.Node, error) {
	return r.st.ListNodes(ctx, runID)
}
func (r storeReader) CancelRun(ctx context.Context, runID string) error {
	// The controller handles POST /api/v1/runs/{id}/cancel on the
	// same mux; the dashboard's /api/runs/{id}/cancel is vestigial
	// local-mode wiring that only fires when no controller is
	// mounted. Return the sentinel so the SPA renders 501 and uses
	// the /api/v1 route instead.
	return web.ErrCancelNotSupported
}
func (r storeReader) ListDebugPauses(ctx context.Context, runID string) ([]*store.DebugPause, error) {
	return r.st.ListDebugPauses(ctx, runID)
}
func (r storeReader) ReleaseDebugPause(ctx context.Context, runID, nodeID, releasedBy, kind string) error {
	return r.st.ReleaseDebugPause(ctx, runID, nodeID, releasedBy, kind)
}
func (r storeReader) ListEventsAfter(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error) {
	return r.st.ListEventsAfter(ctx, runID, afterSeq, limit)
}
