// Command sparkwing-web is the dashboard pod's entry point: an HTTP
// server that serves the embedded Next.js bundle and proxies /api/*
// to the controller and logs-service.
//
// Split from the `sparkwing` admin CLI (FOLLOWUPS binary-split
// 2026-04-22). k8s manifests invoke this binary directly. Humans
// typically use `sparkwing dev` (which spawns it locally) or browse
// to the deployed URL -- they don't type `sparkwing-web` by hand.
//
// Two runtime modes:
//
//   - Laptop-local: no --controller / --logs flags. Reader reads
//     local SQLite; LogSource reads local log files. Used by
//     `sparkwing dev` and by `wing` users running the dashboard
//     against their own state dir.
//   - Cluster: --controller + --logs set. Reader proxies to a remote
//     controller, LogSource proxies to the logs-service. --token
//     required when the upstream has auth enabled.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	flag "github.com/spf13/pflag"

	"github.com/sparkwing-dev/sparkwing-sdk/controller/client"
	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator"
	"github.com/sparkwing-dev/sparkwing-cli/internal/web"
	"github.com/sparkwing-dev/sparkwing-sdk/otelutil"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sparkwing-web:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("sparkwing-web", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:4343", "bind address")
	controllerURL := fs.String("controller", "", "controller URL to read from (default: local SQLite)")
	logsURL := fs.String("logs", "", "logs service URL (default: read log files from local disk)")
	token := fs.String("token", "", "controller bearer token (also SPARKWING_AGENT_TOKEN)")
	apiURL := fs.String("api-url", "", "public API URL injected into the dashboard (default: same origin)")
	requireLogin := fs.Bool("require-login", false,
		"redirect unauthed browsers to /login (prod). Leave off for laptop-local dev where the tokens table is empty and login would loop.")
	_ = fs.Parse(args)

	paths, err := orchestrator.DefaultPaths()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	tel := otelutil.Init(ctx, otelutil.Config{ServiceName: "sparkwing-web"})
	defer tel.Shutdown(context.Background())

	if *token == "" {
		*token = os.Getenv("SPARKWING_AGENT_TOKEN")
	}

	// Cluster-mode wiring: --controller swaps State reads to HTTP,
	// --logs swaps log reads to the sparkwing-logs service. Each is
	// independent; set both for a full cluster dashboard.
	if *controllerURL != "" || *logsURL != "" {
		if *controllerURL == "" {
			return fmt.Errorf("--logs requires --controller (dashboard needs node list from controller)")
		}
		var logSource web.LogSource
		if *logsURL != "" {
			// Pass the web pod's service token so the logs service
			// actually returns content; an unauthenticated request
			// comes back 401 and the dashboard renders "No logs
			// captured" even though the log is on disk.
			logSource = web.NewHTTPLogSource(*logsURL, *token)
		}
		// Reader needs the token too -- /api/runs on the web's local
		// mux delegates to Reader which hits controller /api/v1/runs,
		// gated by runs.read under FOLLOWUPS #2.
		var reader web.Reader
		if *token != "" {
			reader = client.NewWithToken(*controllerURL, nil, *token)
		} else {
			reader = client.New(*controllerURL, nil)
		}
		opts := web.HandlerOptions{
			Reader:        reader,
			Paths:         paths,
			LogSource:     logSource,
			ControllerURL: *controllerURL,
			LogsURL:       *logsURL,
			Token:         *token,
			APIURL:        *apiURL,
			RequireLogin:  *requireLogin,
		}
		return web.ServeWithOptions(ctx, opts, *addr)
	}

	// Local mode: simple handler over local Paths.
	return web.Serve(ctx, paths, *addr)
}
