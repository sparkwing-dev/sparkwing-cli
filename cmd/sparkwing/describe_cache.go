// Describe cache: the per-repo record of each pipeline's typed
// flags (--version, --region, --dry-run, ...). Populated after each
// successful compileAndExec so the cache is always in sync with
// the binary currently on disk. Consumed by:
//
//   - `wing <pipeline> --<TAB>` completion (via
//     `sparkwing _complete-pipeline-flags <pipeline>`)
//   - future per-pipeline help rendering
//
// Cache layout:
//
//	$SPARKWING_HOME/cache/describe/<pipeline-cache-key>.json
//	$SPARKWING_HOME/cache/describe/by-repo/<sha256(abs-sparkwing-dir)>.json
//
// The content-keyed file is keyed by `bincache.PipelineCacheKey`, so
// exact hits require the SDK + .sparkwing/ source to be byte-identical
// to the last successful `wing` run. That's ideal for correctness but
// fragile during active editing -- a single saved file in the SDK
// shifts the key, leaving tab completion with nothing to offer.
//
// The by-repo file is the "last known schema for this repo" fallback,
// written alongside the content-keyed cache on every compile. Tab
// completion reads it when the content-keyed cache misses AND no
// cached binary exists at the current key (so we can't cheaply
// regenerate). Stale flag names on tab are a much better UX than an
// empty menu or a multi-second compile block.
//
// Read order in readDescribeCache:
//  1. Content-keyed cache (fast, exact).
//  2. Cached pipeline binary at the current key -> `--describe` and
//     populate both caches in place (~50ms; keeps tab responsive).
//  3. Per-repo "last known" cache (stale but present).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sparkwing-dev/sparkwing-sdk/bincache"
	"github.com/sparkwing-dev/sparkwing-sdk/sparkwing"
)

// describeCachePath returns the cache file for the given key.
// Callers typically pass bincache.PipelineCacheKey(sparkwingDir).
func describeCachePath(key string) string {
	return filepath.Join(bincache.SparkwingHome(),
		"cache", "describe", key+".json")
}

