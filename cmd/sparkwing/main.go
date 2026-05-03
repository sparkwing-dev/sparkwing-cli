// The sparkwing binary. When invoked as "wing" (symlink or renamed
// copy) it dispatches to the repo's local .sparkwing/ pipeline
// runner. Otherwise it exposes infrastructure and observation
// subcommands (today: jobs list/status/logs).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	flag "github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/sparkwing-dev/sparkwing-cli/pkg/color"
	"github.com/sparkwing-dev/sparkwing-sdk/controller/client"
	"github.com/sparkwing-dev/sparkwing-sdk/logs"
	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator"
	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator/store"
	"github.com/sparkwing-dev/sparkwing-cli/pkg/pipelines"
	"github.com/sparkwing-dev/sparkwing-sdk/repos"
	"github.com/sparkwing-dev/sparkwing-cli/pkg/wingconfig"
)

func main() {
	// Best-effort cleanup of a sparkwing.exe.old left behind by a previous
	// Windows self-update -- the old binary can't be deleted while running,
	// so deletion is deferred to the next launch. No-op on POSIX.
	cleanupStaleUpdate()

	base := filepath.Base(os.Args[0])
	if base == "wing" {
		if err := runWing(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, color.Red(color.Bold("wing error:")), err)
			os.Exit(exitCodeFor(err))
		}
		return
	}
	if err := runSparkwing(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, color.Red(color.Bold("sparkwing error:")), err)
		os.Exit(exitCodeFor(err))
	}
}

// cliError carries an explicit exit code so verbs like `jobs wait` and
// `jobs status` can distinguish "ran successfully, outcome = failed"
// (exit 1) from "timed out" (exit 2) from "infrastructure error"
// (exit 3+). Wrap errors with exitErrorf / newExitError to propagate
// codes through the normal error-return path.
type cliError struct {
	code int
	err  error
}

func (e *cliError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *cliError) Unwrap() error { return e.err }

// exitErrorf wraps a formatted error with the given exit code.
func exitErrorf(code int, format string, args ...any) error {
	return &cliError{code: code, err: fmt.Errorf(format, args...)}
}

// exitError wraps an existing error with the given exit code.
func exitError(code int, err error) error {
	if err == nil {
		return nil
	}
	return &cliError{code: code, err: err}
}

// exitCodeFor resolves the exit code an error should cause main to
// return. Defaults to 1 when the error is not a cliError.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var ce *cliError
	if errors.As(err, &ce) {
		if ce.code == 0 {
			return 1
		}
		return ce.code
	}
	return 1
}

