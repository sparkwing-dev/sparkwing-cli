// Wing-owned flags. `wing <pipeline>` forwards most of its args to
// the user's compiled .sparkwing/ pipeline binary, but a handful of
// flags belong to wing itself (--from, --on, --config) because they
// control the execution environment rather than the pipeline's
// inputs. parseWingFlags pulls those out of the arg stream so the
// pipeline binary never sees them.
//
// Manual walking (vs pflag) because the pipeline binary may define
// flags of its own -- we can't register a FlagSet that errors on
// "unknown flags" without blowing up every pipeline with typed args.
// We only strip what we recognize, pass the rest through untouched.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/sparkwing-dev/sparkwing-sdk/controller/client"
)

// wingFlags is the subset of `wing <pipeline> ...args` that wing
// consumes. Everything else goes to the pipeline binary as-is.
type wingFlags struct {
	// from is the git ref (branch, tag, sha) to check out before
	// compiling the pipeline. Empty = use the current working tree.
	from string
	// on is the profile name to dispatch remotely against. Empty =
	// run locally. Remote dispatch is a separate code path.
	on string
	// config names a preset from config.yaml whose fields are layered
	// onto the explicit flags BEFORE dispatch. Explicit flags win, so
	// `wing X --config dev --on stage` hits stage, not dev's on.
	config string
	// retryOf, when non-empty, runs this invocation as a retry of the
	// named run id. Passed orchestrator-side as Options.RetryOf so
	// skip-passed rehydration can load prior outputs and short-circuit
	// nodes that already succeeded.
	retryOf string
	// fullRetry pairs with retryOf: when set, disables skip-passed so
	// every node re-runs from scratch. Ignored when retryOf is empty.
	fullRetry bool
	// noUpdate skips the sparks auto-resolve step before compile so
	// offline work doesn't try to hit the module proxy. Env var
	// SPARKWING_NO_SPARKS_RESOLVE=1 has the same effect for CI.
	noUpdate bool
	// verbose elevates orchestrator log output to debug level by
	// setting SPARKWING_LOG_LEVEL=debug on the child exec env. Named
	// "verbose" rather than "debug" because --debug is ambiguous
	// with the `sparkwing debug run` subcommand.
	verbose bool
	// secrets, when set, sources secrets from the named profile's
	// controller instead of the laptop dotenv. Orthogonal to --on:
	// `wing X --secrets prod` runs locally but resolves
	// sparkwing.Secret(...) calls against prod's controller. Empty
	// = laptop dotenv (REG-017 default).
	secrets string
	// changeDir is the -C / --change-directory flag, mirroring
	// `git -C <path>` and `make -C <path>`. When set, wing resolves
	// .sparkwing/ relative to this path instead of cwd, and the child
	// pipeline binary's cwd is set to the resolved repo root so the
	// SDK's walk-up still finds the correct project. Useful for
	// running `wing -C ../app2 deploy` from anywhere on the laptop.
	changeDir string
}

// collectPipelineArgs parses the passthrough into a string-keyed
// map for TriggerRequest.Args. Treats every `--<name> VALUE` and
// `--<name>=VALUE` as an entry keyed by the dash-case flag name;
// bare `--<name>` with no following value (or followed by another
// `--flag`) is recorded as "true" to mirror how the SDK's typed-flag
// parser handles bool flags.
//
// We do NOT validate against the pipeline's schema here -- the
// controller re-parses using the remote pipeline's own schema. If
// the operator passes a flag the remote doesn't know, the trigger
// will 4xx cleanly with a schema-aware message.
func collectPipelineArgs(passthrough []string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(passthrough) {
		tok := passthrough[i]
		if !strings.HasPrefix(tok, "--") {
			i++
			continue
		}
		name := strings.TrimPrefix(tok, "--")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			key := name[:eq]
			if key != "" {
				out[key] = name[eq+1:]
			}
			i++
			continue
		}
		// Look ahead for a value. A following token that also starts
		// with -- belongs to the next flag, so the current flag is
		// treated as a bool.
		if i+1 < len(passthrough) && !strings.HasPrefix(passthrough[i+1], "--") {
			out[name] = passthrough[i+1]
			i += 2
			continue
		}
		out[name] = "true"
		i++
	}
	return out
}

