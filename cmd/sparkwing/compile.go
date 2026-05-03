package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sparkwing-dev/sparkwing-sdk/bincache"
	"github.com/sparkwing-dev/sparkwing-cli/internal/sparks"
	"github.com/sparkwing-dev/sparkwing-cli/pkg/color"
)

// compileAndExec compiles the .sparkwing/ Go module to a cache
// directory keyed on a fingerprint of the module plus every local
// `replace` target, then execs the cached binary with the given
// args. Subsequent invocations with no source changes skip the
// compile entirely.
//
// Falls back to `go run .` if the fingerprint can't be computed
// (e.g. a replace target was deleted); the result is correct, just
// slow.
//
// All the heavy lifting -- hashing, fetch-from-cache, build, exec-
// replace -- lives in internal/bincache so cmd/sparkwing-fleet-worker
// can share it. This file is pure orchestration of that library
// against the `wing` dispatch path.
func compileAndExec(sparkwingDir string, args []string, env []string, opts compileOptions) error {
	// Resolve sparks libraries before we compute the cache key so any
	// overlay modfile change busts the hash (PipelineCacheKey already
	// hashes .resolved.mod/.sum; see REG-011e). Fast path: absent
	// sparks.yaml is a single os.ReadFile that returns ErrNotExist --
	// negligible latency for the common case.
	if err := resolveSparks(context.Background(), sparkwingDir, opts); err != nil {
		return err
	}

	if os.Getenv("SPARKWING_NO_CACHE") != "" {
		return runGo(sparkwingDir, append([]string{"run", "."}, args...), env)
	}

	key, err := bincache.PipelineCacheKey(sparkwingDir)
	if err != nil {
		// Treat hashing failures as a cache miss without caching.
		return runGo(sparkwingDir, append([]string{"run", "."}, args...), env)
	}

	binPath := bincache.CachedBinaryPath(key)

	// 1) Local disk cache. If present, skip compile and remote
	// roundtrip entirely -- the tight laptop dev loop.
	if _, err := os.Stat(binPath); err == nil {
		ensureDescribeCache(sparkwingDir, binPath)
		return bincache.ExecReplace(binPath, args, sparkwingDir, env)
	}

	// 2) Remote binary cache (sparkwing-cache /bin/<hash>). When
	// SPARKWING_GITCACHE_URL is set, try to download a pre-built
	// binary before falling back to `go build`. Every runner in the
	// fleet shares the same cache, so a new commit's binary compiles
	// exactly once across the cluster.
	if gcURL := bincache.CacheURL(); gcURL != "" {
		if err := bincache.TryBinary(gcURL, key, binPath); err == nil {
			ensureDescribeCache(sparkwingDir, binPath)
			return bincache.ExecReplace(binPath, args, sparkwingDir, env)
		}
	}

	// 3) Compile locally. Announce first so the user understands why
	// this run is taking longer than the steady-state ~instant exec.
	announceCompile(binPath)
	if err := bincache.CompilePipeline(sparkwingDir, binPath); err != nil {
		return err
	}

	// 4) Upload so the next runner that wants this binary gets a
	// cache hit. Failures here are non-fatal.
	if gcURL := bincache.CacheURL(); gcURL != "" {
		if err := bincache.UploadBinary(gcURL, bincache.CacheToken(), key, binPath); err != nil {
			slog.Default().Warn("bin cache upload failed", "err", err, "hash", key)
		}
	}

	// Warm the describe cache before exec so `wing <pipeline> --<TAB>`
	// shows typed flags without waiting for a second run.
	ensureDescribeCache(sparkwingDir, binPath)
	return bincache.ExecReplace(binPath, args, sparkwingDir, env)
}

// ensureDescribeCache writes the describe-cache file if it's missing
// for the current PipelineCacheKey. Failures are logged at debug-
// level and swallowed -- the cache is a perf optimization, not a
// correctness gate on the pipeline run.
func ensureDescribeCache(sparkwingDir, binPath string) {
	key, err := bincache.PipelineCacheKey(sparkwingDir)
	if err != nil {
		return
	}
	if _, err := os.Stat(describeCachePath(key)); err == nil {
		return
	}
	if err := writeDescribeCache(sparkwingDir, binPath); err != nil {
		slog.Default().Debug("describe cache write failed", "err", err, "hash", key)
	}
}

// announceCompile prints a one-line stderr message before a local
// compile so the user knows why this run is slower than steady-state.
// Distinguishes "first time ever" (no other cached pipeline binaries
// on this laptop) from "source changed since last run" (cache root
// has entries, just not for this hash). Stays silent when stderr
// isn't a TTY (agents and pipes get clean logs already).
//
// binPath looks like ~/.sparkwing/cache/pipelines/<hash>/pipelines;
// its grandparent is the cache root we inspect for prior entries.
// We don't try to track "first compile for this specific repo"
// because that would mean persisting per-dir bookkeeping; the
// repo-vs-laptop signal is close enough in practice.
func announceCompile(binPath string) {
	cacheRoot := filepath.Dir(filepath.Dir(binPath))
	firstEver := true
	if entries, err := os.ReadDir(cacheRoot); err == nil && len(entries) > 0 {
		firstEver = false
	}
	var msg string
	if firstEver {
		msg = "first run on this machine: compiling .sparkwing/ pipeline binary (one-time, ~5-10s)..."
	} else {
		msg = "pipeline source changed since last run: recompiling .sparkwing/ binary..."
	}
	fmt.Fprintln(os.Stderr, color.Dim(msg))
}

// runExec runs a binary with the given args/env and propagates its
// exit code to the current process on non-zero termination. Used by
// runGo for the `go run .` fallback.
func runExec(bin string, args []string, dir string, env []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// runGo shells out to the `go` toolchain.
func runGo(dir string, args, env []string) error {
	return runExec("go", args, dir, env)
}

// compileOptions bundles the subset of wing flags that affects how we
// prepare the module graph before compile. Today only `--no-update`
// (gate on sparks auto-resolve); extend here rather than threading
// booleans one at a time through compileAndExec.
type compileOptions struct {
	// NoUpdate skips the sparks auto-resolve step. Set when the
	// operator passed --no-update or when SPARKWING_NO_SPARKS_RESOLVE=1
	// is exported. Absent sparks.yaml is already a no-op regardless of
	// this flag.
	NoUpdate bool
}

// resolveSparks invokes sparks.ResolveAndWrite unless the operator
// opted out. When the sparks manifest is absent ResolveAndWrite is a
// single stat call, so the fast-path cost is negligible. Errors bubble
// up as compile failures by default -- an agent wanting `latest`
// should fail loudly rather than silently pin to stale `go.mod`
// versions. `--no-update` (or SPARKWING_NO_SPARKS_RESOLVE=1) flips to
// the "warn and fall back" path for offline work.
func resolveSparks(ctx context.Context, sparkwingDir string, opts compileOptions) error {
	noUpdate := opts.NoUpdate || os.Getenv("SPARKWING_NO_SPARKS_RESOLVE") != ""
	if noUpdate {
		// Offline / CI path: the user explicitly asked us not to hit
		// the network. Skip entirely; any pre-existing overlay on disk
		// is still honored by the compile step.
		return nil
	}
	if _, err := sparks.ResolveAndWrite(ctx, sparkwingDir); err != nil {
		return fmt.Errorf("sparks resolve: %w (use --no-update to compile against existing go.mod pins)", err)
	}
	return nil
}
