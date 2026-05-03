// Bootstrap helpers for `sparkwing pipeline new` -- when the user
// creates their first pipeline in a fresh repo, the .sparkwing/
// skeleton (go.mod, main.go, pipelines.yaml) is written here, then
// the requested pipeline is scaffolded on top. writeSkeleton +
// renderInit* are the file emitters; printInitReport renders the
// post-bootstrap output. There is no jobs/ package anchor: bootstrap
// is only called from pipeline-new, so the user's first scaffold
// always lands a real .go file in jobs/ on the very next step --
// no placeholder needed.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sparkwing-dev/sparkwing-cli/pkg/color"
)

// fallbackSDKVersion is the sparkwing module version pinned into a
// freshly-generated .sparkwing/go.mod when the running CLI's own
// version can't be detected (e.g. binary downloaded from
// sparkwing.dev rather than `go install`'d). Bump on each release.
// `go mod tidy` will resolve this to the actual latest if Go is on
// PATH, so the constant is a fail-safe, not the source of truth.
const fallbackSDKVersion = "v0.41.9"

// bootstrapDotSparkwing creates the .sparkwing/ skeleton (go.mod,
// main.go, pipelines.yaml) and prints the section header + file
// list. `go mod tidy` is deferred to the scaffolder: an empty
// jobs/ package anchored only by main.go's blank import would fail
// tidy with "no Go files in <module>/jobs", and the scaffolder is
// always the next step (it writes the first jobs/*.go).
//
// Called by `sparkwing pipeline new --name X` when it discovers the
// .sparkwing/ directory is missing -- the user gets bootstrapped
// implicitly on first pipeline creation, no separate `init` step
// needed.
//
// Module name defaults to `<cwd-basename>-pipelines`. The caller
// owns the post-bootstrap pipeline scaffold (writes jobs/<name>.go,
// appends to pipelines.yaml, runs tidy) -- bootstrap only sets up
// the package skeleton (go.mod, main.go, pipelines.yaml).
// terse=true (used when called from `sparkwing pipeline new`)
// suppresses the "next steps" block -- the scaffolder prints its
// own, naming the actual pipeline being created. Printing both
// produced a confusing concatenated wall where the bootstrap block
// told the user to scaffold the same pipeline they had just
// scaffolded. The toolchain-missing alert still prints in terse
// mode because that's a real blocker.
func bootstrapDotSparkwingOpts(cwd, sparkwingDir string, terse bool) error {
	moduleName := filepath.Base(cwd) + "-pipelines"
	existed := dirExists(sparkwingDir)
	report, err := writeSkeleton(sparkwingDir, moduleName, false)
	if err != nil {
		return err
	}
	// tidy is deferred to the scaffolder -- jobs/ is empty until
	// the first pipeline file lands, and tidying an empty package
	// fails. The scaffolder runs tidy once after writing jobs/<name>.go.
	printInitReport(cwd, moduleName, existed, report, tidyStatus{Skipped: true}, terse)
	return nil
}

// initFileReport tracks per-file outcomes so the post-init summary
// can tell the user which pieces were created vs already in place.
// Useful when re-running init on a half-set-up project: a clear
// "this file existed, this one we just wrote" listing demystifies
// the idempotent behavior.
type initFileReport struct {
	Created []string
	Existed []string
	Skipped []string // skipped because --force wasn't passed and file existed
}

func writeSkeleton(sparkwingDir, moduleName string, force bool) (initFileReport, error) {
	rep := initFileReport{}

	if err := os.MkdirAll(sparkwingDir, 0o755); err != nil {
		return rep, fmt.Errorf("init: create %s: %w", sparkwingDir, err)
	}
	for _, sub := range []string{"jobs"} {
		if err := os.MkdirAll(filepath.Join(sparkwingDir, sub), 0o755); err != nil {
			return rep, fmt.Errorf("init: create %s/%s: %w", sparkwingDir, sub, err)
		}
	}

	files := []struct {
		Path    string
		Content func() string
	}{
		{filepath.Join(sparkwingDir, "go.mod"), func() string { return renderInitGoMod(moduleName) }},
		{filepath.Join(sparkwingDir, "main.go"), func() string { return renderInitMainGo(moduleName) }},
		{filepath.Join(sparkwingDir, "pipelines.yaml"), func() string { return renderInitPipelinesYAML() }},
	}
	for _, f := range files {
		rel, _ := filepath.Rel(filepath.Dir(sparkwingDir), f.Path)
		if _, err := os.Stat(f.Path); err == nil {
			if !force {
				rep.Existed = append(rep.Existed, rel)
				continue
			}
			rep.Skipped = append(rep.Skipped, rel)
			continue
		}
		if err := os.WriteFile(f.Path, []byte(f.Content()), 0o644); err != nil {
			return rep, fmt.Errorf("init: write %s: %w", f.Path, err)
		}
		rep.Created = append(rep.Created, rel)
	}

	// .gitignore the cached pipeline binary so a future `go build` /
	// `wing` invocation's intermediate doesn't get committed.
	if err := ensureGitignoreEntry(filepath.Dir(sparkwingDir), ".sparkwing/sparkwing-pipeline"); err != nil {
		// Non-fatal: not every project tracks its .gitignore via git
		// (e.g. throwaway test repos). Surface as a note, not a crash.
		fmt.Fprintf(os.Stderr, "init: note: could not update .gitignore: %v\n", err)
	}

	return rep, nil
}