// runWing implements `wing <pipeline> [flags...]`. It locates the
// enclosing .sparkwing/ directory, strips wing-owned flags from the
// arg stream (parseWingFlags), optionally re-roots on a different
// git ref (--from), then compiles + execs the user's pipeline
// binary with the remaining args.
//
// Wing-owned flags today:
//
//	--from REF     compile from a different git ref via `git worktree add`
//	--on NAME      [RESERVED] dispatch remotely; not yet wired
//
// `-h` / `--help` as the first arg (before the pipeline name) prints
// the shared `sparkwing run` help page. `wing <pipeline> --help`
// cannot short-circuit here because pipeline flags are parsed by the
// user's compiled binary, not by us -- we pass it through.
func runWing(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		if len(args) == 0 {
			PrintHelp(cmdWing, os.Stderr)
			return errors.New("wing: pipeline name required")
		}
		PrintHelp(cmdWing, os.Stdout)
		return nil
	}

	pipelineName := args[0]
	wf, passthrough := parseWingFlags(args[1:])

	// Resolve the nearest .sparkwing/ first -- both the config-preset
	// layering and the compile+exec path need it, and we want consistent
	// "no .sparkwing/" errors regardless of which path fires.
	//
	// `-C <path>` re-anchors discovery: `wing -C ../app2 deploy` walks
	// up from ../app2 instead of cwd. Same shape as `git -C` and
	// `make -C`. compileAndExec / ExecReplace then chdir into the
	// resolved .sparkwing/ before exec, and the SDK's walk-up picks
	// up the right repo root from there -- no cross-coordination
	// needed beyond cwd.
	var dir string
	var err error
	if wf.changeDir != "" {
		dir, err = findSparkwingDirFrom(wf.changeDir)
	} else {
		dir, err = findSparkwingDir()
	}
	if err != nil {
		return err
	}

	// Auto-register this checkout in the repo registry so cross-repo
	// AwaitPipelineJob calls from elsewhere can resolve "pipeline X"
	// to a local path without a hardcoded WithAwaitRepo annotation.
	// Worktree checkouts are skipped by default (see pkg/repos);
	// SPARKWING_NO_AUTO_REGISTER=1 opts out entirely. Errors are
	// dropped because auto-register is QoL -- a read-only home dir
	// or transient FS hiccup shouldn't break `wing <pipeline>`.
	_ = repos.AutoRegister(filepath.Dir(dir))

	// --config PRESET layers wing flags from config.yaml onto the
	// explicit flags. Explicit flags always win, so a preset sets
	// the defaults and the operator overrides per-invocation.
	if wf.config != "" {
		preset, found, perr := wingconfig.Resolve(dir, wf.config)
		if perr != nil {
			return fmt.Errorf("--config %s: %w", wf.config, perr)
		}
		if !found {
			return fmt.Errorf("--config %s: preset not found in .sparkwing/config.yaml or ~/.config/sparkwing/config.yaml", wf.config)
		}
		if wf.on == "" {
			wf.on = preset.On
		}
		if wf.from == "" {
			wf.from = preset.From
		}
	}

	// --on dispatches against a remote controller via CreateTrigger
	// and short-circuits the local compile+exec path. Pipe --from
	// through as the git branch on the trigger; --arg KEY=VALUE
	// entries in passthrough become the Args map.
	if wf.on != "" {
		return dispatchRemote(pipelineName, wf, passthrough)
	}

	// --from re-roots compilation on a git worktree for the requested
	// ref. The cleanup runs regardless of how compileAndExec returns --
	// defer lets git reclaim the worktree entry on success and failure.
	if wf.from != "" {
		_, sparkwingSub, cleanup, err := setupFromRef(dir, wf.from)
		if err != nil {
			return fmt.Errorf("wing: --from %s: %w", wf.from, err)
		}
		defer cleanup()
		dir = sparkwingSub
	}

	// The pipeline binary self-locates the repo root via walk-up to
	// `.sparkwing/` at SDK init -- no env-var handoff. compileAndExec
	// chdir's into the .sparkwing dir before exec, so walk-up from
	// there finds the parent (the repo root) on its first ascent.
	env := os.Environ()
	// --verbose flips the orchestrator's log-level env var so debug
	// records surface in the TTY / JSON stream.
	if wf.verbose {
		env = append(env, "SPARKWING_LOG_LEVEL=debug")
	}
	// --retry-of / --full plumb through an env-var channel because
	// the pipeline binary parses its own Options inside
	// orchestrator.Main; wing_flags already stripped them from the
	// passthrough args. Same pattern as SPARKWING_LOG_LEVEL and
	// SPARKWING_PAUSE_TIMEOUT: wing-owned knobs ride env vars the
	// binary reads at Options-build time.
	if wf.retryOf != "" {
		env = append(env, "SPARKWING_RETRY_OF="+wf.retryOf)
		if wf.fullRetry {
			env = append(env, "SPARKWING_RETRY_FULL=1")
		}
	}
	// --secrets PROF (REG-017 phase 4): the local pipeline binary
	// resolves sparkwing.Secret(...) against the named profile's
	// controller instead of the laptop dotenv. Plumbed via env var
	// for the same reason as SPARKWING_RETRY_OF: the binary builds
	// its own Options inside orchestrator.Main.
	if wf.secrets != "" {
		env = append(env, "SPARKWING_SECRETS_PROFILE="+wf.secrets)
	}

	return compileAndExec(dir, append([]string{pipelineName}, passthrough...), env,
		compileOptions{NoUpdate: wf.noUpdate})
}