// parseWingFlags walks args and splits out the wing-owned flags from
// the pipeline's pass-through args. Supports both `--flag value` and
// `--flag=value` forms. Unknown flags are left in pass so the
// pipeline binary can parse them.
//
// Malformed trailing flags (`--from` with no value) are left in pass
// so the pipeline's parser gets a chance to complain with a better
// error than we could produce here.
func parseWingFlags(args []string) (wingFlags, []string) {
	var wf wingFlags
	pass := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--from":
			if i+1 < len(args) {
				wf.from = args[i+1]
				i += 2
				continue
			}
			pass = append(pass, a)
			i++
		case strings.HasPrefix(a, "--from="):
			wf.from = strings.TrimPrefix(a, "--from=")
			i++
		case a == "--on":
			if i+1 < len(args) {
				wf.on = args[i+1]
				i += 2
				continue
			}
			pass = append(pass, a)
			i++
		case strings.HasPrefix(a, "--on="):
			wf.on = strings.TrimPrefix(a, "--on=")
			i++
		case a == "--retry-of":
			if i+1 < len(args) {
				wf.retryOf = args[i+1]
				i += 2
				continue
			}
			pass = append(pass, a)
			i++
		case strings.HasPrefix(a, "--retry-of="):
			wf.retryOf = strings.TrimPrefix(a, "--retry-of=")
			i++
		case a == "--full":
			wf.fullRetry = true
			i++
		case a == "--config":
			if i+1 < len(args) {
				wf.config = args[i+1]
				i += 2
				continue
			}
			pass = append(pass, a)
			i++
		case strings.HasPrefix(a, "--config="):
			wf.config = strings.TrimPrefix(a, "--config=")
			i++
		case a == "--no-update":
			wf.noUpdate = true
			i++
		case a == "--verbose", a == "-v":
			wf.verbose = true
			i++
		case a == "--secrets":
			if i+1 < len(args) {
				wf.secrets = args[i+1]
				i += 2
				continue
			}
			pass = append(pass, a)
			i++
		case strings.HasPrefix(a, "--secrets="):
			wf.secrets = strings.TrimPrefix(a, "--secrets=")
			i++
		case a == "-C", a == "--change-directory":
			if i+1 < len(args) {
				wf.changeDir = args[i+1]
				i += 2
				continue
			}
			pass = append(pass, a)
			i++
		case strings.HasPrefix(a, "--change-directory="):
			wf.changeDir = strings.TrimPrefix(a, "--change-directory=")
			i++
		default:
			pass = append(pass, a)
			i++
		}
	}
	return wf, pass
}

// setupFromRef creates a git worktree rooted at ref, returning the
// worktree's on-disk path and a cleanup function the caller must
// defer. Uses `git worktree add` against the enclosing repo rather
// than cloning from a URL -- so any ref your local clone can resolve
// (origin/foo, a SHA, a tag, a stash) works with no network.
//
// Tries a best-effort `git fetch origin <ref>` first for refs the
// local clone hasn't seen yet; fetch failures are non-fatal because
// operators may be offline or targeting local branches.
func setupFromRef(sparkwingDir, ref string) (worktreeDir string, sparkwingSub string, cleanup func(), err error) {
	// The sparkwing dir is <repo>/.sparkwing; parent is the repo root.
	repoRoot := filepath.Dir(sparkwingDir)

	tmpDir, err := os.MkdirTemp("", "sparkwing-from-*")
	if err != nil {
		return "", "", nil, fmt.Errorf("mkdir tmp: %w", err)
	}

	// Best-effort fetch so refs like 'main' or 'origin/feature-x' that
	// haven't been pulled recently still resolve. Any failure (network
	// out, missing origin) is swallowed -- worktree add will fail with
	// a clearer message if the ref truly doesn't exist.
	_ = exec.Command("git", "-C", repoRoot, "fetch", "--quiet", "origin", ref).Run()

	out, err := exec.Command("git", "-C", repoRoot,
		"worktree", "add", "--detach", "--quiet", tmpDir, ref).CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", "", nil, fmt.Errorf("git worktree add %s: %w: %s",
			ref, err, strings.TrimSpace(string(out)))
	}

	sub := filepath.Join(tmpDir, ".sparkwing")
	if fi, statErr := os.Stat(sub); statErr != nil || !fi.IsDir() {
		// Cleanup before returning so the caller doesn't have to.
		_ = exec.Command("git", "-C", repoRoot,
			"worktree", "remove", "--force", tmpDir).Run()
		_ = os.RemoveAll(tmpDir)
		return "", "", nil, fmt.Errorf("ref %s has no .sparkwing/ directory", ref)
	}

	cleanup = func() {
		// worktree remove is the correct tear-down -- a bare RemoveAll
		// leaves a dangling entry in .git/worktrees. --force so we
		// don't balk on the scratch state we wrote during the run.
		_ = exec.Command("git", "-C", repoRoot,
			"worktree", "remove", "--force", tmpDir).Run()
		_ = os.RemoveAll(tmpDir)
	}
	return tmpDir, sub, cleanup, nil
}