func renderInitGoMod(moduleName string) string {
	goDirective := userGoModDirective()
	if goDirective == "" {
		// Fallback for the no-Go-on-PATH case. We pick a recent but
		// not bleeding-edge version so any modern Go install can
		// auto-switch toolchains without complaining.
		goDirective = "1.22"
	}
	return fmt.Sprintf(`module %s

go %s

require github.com/sparkwing-dev/sparkwing-sdk %s
`, moduleName, goDirective, sdkRequirementVersion())
}

// sdkRequirementVersion picks the sparkwing module version to pin
// in a freshly-generated go.mod. Prefers the running CLI's own
// version when it parses as a clean semver tag; otherwise falls
// back to the fallbackSDKVersion constant. `go mod tidy` resolves
// to actual latest at first compile if Go is on PATH, so a
// slightly-stale fallback isn't load-bearing.
//
// Strips pseudo-version suffixes ("+dirty", "-rc1", anything after
// the vX.Y.Z): a tag like "v0.41.0+dirty" from runtime/debug isn't
// a valid go.mod require version. We round down to vX.Y.Z so the
// generated file is always parseable by `go mod tidy`.
func sdkRequirementVersion() string {
	v := installedVersion()
	if !strings.HasPrefix(v, "v") || strings.Contains(v, "(") {
		return fallbackSDKVersion
	}
	// Trim at the first "+" or "-" after the version number.
	clean := v
	if idx := strings.IndexAny(clean, "+-"); idx >= 0 {
		clean = clean[:idx]
	}
	// Sanity: must look like vX.Y.Z (three dot-separated parts).
	parts := strings.Split(strings.TrimPrefix(clean, "v"), ".")
	if len(parts) != 3 {
		return fallbackSDKVersion
	}
	return clean
}

func renderInitMainGo(moduleName string) string {
	return fmt.Sprintf(`// Command %s is this repo's local pipeline runner.
// It re-exports orchestrator.Main, which dispatches based on argv:
// `+"`wing <pipeline>`"+` invokes the pipeline; `+"`sparkwing pipeline ...`"+`
// is the agent/operator surface.
package main

import (
	"github.com/sparkwing-dev/sparkwing-sdk/orchestrator"

	_ %q
)

func main() { orchestrator.Main() }
`, moduleName, moduleName+"/jobs")
}

func renderInitPipelinesYAML() string {
	return `# Registry of every pipeline this repo defines. Each entry
# below becomes an invocable target for ` + "`sparkwing run <name>`" + `
# (or the human shortcut ` + "`wing <name>`" + `).
#
# Add an entry by running:
#   sparkwing pipeline new --name <name>
#
# (Default template is minimal; pass --template build-test-deploy
# for a build/test/deploy DAG.)
pipelines:
`
}