// byRepoDescribePath returns a per-repo fallback cache file, keyed
// by sha256 of the absolute sparkwing-dir path. This is the "last
// known schema for this repo" file, written alongside the content-
// keyed cache on every writeDescribeCache call. Completion reads it
// when the content-keyed cache misses (source edited since the last
// `wing` run) so tab still produces flag names instead of silently
// nothing -- stale flag descriptions are a much better UX than an
// empty menu or a multi-second recompile block.
func byRepoDescribePath(sparkwingDir string) string {
	abs, err := filepath.Abs(sparkwingDir)
	if err != nil {
		abs = sparkwingDir
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(bincache.SparkwingHome(),
		"cache", "describe", "by-repo", hex.EncodeToString(sum[:16])+".json")
}

// readDescribeCache loads the cached schema for sparkwingDir.
//
// Lookup order:
//  1. Content-keyed cache (bincache.PipelineCacheKey). Exact-hit when
//     source is unchanged since the last `wing` run -- instant and
//     always accurate.
//  2. Cached pipeline binary at the current content key. If present,
//     spawn `--describe` (~50ms) to refresh the cache in place. This
//     closes the gap between "ran wing once, then tweaked docs on an
//     arg" and "tab completion suddenly empty".
//  3. Per-repo "last known" fallback file. Stale flag names are
//     acceptable; blocking the shell on a compile is not.
//
// Returns (nil, nil) on cache miss so callers can treat missing-cache
// as "no typed flags known yet" rather than a hard error.
func readDescribeCache(sparkwingDir string) ([]sparkwing.DescribePipeline, error) {
	key, err := bincache.PipelineCacheKey(sparkwingDir)
	if err != nil {
		// Hash failure still has a fighting chance via the by-repo
		// fallback.
		return readDescribeFile(byRepoDescribePath(sparkwingDir)), nil
	}
	if out := readDescribeFile(describeCachePath(key)); out != nil {
		return out, nil
	}
	// Cached binary exists but describe cache is missing -- regenerate
	// inline. Binary-is-absent means compile would be needed, which is
	// too slow for tab; fall through to the by-repo fallback instead.
	if binPath := bincache.CachedBinaryPath(key); fileExists(binPath) {
		if out, err := refreshDescribeFromBinary(sparkwingDir, binPath, key); err == nil && out != nil {
			return out, nil
		}
	}
	return readDescribeFile(byRepoDescribePath(sparkwingDir)), nil
}

// readDescribeFile reads and decodes one describe-cache JSON file.
// Returns nil on miss or corruption -- completion always wants
// silent fallthrough rather than a cryptic JSON error.
func readDescribeFile(path string) []sparkwing.DescribePipeline {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []sparkwing.DescribePipeline
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// fileExists returns true if path is a regular file. Used to gate
// the refresh-on-tab path on "binary cache hit" without incurring
// the cost of an exec attempt on a missing binary.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// refreshDescribeFromBinary runs `<binPath> --describe`, persists the
// result under both the content-keyed and by-repo cache paths, and
// returns the parsed schema. Called from readDescribeCache when the
// content-keyed describe file is missing but the corresponding
// pipeline binary still lives in the bincache.
//
// Kept separate from writeDescribeCache because the compile-time
// caller already has the JSON bytes in hand; this path rediscovers
// them from disk and wants the parsed form back for immediate use.
func refreshDescribeFromBinary(sparkwingDir, binPath, key string) ([]sparkwing.DescribePipeline, error) {
	cmd := exec.Command(binPath, "--describe")
	cmd.Dir = filepath.Dir(sparkwingDir)
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("run %s --describe: %w", binPath, err)
	}
	var schemas []sparkwing.DescribePipeline
	if err := json.Unmarshal(raw, &schemas); err != nil {
		return nil, fmt.Errorf("parse --describe output: %w", err)
	}
	writeDescribeFile(describeCachePath(key), raw)
	writeDescribeFile(byRepoDescribePath(sparkwingDir), raw)
	return schemas, nil
}

// writeDescribeFile is a silent-on-failure writer used for both the
// content-keyed and by-repo cache paths. Completion callers never
// want a mid-tab crash if $SPARKWING_HOME becomes unwritable.
func writeDescribeFile(path string, raw []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o644)
}

// writeDescribeCache invokes `<binPath> --describe`, validates the
// output is parseable JSON, and persists it under the cache key.
// Called from compileAndExec after a successful build so every
// pipeline the user runs keeps its cache warm. Failures are
// non-fatal for the caller -- the cache is a perf optimization,
// not a correctness invariant.
func writeDescribeCache(sparkwingDir, binPath string) error {
	key, err := bincache.PipelineCacheKey(sparkwingDir)
	if err != nil {
		return fmt.Errorf("cache key: %w", err)
	}

	// Invoke in a child process (not ExecReplace) so we can capture
	// stdout and continue to the actual pipeline run afterward.
	// Run from the repo root so the binary's cwd matches what it'd
	// see under normal execution -- the SDK walks up from cwd to
	// find `.sparkwing/`.
	cmd := exec.Command(binPath, "--describe")
	cmd.Dir = filepath.Dir(sparkwingDir)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("run %s --describe: %w", binPath, err)
	}
	var schemas []sparkwing.DescribePipeline
	if err := json.Unmarshal(out, &schemas); err != nil {
		return fmt.Errorf("parse --describe output: %w", err)
	}

	path := describeCachePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Mirror the same JSON under a per-repo path so tab completion
	// still has a "last known" schema to fall back on when the content
	// key shifts (mid-edit). Mirror failures are non-fatal.
	writeDescribeFile(byRepoDescribePath(sparkwingDir), out)
	return nil
}

// pipelineFlagsFromCache looks up one pipeline's typed flags from
// the describe cache rooted at sparkwingDir. Returns nil, nil when
// the cache is empty or the pipeline name isn't known.
func pipelineFlagsFromCache(sparkwingDir, pipelineName string) ([]sparkwing.DescribeArg, error) {
	schemas, err := readDescribeCache(sparkwingDir)
	if err != nil {
		return nil, err
	}
	for _, s := range schemas {
		if s.Name == pipelineName {
			return s.Args, nil
		}
	}
	return nil, nil
}