// dispatchRemote is the `wing <pipeline> --on <profile>` code path.
// Builds a TriggerRequest from wingFlags (env, from=branch) + the
// --arg KEY=VALUE entries in passthrough, POSTs it to the profile's
// controller, and prints the resulting run id + a hint for tailing
// logs. Does NOT compile the local pipeline -- it assumes the remote
// side already has the pipeline registered.
//
// If the operator wants to dispatch code that isn't on the remote
// yet, the companion workflow is: `sparkwing push --on <profile>`
// first (which returns a timestamped ref), then
// `wing <pipeline> --on <profile> --from <that-ref>` to trigger
// against that ref.
func dispatchRemote(pipelineName string, wf wingFlags, passthrough []string) error {
	prof, err := resolveProfile(wf.on)
	if err != nil {
		return err
	}
	if err := requireController(prof, "wing --on"); err != nil {
		return err
	}

	args := collectPipelineArgs(passthrough)
	source := "wing"
	if host, err := os.Hostname(); err == nil && host != "" {
		source = "wing@" + host
	}
	var userName string
	if u, err := user.Current(); err == nil {
		userName = u.Username
	}

	// Detect the calling repo so the warm-runner can clone + compile
	// the right source tree. Without this, the trigger arrives with an
	// empty GITHUB_REPOSITORY and the runner falls back to the baked
	// binary in the sparkwing-runner image -- which only knows about
	// sparkwing's own pipelines, so every consumer-repo dispatch fails
	// with "pipeline is not registered".
	//
	// The fields are plumbed through both the env map (what the
	// warm-runner actually reads for the clone+compile path) and the
	// Git meta block (what the dashboard reads to render the repo
	// pill on the run detail page).
	branch, sha, repoSlug, repoURL := detectRemoteGit()
	envMap := map[string]string{}
	if repoSlug != "" {
		envMap["GITHUB_REPOSITORY"] = repoSlug
	}

	triggerBranch := wf.from
	if triggerBranch == "" {
		triggerBranch = branch
	}

	owner, name := "", ""
	if slash := strings.IndexByte(repoSlug, '/'); slash > 0 {
		owner, name = repoSlug[:slash], repoSlug[slash+1:]
	}

	req := client.TriggerRequest{
		Pipeline: pipelineName,
		Args:     args,
		Trigger: client.TriggerMeta{
			Source: source,
			User:   userName,
			Env:    envMap,
		},
		Git: client.GitMeta{
			Branch:      triggerBranch,
			SHA:         sha,
			Repo:        name,
			RepoURL:     repoURL,
			GithubOwner: owner,
			GithubRepo:  name,
		},
		RetryOf: wf.retryOf,
	}
	// --full is intentionally local-only: remote dispatch always
	// does skip-passed retries. A user who wants a full re-run on
	// the remote side should dispatch fresh (without --retry-of)
	// rather than pass the flag. Warn if they combined them so the
	// silent no-op isn't mysterious.
	if wf.fullRetry && wf.retryOf != "" {
		fmt.Fprintln(os.Stderr, "wing: --full is local-only; remote retry always skips passed nodes (ignoring --full)")
	}

	c := client.NewWithToken(prof.Controller, nil, prof.Token)
	resp, err := c.CreateTrigger(context.Background(), req)
	if err != nil {
		return fmt.Errorf("create trigger on %s: %w", prof.Name, err)
	}

	fmt.Fprintf(os.Stdout, "dispatched %s on %s as %s (status=%s)\n",
		pipelineName, prof.Name, resp.RunID, resp.Status)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "tail logs:\n")
	fmt.Fprintf(os.Stderr, "  sparkwing runs logs --run %s --on %s --follow\n",
		resp.RunID, prof.Name)
	fmt.Fprintf(os.Stderr, "check status:\n")
	fmt.Fprintf(os.Stderr, "  sparkwing runs status --run %s --on %s\n",
		resp.RunID, prof.Name)
	return nil
}

// detectRemoteGit reads the calling CWD's git state: current branch,
// HEAD SHA, the `owner/repo` slug, and the raw origin URL. Any piece
// that can't be resolved returns empty. Used by dispatchRemote to
// stamp GITHUB_REPOSITORY on the trigger env so warm-runners can
// clone+compile the right source tree, and to populate git meta on
// the trigger so the dashboard shows the right repo pill.
func detectRemoteGit() (branch, sha, repo, repoURL string) {
	if out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
		if branch == "HEAD" {
			branch = ""
		}
	}
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		sha = strings.TrimSpace(string(out))
	}
	if out, err := exec.Command("git", "remote", "get-url", "origin").Output(); err == nil {
		repoURL = strings.TrimSpace(string(out))
		repo = parseGithubOwnerRepo(repoURL)
	}
	return branch, sha, repo, repoURL
}

// parseGithubOwnerRepo extracts "owner/name" from ssh or https github
// origin URLs. Returns empty for non-github hosts so the warm-runner
// doesn't try to clone from a URL it can't handle.
func parseGithubOwnerRepo(url string) string {
	// git@github.com:owner/name.git
	if strings.HasPrefix(url, "git@github.com:") {
		rest := strings.TrimPrefix(url, "git@github.com:")
		rest = strings.TrimSuffix(rest, ".git")
		return rest
	}
	// https://github.com/owner/name(.git)
	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		if strings.HasPrefix(url, prefix) {
			rest := strings.TrimPrefix(url, prefix)
			rest = strings.TrimSuffix(rest, ".git")
			return rest
		}
	}
	return ""
}