// ensureGitignoreEntry appends a single line to the repo root's
// .gitignore if it isn't already present. Creates .gitignore with
// the entry if no .gitignore exists. Idempotent: running twice
// leaves a single line.
func ensureGitignoreEntry(repoRoot, entry string) error {
	path := filepath.Join(repoRoot, ".gitignore")
	body, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		body = nil
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	var b strings.Builder
	if len(body) > 0 {
		b.Write(body)
		if !strings.HasSuffix(string(body), "\n") {
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n# sparkwing: cached pipeline binary, regenerated on each `wing` invocation\n")
	b.WriteString(entry)
	b.WriteByte('\n')
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// tidyStatus captures the outcome of the post-scaffold `go mod tidy`
// so printInitReport can render a single status line. Empty Note +
// Skipped=true => omit the line entirely (no Go on PATH, etc.).
type tidyStatus struct {
	Skipped bool
	OK      bool
	Note    string // displayed in the status line; populated on success or failure
	Err     string // multi-line stderr to dump on failure
}

// tidySkeleton runs `go mod tidy` inside the freshly-written
// .sparkwing/ so go.sum is populated before the user's first
// `wing <name>` invocation. Without this, a Go-template pipeline's
// first run fails with "missing go.sum entry" -- a confusing first
// experience, especially for users who haven't touched Go in years.
//
// Skipped silently when Go isn't on PATH (the toolchain warning
// already covers that path). When createdAny is false (idempotent
// re-run with no new files) we still tidy, since a previous init
// could have stopped before tidy ran -- belt-and-suspenders is
// cheaper than the user wondering why `wing release` still fails.
func tidySkeleton(sparkwingDir string, createdAny bool) tidyStatus {
	_ = createdAny // kept for future short-circuit; current behavior tidies always when Go is present
	if !goOnPath() {
		return tidyStatus{Skipped: true}
	}
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = sparkwingDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return tidyStatus{OK: false, Note: "go mod tidy failed", Err: strings.TrimSpace(string(out))}
	}
	return tidyStatus{OK: true, Note: "resolved dependencies (go mod tidy)"}
}

// printInitReport renders the post-init summary: file pipelines,
// toolchain alert (only when Go is missing -- the happy path is
// silent), next-step recipe, and the opt-in CLAUDE.md/AGENTS.md
// block. The block is a copy-paste suggestion, not an auto-written
// file, so users who don't use AI tools never have a surprise edit
// on their disk.
func printInitReport(cwd, moduleName string, existedBefore bool, rep initFileReport, tidy tidyStatus, terse bool) {
	if existedBefore {
		fmt.Printf("%s .sparkwing already in place (module %s)\n", color.Cyan("==>"), moduleName)
	} else {
		fmt.Printf("%s bootstrapping .sparkwing\n", color.Cyan("==>"))
	}

	// File pipelines. "+" / "-" glyphs match the `pipeline new`
	// scaffold output for a unified visual rhythm across both flows.
	for _, p := range rep.Created {
		fmt.Printf("  %s %s\n", color.Green("+"), p)
	}
	for _, p := range rep.Existed {
		fmt.Printf("  %s %s\n", color.Dim("="), color.Dim(p))
	}
	for _, p := range rep.Skipped {
		fmt.Printf("  %s %s %s\n", color.Yellow("!"), p, color.Dim("(kept; pass --force to overwrite)"))
	}
	switch {
	case tidy.Skipped:
		// nothing to print
	case tidy.OK:
		fmt.Printf("  %s resolved dependencies (go mod tidy)\n", color.Green("+"))
	default:
		fmt.Printf("  %s go mod tidy %s\n", color.Red("x"), color.Dim("(see error below)"))
		if tidy.Err != "" {
			for _, line := range strings.Split(tidy.Err, "\n") {
				fmt.Printf("      %s\n", color.Dim(line))
			}
		}
	}

	// Toolchain alert: only shown when Go is missing.
	if !goOnPath() {
		fmt.Println()
		fmt.Println("toolchain: Go is NOT on PATH")
		fmt.Printf("  %s\n", goInstallHintForce())
	}

	// In terse mode (called from `sparkwing pipeline new`), skip the
	// "next steps" block -- the scaffolder owns that output, naming
	// the actual pipeline being created. Printing both produced a
	// confusing concat where bootstrap told the user to scaffold the
	// same pipeline they had just scaffolded.
	if terse {
		return
	}

	fmt.Println()
	fmt.Println("next steps:")
	fmt.Printf("  1. sparkwing pipeline new --name release   %s\n", color.Dim("# scaffold a single-node pipeline (default --template minimal)"))
	fmt.Printf("  2. sparkwing run release                   %s\n", color.Dim("# run it; replace the Log(\"TODO\") with real logic"))
	fmt.Printf("  %s\n", color.Dim("for a build/test/deploy DAG: sparkwing pipeline new --name release --template build-test-deploy"))
	fmt.Println()
	fmt.Printf("  %s\n", color.Dim("dashboard:    sparkwing dashboard start"))
	fmt.Printf("  %s\n", color.Dim("docs:         sparkwing docs list  (or https://sparkwing.dev/docs)"))
	fmt.Printf("  %s\n", color.Dim("AI agents:    sparkwing info --for-agent  (paste into CLAUDE.md / AGENTS.md)"))
}