// runSparkwing dispatches `sparkwing <subcommand>`. For milestone 1
// that's only `jobs`; further subcommands (cluster, hooks, web) come
// in subsequent phases.
func runSparkwing(args []string) error {
	if len(args) == 0 {
		PrintHelp(cmdSparkwing, os.Stderr)
		os.Exit(2)
	}
	switch args[0] {
	// === PRIMARY AGENT + OPERATOR SURFACE ===
	case "info":
		// `sparkwing info` is the agent entrypoint: self-describe
		// CLI + current project + toolchain + next-step commands so
		// an agent that has just downloaded the binary can pick up
		// from one invocation rather than multiple discovery probes.
		return runInfo(args[1:])
	case "pipeline":
		// `sparkwing pipeline <verb>` is the per-project namespace:
		// list / describe / discover / new / explain / run, with
		// hooks (git pre-commit / pre-push) and sparks (library
		// management) nested.
		return runPipeline(args[1:])
	case "run":
		// `sparkwing run <pipeline>` is the top-level invoke verb.
		// Takes the pipeline name as a positional (the deliberate
		// exception to an otherwise flag-only sparkwing surface)
		// because run is the hot-path verb. Identical behavior to
		// `wing <pipeline>`.
		return runRun(args[1:])
	case "runs":
		// `sparkwing runs <verb>` covers the run lifecycle: list /
		// status / logs / retry / cancel / prune (plus the nested
		// approvals and triggers surfaces).
		return runJobs(args[1:])

	case "dashboard":
		// `sparkwing dashboard {start,kill,status}` - background
		// lifecycle for the laptop-local dashboard + API server. The
		// server is for visualization only; everything it shows is
		// reachable via the CLI as well.
		return runDashboard(args[1:])

	// === CLUSTER OPS ===
	case "cluster":
		// Cluster-scoped operations: status / agents / worker / gc /
		// push plus cluster-stored state (users, tokens, image
		// rollouts, webhooks).
		return runCluster(args[1:])
	case "secrets":
		// Controller-stored (or laptop-dotenv) secrets: get / list /
		// set / delete. Top-level because pipelines reference secrets
		// by name; users reach for them constantly and they're not a
		// cluster-only concern (local-mode writes the laptop dotenv).
		return runSecret(args[1:])

	// === LAPTOP CONFIG ===
	case "configure":
		// Laptop-local config: profiles (the control plane for every
		// --on dispatch).
		return runConfigure(args[1:])
	// === CLI META (top-level — low surface, high use) ===
	case "completion":
		return runCompletion(args[1:])
	case "docs":
		// Embedded /docs/ tree shipped with the binary. Agent-first:
		// `docs read --topic NAME` returns raw markdown, `docs all`
		// dumps the full corpus, `docs list --json` is the catalog.
		return runDocs(args[1:])
	case "commands":
		// Full CLI surface as JSON. One probe = the entire verb tree
		// (path, synopsis, flags, examples) so agents can self-
		// discover without scraping per-verb help text.
		return runCommands(args[1:])
	case "update":
		// `sparkwing update` is the binary self-update fast-path.
		// No --cli/--sdk split; always updates the running binary.
		// For SDK (go.mod) bumps, use `sparkwing version update --sdk`.
		return runUpdate(args[1:])
	case "version", "--version", "-V":
		// kubectl-style version surface: CLI + latest release +
		// per-project SDK / sparks pins, with semver-compare behind
		// detection. --version / -V are aliases so the typical
		// `<cli> --version` instinct works.
		return runVersion(args[1:])

	// === DEVELOPER DEBUGGING ===
	case "debug":
		// REG-013: interactive debugging surface (pause / release /
		// attach / env). Kept out of wing so production runs can
		// never carry a debug directive. Kept at the top level
		// because debugging crosses cluster/pipelines boundaries.
		return runDebug(args[1:])

	// === INTERNAL EXEC PROTOCOL ===
	case "handle-trigger":
		// Adopt an already-claimed trigger and run it end-to-end.
		// The warm-runner's trigger loop exec's this after compiling
		// the consumer's .sparkwing/ binary.
		return runWing(args)
	case "__dashboard-supervise":
		// Hidden body of the detached dashboard child. `sparkwing
		// dashboard start` exec's this after fork+setsid. Owns the
		// PID file and runs localws.Run until SIGTERM.
		return runDashboardSupervise(args[1:])
	case "_complete-profiles":
		// Hidden helper called by the shell completion functions to
		// enumerate profile names. Output is one name per line so
		// bash/zsh/fish can slurp directly. Intentionally undocumented
		// in the user-facing usage banner.
		return runInternalCompleteProfiles(args[1:])
	case "_complete-pipelines":
		// Hidden helper: enumerate pipelines registered in the nearest
		// .sparkwing/ for 'sparkwing run <TAB>' and 'wing <TAB>'.
		// Output is tab-separated "name\tdescription" so zsh renders
		// the trigger + tag summary alongside each pipeline.
		return runInternalCompletePipelines(args[1:])
	case "_complete-flags":
		// Hidden helper: emit "--flag\tdescription" for the leaf
		// command whose argv path equals args[1:]. Feeds the shell
		// completion when the current word starts with '--'.
		return runInternalCompleteFlags(args[1:])
	case "_complete-verbs":
		// Hidden helper: emit "name\tsynopsis" for the subcommands
		// of the parent command at args[1:] (empty = top level).
		return runInternalCompleteVerbs(args[1:])
	case "_complete-hint":
		// Hidden helper: emit a single "name\trequirement\tdesc" line
		// describing the next positional expected at the argv path in
		// args[1:]. Shell uses it to render a non-clickable hint when
		// the cursor is at a positional slot.
		return runInternalCompleteHint(args[1:])
	case "_complete-pipeline-flags":
		// Hidden helper: emit "--name\tGroup\tdesc" per typed flag
		// declared by the named pipeline's Args type, read from the
		// describe cache populated during compileAndExec. Used when
		// the user types `wing <pipeline> --<TAB>`.
		return runInternalCompletePipelineFlags(args[1:])
	case "help", "-h", "--help":
		PrintHelp(cmdSparkwing, os.Stdout)
		return nil
	default:
		PrintHelp(cmdSparkwing, os.Stderr)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// runRunsApprovals dispatches `sparkwing runs approvals <verb>`.
// Approvals nest under runs because every approval gates a
// specific run/node, and inspecting / resolving them belongs in
// the run-lifecycle surface.
func runRunsApprovals(ctx context.Context, paths orchestrator.Paths, args []string) error {
	if handleParentHelp(cmdApprovals, args) {
		return nil
	}
	if len(args) == 0 {
		PrintHelp(cmdApprovals, os.Stdout)
		return nil
	}
	switch args[0] {
	case "list":
		return runApprovals(args[1:])
	case "approve":
		return runApprove(ctx, paths, args[1:])
	case "deny":
		return runDeny(ctx, paths, args[1:])
	default:
		PrintHelp(cmdApprovals, os.Stderr)
		return fmt.Errorf("runs approvals: unknown subcommand %q", args[0])
	}
}

// runCluster dispatches `sparkwing cluster <verb>`. Every verb lives
// as its own runXxx in the existing codebase; cluster is just a
// grouping namespace. `sparkwing cluster` with no verb prints help.
func runCluster(args []string) error {
	if handleParentHelp(cmdCluster, args) {
		return nil
	}
	if len(args) == 0 {
		PrintHelp(cmdCluster, os.Stdout)
		return nil
	}
	switch args[0] {
	case "status":
		return runHealth(args[1:])
	case "agents":
		return runAgents(args[1:])
	case "worker":
		return runWorker(args[1:])
	case "gc":
		return runGC(args[1:])
	case "push":
		return runPush(args[1:])
	case "users":
		return runUsers(args[1:])
	case "tokens":
		return runTokens(args[1:])
	case "image":
		return runImage(args[1:])
	case "webhooks":
		return runWebhooks(args[1:])
	default:
		PrintHelp(cmdCluster, os.Stderr)
		return fmt.Errorf("cluster: unknown subcommand %q", args[0])
	}
}

// runConfigure dispatches `sparkwing configure <verb>`. Today only
// profiles; a second child (hooks or aliases) could land here if the
// laptop-config surface grows.
func runConfigure(args []string) error {
	if handleParentHelp(cmdConfigure, args) {
		return nil
	}
	if len(args) == 0 {
		PrintHelp(cmdConfigure, os.Stdout)
		return nil
	}
	switch args[0] {
	case "init":
		return runConfigureInit(args[1:])
	case "profiles":
		return runProfiles(args[1:])
	case "xrepo":
		return runXrepo(args[1:])
	default:
		PrintHelp(cmdConfigure, os.Stderr)
		return fmt.Errorf("configure: unknown subcommand %q", args[0])
	}
}

func runJobs(args []string) error {
	if handleParentHelp(cmdJobs, args) {
		return nil
	}
	if len(args) == 0 {
		PrintHelp(cmdJobs, os.Stderr)
		return errors.New("jobs: subcommand required")
	}
	paths, err := orchestrator.DefaultPaths()
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch args[0] {
	// Nested surfaces: approvals and triggers live under runs because
	// they're run-lifecycle concerns. Dispatches to the existing
	// approval/trigger functions unchanged.
	case "approvals":
		return runRunsApprovals(ctx, paths, args[1:])
	case "triggers":
		return runTriggers(args[1:])
	case "list":
		fs := flag.NewFlagSet(cmdJobsList.Path, flag.ContinueOnError)
		limit := fs.Int("limit", 20, "max runs to show")
		outFmt := fs.StringP("output", "o", "", "output format: table|json|plain (default: table)")
		asJSON := fs.Bool("json", false, "emit JSON (hidden alias for -o json)")
		_ = fs.MarkHidden("json")
		quiet := fs.BoolP("quiet", "q", false, "print only run ids, one per line")
		since := fs.Duration("since", 0, "only runs newer than this (e.g. 1h, 24h, 7d)")
		pipelines := multiFlagVar(fs, "pipeline", "filter by pipeline (repeatable, OR semantics)")
		statuses := multiFlagVar(fs, "status", "filter by status (repeatable, OR semantics)")
		tags := multiFlagVar(fs, "tag", "filter by pipelines.yaml tag (repeatable, OR semantics)")
		on := fs.String("on", "", "profile name (default: current default). Omit for local-only reads.")
		if err := parseAndCheck(cmdJobsList, fs, args[1:]); err != nil {
			if errors.Is(err, errHelpRequested) {
				return nil
			}
			return err
		}
		resolvedFmt, err := resolveOutputFormat(*outFmt, *asJSON, "jobs list")
		if err != nil {
			return err
		}

		pipelineSet := *pipelines
		if len(*tags) > 0 {
			extra, err := pipelinesWithTags(*tags)
			if err != nil {
				return err
			}
			pipelineSet = append(pipelineSet, extra...)
			if len(pipelineSet) == 0 {
				// Tag filter matched nothing; avoid "no filter -> all runs".
				if resolvedFmt == "json" {
					fmt.Fprintln(os.Stdout, "[]")
					return nil
				}
				fmt.Fprintln(os.Stdout, "no runs match the requested tags")
				return nil
			}
		}
		listOpts := orchestrator.ListOpts{
			Limit:     *limit,
			Pipelines: pipelineSet,
			Statuses:  *statuses,
			Since:     *since,
			JSON:      resolvedFmt == "json",
			Quiet:     *quiet,
		}
		if *on != "" {
			prof, err := resolveProfile(*on)
			if err != nil {
				return err
			}
			if err := requireController(prof, "jobs list"); err != nil {
				return err
			}
			return orchestrator.ListJobsRemote(ctx, prof.Controller, prof.Token, listOpts, os.Stdout)
		}
		return orchestrator.ListJobs(ctx, paths, listOpts, os.Stdout)

	case "status":
		fs := flag.NewFlagSet(cmdJobsStatus.Path, flag.ContinueOnError)
		runID := fs.String("run", "", "run identifier")
		outFmt := fs.StringP("output", "o", "", "output format: json|table|plain (default: table)")
		asJSON := fs.Bool("json", false, "emit JSON (hidden alias for -o json)")
		_ = fs.MarkHidden("json")
		follow := fs.BoolP("follow", "f", false, "poll until the run reaches a terminal state")
		on := fs.String("on", "", "profile name (default: current default). Omit for local-only reads.")
		exitZero := fs.Bool("exit-zero", false,
			"return exit code 0 even when the run failed/cancelled (opt out of the scriptable exit contract)")
		if err := parseAndCheck(cmdJobsStatus, fs, args[1:]); err != nil {
			if errors.Is(err, errHelpRequested) {
				return nil
			}
			return err
		}
		resolvedFmt, err := resolveOutputFormat(*outFmt, *asJSON, "jobs status")
		if err != nil {
			return err
		}
		statusOpts := orchestrator.StatusOpts{JSON: resolvedFmt == "json", Follow: *follow}
		if *on != "" {
			prof, err := resolveProfile(*on)
			if err != nil {
				return err
			}
			if err := requireController(prof, "jobs status"); err != nil {
				return err
			}
			if err := orchestrator.JobStatusRemote(ctx, prof.Controller, prof.Token,
				*runID, statusOpts, os.Stdout); err != nil {
				return err
			}
			if *exitZero {
				return nil
			}
			return remoteStatusExitCheck(ctx, prof.Controller, prof.Token, *runID)
		}
		if err := orchestrator.JobStatus(ctx, paths, *runID, statusOpts, os.Stdout); err != nil {
			return err
		}
		if *exitZero {
			return nil
		}
		return localStatusExitCheck(ctx, paths, *runID)

	case "logs":
		fs := flag.NewFlagSet(cmdJobsLogs.Path, flag.ContinueOnError)
		runID := fs.String("run", "", "run identifier")
		node := fs.String("node", "", "limit output to one node id")
		outFmt := fs.StringP("output", "o", "", "output format: table|json|plain (default: table on TTY, json when piped)")
		asJSON := fs.Bool("json", false, "emit JSON (alias for -o json)")
		pretty := fs.Bool("pretty", false, "force the human-readable colored renderer even when stdout isn't a terminal (alias for -o table)")
		follow := fs.BoolP("follow", "f", false, "tail the log(s) until the run terminates")
		on := fs.String("on", "",
			"profile name (cluster mode). Omit to read logs from the local SQLite store.")
		tail := fs.Int("tail", 0, "print only the last N lines (server-side in cluster mode)")
		head := fs.Int("head", 0, "print only the first N lines (server-side in cluster mode)")
		lines := fs.String("lines", "", "1-indexed inclusive line range A:B (server-side in cluster mode)")
		grep := fs.String("grep", "", "substring filter (server-side in cluster mode)")
		since := fs.Duration("since", 0,
			"only include output from nodes whose StartedAt >= now-D (e.g. 5m, 1h)")
		// --format is the legacy name; kept for compatibility with
		// callers that wired it pre-rewrite. `-o` is the canonical
		// surface going forward, matching kubectl / gh.
		format := fs.String("format", "", "DEPRECATED alias for -o/--output")
		_ = fs.MarkHidden("format")
		tree := fs.Bool("tree", false, "merge parent run + descendants into one chronological stream (local only)")
		if err := parseAndCheck(cmdJobsLogs, fs, args[1:]); err != nil {
			if errors.Is(err, errHelpRequested) {
				return nil
			}
			return err
		}
		if *tree && *on != "" {
			return errors.New("jobs logs: --tree is local-mode only (cannot combine with --on)")
		}
		// Merge --format into --output for backward compat. --format
		// accepts "pretty" (the old default) which maps to "table"
		// here; json/plain map through directly.
		effectiveOut := *outFmt
		if effectiveOut == "" && *format != "" {
			switch *format {
			case "pretty":
				effectiveOut = "table"
			case "json", "plain":
				effectiveOut = *format
			default:
				return fmt.Errorf("jobs logs: --format must be one of json|pretty|plain, got %q", *format)
			}
		}
		// --pretty forces the human-readable renderer. Explicit -o
		// still wins so the two can disagree loudly rather than
		// silently. `--pretty` + `-o json` reports the conflict.
		if *pretty {
			if effectiveOut != "" && effectiveOut != "table" {
				return fmt.Errorf("jobs logs: --pretty and -o %s disagree", effectiveOut)
			}
			effectiveOut = "table"
		}
		// Agent-first default: when the operator didn't pick a format
		// AND stdout isn't a terminal, emit raw JSONL so piped /
		// subprocess consumers (agents, CI, scripts) get the structured
		// form automatically. A human at a terminal still gets the
		// colored banners without typing --pretty.
		if effectiveOut == "" && !*asJSON && !term.IsTerminal(int(os.Stdout.Fd())) {
			effectiveOut = "json"
		}
		resolvedFmt, err := resolveOutputFormat(effectiveOut, *asJSON, "jobs logs")
		if err != nil {
			return err
		}
		if *tail > 0 && *head > 0 {
			return errors.New("jobs logs: --tail and --head cannot be combined")
		}
		opts := orchestrator.LogsOpts{
			Node:   *node,
			JSON:   resolvedFmt == "json",
			Follow: *follow,
			Format: resolvedFmt,
			Tail:   *tail,
			Head:   *head,
			Lines:  *lines,
			Grep:   *grep,
			Since:  *since,
			Tree:   *tree,
		}
		// --on selects cluster mode (remote logs via the profile's
		// controller+logs URLs). Omitting --on reads the local run
		// directory, which is what `wing`-on-laptop users want.
		if *on != "" {
			prof, err := resolveProfile(*on)
			if err != nil {
				return err
			}
			if prof.Controller == "" || prof.Logs == "" {
				return fmt.Errorf("jobs logs: profile %q must have both controller and logs URLs", prof.Name)
			}
			return orchestrator.JobLogsRemoteWithTokens(ctx, prof.Controller, prof.Logs, prof.Token,
				*runID, opts, os.Stdout)
		}
		return orchestrator.JobLogs(ctx, paths, *runID, opts, os.Stdout)

	case "errors":
		fs := flag.NewFlagSet(cmdJobsErrors.Path, flag.ContinueOnError)
		runID := fs.String("run", "", "run identifier")
		outFmt := fs.StringP("output", "o", "", "output format: table|json|plain")
		asJSON := fs.Bool("json", false, "emit JSON (hidden alias for -o json)")
		_ = fs.MarkHidden("json")
		on := fs.String("on", "", "profile name (default: current default). Omit for local-only reads.")
		if err := parseAndCheck(cmdJobsErrors, fs, args[1:]); err != nil {
			if errors.Is(err, errHelpRequested) {
				return nil
			}
			return err
		}
		resolvedFmt, err := resolveOutputFormat(*outFmt, *asJSON, "jobs errors")
		if err != nil {
			return err
		}
		emitJSON := resolvedFmt == "json"
		if *on != "" {
			prof, err := resolveProfile(*on)
			if err != nil {
				return err
			}
			if err := requireController(prof, "jobs errors"); err != nil {
				return err
			}
			return orchestrator.JobErrorsRemote(ctx, prof.Controller, prof.Token,
				*runID, emitJSON, os.Stdout)
		}
		return orchestrator.JobErrors(ctx, paths, *runID, emitJSON, os.Stdout)

	case "cancel":
		fs := flag.NewFlagSet(cmdJobsCancel.Path, flag.ContinueOnError)
		runID := fs.String("run", "", "run identifier to cancel")
		on := fs.String("on", "", "profile name (default: current default)")
		if err := parseAndCheck(cmdJobsCancel, fs, args[1:]); err != nil {
			if errors.Is(err, errHelpRequested) {
				return nil
			}
			return err
		}
		prof, err := resolveProfile(*on)
		if err != nil {
			return err
		}
		if err := requireController(prof, "jobs cancel"); err != nil {
			return err
		}
		c := client.NewWithToken(prof.Controller, nil, prof.Token)
		if err := c.CancelRun(ctx, *runID); err != nil {
			return fmt.Errorf("cancel %s: %w", *runID, err)
		}
		fmt.Fprintf(os.Stdout, "cancel requested for %s\n", *runID)
		return nil

	case "retry":
		fs := flag.NewFlagSet(cmdJobsRetry.Path, flag.ContinueOnError)
		srcRunIDFlag := fs.String("run", "", "run identifier to retry")
		on := fs.String("on", "", "profile name (default: current default)")
		if err := parseAndCheck(cmdJobsRetry, fs, args[1:]); err != nil {
			if errors.Is(err, errHelpRequested) {
				return nil
			}
			return err
		}
		prof, err := resolveProfile(*on)
		if err != nil {
			return err
		}
		if err := requireController(prof, "jobs retry"); err != nil {
			return err
		}
		srcRunID := *srcRunIDFlag
		c := client.NewWithToken(prof.Controller, nil, prof.Token)
		run, err := c.GetRun(ctx, srcRunID)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", srcRunID, err)
		}
		resp, err := c.CreateTrigger(ctx, client.TriggerRequest{
			Pipeline: run.Pipeline,
			Args:     run.Args,
			Trigger: client.TriggerMeta{
				// Preserve an audit trail to the origin run so the
				// dashboard can surface "retry of run-X" if it wants.
				Source: "retry:" + srcRunID,
			},
			Git:     client.GitMeta{Branch: run.GitBranch, SHA: run.GitSHA},
			RetryOf: srcRunID,
		})
		if err != nil {
			return fmt.Errorf("trigger retry: %w", err)
		}
		fmt.Fprintf(os.Stdout, "retried %s as %s\n", srcRunID, resp.RunID)
		return nil

	case "prune":
		fs := flag.NewFlagSet(cmdJobsPrune.Path, flag.ContinueOnError)
		on := fs.String("on", "", "profile name (default: current default)")
		olderThan := fs.Duration("older-than", 0, "prune runs older than this (e.g. 7d, 48h). Required unless --run is set.")
		dryRun := fs.Bool("dry-run", false, "list matching runs without deleting")
		oneRun := fs.String("run", "", "prune this specific run id")
		if err := parseAndCheck(cmdJobsPrune, fs, args[1:]); err != nil {
			if errors.Is(err, errHelpRequested) {
				return nil
			}
			return err
		}
		prof, err := resolveProfile(*on)
		if err != nil {
			return err
		}
		if err := requireController(prof, "jobs prune"); err != nil {
			return err
		}
		// parseAndCheck handles the --run / --older-than conflict, but
		// doesn't know that at least ONE of them is required -- that's a
		// per-call rule. Enforce it explicitly.
		if *oneRun == "" && *olderThan <= 0 {
			return errors.New("jobs prune: either --older-than DUR or --run RUN_ID is required")
		}
		c := client.NewWithToken(prof.Controller, nil, prof.Token)
		var logc *logs.Client
		if prof.Logs != "" {
			logc = logs.NewClientWithToken(prof.Logs, nil, prof.Token)
		}
		var victims []string
		if *oneRun != "" {
			victims = []string{*oneRun}
		} else {
			// Use a large page; prune is a maintenance op, not a hot path.
			runs, err := c.ListRuns(ctx, store.RunFilter{Limit: 10000})
			if err != nil {
				return fmt.Errorf("list runs: %w", err)
			}
			cutoff := time.Now().Add(-*olderThan)
			for _, r := range runs {
				if !r.StartedAt.Before(cutoff) {
					continue
				}
				// Never prune in-flight work; the worker owns that row.
				if r.Status != "success" && r.Status != "failed" && r.Status != "cancelled" {
					continue
				}
				victims = append(victims, r.ID)
			}
		}
		if len(victims) == 0 {
			fmt.Fprintln(os.Stdout, "no runs match prune criteria")
			return nil
		}
		if *dryRun {
			fmt.Fprintf(os.Stdout, "would prune %d run(s):\n", len(victims))
			for _, id := range victims {
				fmt.Fprintln(os.Stdout, "  "+id)
			}
			return nil
		}
		for _, id := range victims {
			if err := c.DeleteRun(ctx, id); err != nil {
				return fmt.Errorf("delete run %s: %w", id, err)
			}
			if logc != nil {
				if err := logc.DeleteRun(ctx, id); err != nil {
					// Don't abort the whole prune on a single logs
					// deletion -- the controller row is already gone.
					// Surface the problem on stderr so it's visible.
					fmt.Fprintf(os.Stderr, "warn: logs delete %s: %v\n", id, err)
				}
			}
		}
		fmt.Fprintf(os.Stdout, "pruned %d run(s)\n", len(victims))
		return nil

	case "failures":
		return runJobsFailures(ctx, paths, args[1:])
	case "stats":
		return runJobsStats(ctx, paths, args[1:])
	case "last":
		return runJobsLast(ctx, paths, args[1:])
	case "tree":
		return runJobsTree(ctx, paths, args[1:])
	case "get":
		return runJobsGet(ctx, paths, args[1:])
	case "wait":
		return runJobsWait(ctx, paths, args[1:])
	case "find":
		return runJobsFind(ctx, paths, args[1:])
	default:
		return fmt.Errorf("jobs: unknown command %q", args[0])
	}
}

// resolveOutputFormat canonicalizes the -o/--output + --json pair into
// one of {"table","json","plain"}. --json is a hidden alias for -o
// json; setting both with disagreeing values errors so operators see
// the contradiction rather than one silently winning.
func resolveOutputFormat(outFmt string, jsonAlias bool, cmdPath string) (string, error) {
	switch outFmt {
	case "", "table", "json", "plain":
	default:
		return "", fmt.Errorf("%s: -o/--output must be one of table|json|plain, got %q", cmdPath, outFmt)
	}
	if jsonAlias {
		if outFmt != "" && outFmt != "json" {
			return "", fmt.Errorf("%s: --json and -o %s disagree", cmdPath, outFmt)
		}
		return "json", nil
	}
	if outFmt == "" {
		return "table", nil
	}
	return outFmt, nil
}

// isTerminalRunStatus mirrors orchestrator.isTerminalStatus; kept local
// to avoid leaking an exported helper out of the orchestrator package
// solely for CLI exit-code logic.
func isTerminalRunStatus(s string) bool {
	return s == "success" || s == "failed" || s == "cancelled"
}

// statusExitCode maps a run's status to the scripted exit contract:
//
//	success   -> 0
//	running   -> 1 (still running at the time jobs status returned;
//	              we treat "not yet done" as non-success for --follow
//	              callers that explicitly opted in to the single-shot
//	              read. `jobs wait` is the right tool for blocking.)
//	failed    -> 1
//	cancelled -> 1
//
// Returns nil when the status is success so callers can `return` the
// value directly.
func statusExitCode(status string) error {
	if status == "success" {
		return nil
	}
	return exitErrorf(1, "run status: %s", status)
}

// localStatusExitCheck reads the run's terminal status from the local
// SQLite store and maps it to an exit code per the jobs-status
// contract. Called after the non-follow render so the operator sees
// the rendered table AND the exit code scripts can rely on.
func localStatusExitCheck(ctx context.Context, paths orchestrator.Paths, runID string) error {
	st, err := store.Open(paths.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	return statusExitCode(run.Status)
}

// remoteStatusExitCheck is the remote counterpart to
// localStatusExitCheck. Hits GetRun on the controller rather than the
// local store.
func remoteStatusExitCheck(ctx context.Context, controllerURL, token, runID string) error {
	c := client.NewWithToken(controllerURL, nil, token)
	run, err := c.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	return statusExitCode(run.Status)
}

// multiFlagVar registers a repeatable string flag on fs and returns a
// pointer to the accumulated slice. Kept as a helper so call sites
// stay terse; pflag natively supports the repeat-flag idiom via
// StringSliceVar but needs a pre-declared destination.
func multiFlagVar(fs *flag.FlagSet, name, usage string) *[]string {
	var dest []string
	fs.StringSliceVar(&dest, name, nil, usage)
	return &dest
}

// pipelinesWithTags resolves tag names to pipeline names via the
// enclosing .sparkwing/pipelines.yaml. Returns an empty slice when the
// yaml isn't found or no pipelines carry the requested tags.
func pipelinesWithTags(tags []string) ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	_, cfg, err := pipelines.Discover(cwd)
	if err != nil {
		// Missing pipelines.yaml isn't a hard error for filtering;
		// it just means no tag→pipeline resolution is possible.
		return nil, nil
	}
	want := map[string]struct{}{}
	for _, t := range tags {
		want[t] = struct{}{}
	}
	var matched []string
	for _, p := range cfg.Pipelines {
		for _, t := range p.Tags {
			if _, ok := want[t]; ok {
				matched = append(matched, p.Name)
				break
			}
		}
	}
	return matched, nil
}

// findSparkwingDir walks up from cwd looking for a .sparkwing/
// directory that contains a main.go. Returns an error if none is
// found before the filesystem root.
func findSparkwingDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return findSparkwingDirFrom(dir)
}

// findSparkwingDirFrom is findSparkwingDir starting at an explicit
// path. Used by `wing -C <path>` to anchor discovery somewhere other
// than cwd.
func findSparkwingDirFrom(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", start, err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", start)
	}
	dir := abs
	for {
		candidate := filepath.Join(dir, ".sparkwing")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			if _, err := os.Stat(filepath.Join(candidate, "main.go")); err == nil {
				return candidate, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .sparkwing/main.go found from %s up", abs)
		}
		dir = parent
	}
}

func mustGetwd() string {
	d, _ := os.Getwd()
	return d
}
