// Scaffolds a new pipeline in the nearest .sparkwing/. Two templates:
//
//   - minimal (default): a Plan-returning Go pipeline with one stubbed
//     node. Plan() is the entrypoint so adding/removing nodes is a
//     one-line edit instead of a refactor.
//   - build-test-deploy: a three-node Plan (build -> test -> deploy)
//     with stubbed bodies. The canonical CI/CD-shaped template.
//
// Goal is "get to a compiling, runnable stub fast" -- the generated
// code includes registration + pipelines.yaml entry, plus matching
// ShortHelp / Help / Examples so the entry shows up correctly in
// `sparkwing pipeline list`.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	flag "github.com/spf13/pflag"

	"github.com/sparkwing-dev/sparkwing-cli/pkg/color"
	"github.com/sparkwing-dev/sparkwing-cli/pkg/pipelines"
)

func runPipelineNew(args []string) error {
	fs := flag.NewFlagSet(cmdPipelineNew.Path, flag.ContinueOnError)
	pipelineName := fs.String("name", "", "new pipeline name (kebab-case, e.g. deploy-staging)")
	template := fs.String("template", "minimal", "template: minimal (default) | build-test-deploy")
	group := fs.String("group", "", "group name (surfaces in wing <TAB> section header)")
	hidden := fs.Bool("hidden", false, "mark the entry hidden in tab-complete menus")
	short := fs.String("short", "", "short one-line description (ShortHelp / frontmatter desc)")
	if err := parseAndCheck(cmdPipelineNew, fs, args); err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		PrintHelp(cmdPipelineNew, os.Stderr)
		return fmt.Errorf("new: unexpected positional %q (use --name)", fs.Arg(0))
	}
	if *pipelineName == "" {
		PrintHelp(cmdPipelineNew, os.Stderr)
		return errors.New("new: --name is required (e.g. --name deploy-staging)")
	}
	name := *pipelineName
	if err := validatePipelineName(name); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	sparkwingDir, ok := walkUpForSparkwing(cwd)
	bootstrapped := !ok
	if !ok {
		// Fresh repo: bootstrap the .sparkwing/ skeleton (go.mod,
		// main.go, pipelines.yaml, jobs/doc.go) before scaffolding
		// the requested pipeline. Bootstrap is now sample-free --
		// the user's first scaffold IS the first runnable pipeline,
		// no surprise hello.go to delete afterward.
		if err := bootstrapDotSparkwingOpts(cwd, filepath.Join(cwd, ".sparkwing"), true); err != nil {
			return err
		}
		sparkwingDir = filepath.Join(cwd, ".sparkwing")
	}

	// Refuse clobber: fail if the name is already in pipelines.yaml.
	// Easier to diagnose than a silent overwrite.
	if _, cfg, derr := pipelines.Discover(cwd); derr == nil && cfg != nil {
		for _, p := range cfg.Pipelines {
			if p.Name == name {
				return fmt.Errorf("pipeline %q already exists in pipelines.yaml (entrypoint %q)", name, p.Entrypoint)
			}
		}
	}

	// Warn (don't fail) when scaffolding on a machine without Go.
	// The file writes fine; the user just won't be able to run it
	// until Go is on PATH. Surfacing this after success would bury
	// the lede; surfacing as an error would block valid use cases
	// (e.g. authoring on a checkout that'll be built elsewhere).
	if hint := goInstallHint(); hint != "" {
		fmt.Fprintln(os.Stderr, "warning: Go is not on PATH. Scaffolding will succeed but `sparkwing run "+name+"` will fail until Go is installed.")
		fmt.Fprintln(os.Stderr, "  "+hint)
		fmt.Fprintln(os.Stderr)
	}

	switch *template {
	case "minimal":
		return scaffoldGoMinimal(sparkwingDir, name, *group, *hidden, *short, bootstrapped)
	case "build-test-deploy":
		return scaffoldGoBuildTestDeploy(sparkwingDir, name, *group, *hidden, *short, bootstrapped)
	default:
		return fmt.Errorf("new: unknown template %q (valid: minimal, build-test-deploy)", *template)
	}
}

// validatePipelineName enforces kebab-case lower-letters+digits+dashes
// so the name round-trips cleanly through yaml, shell, and Go-
// identifier conversion. Catches mistakes (uppercase, spaces) early
// before we write a half-broken scaffold.
func validatePipelineName(name string) error {
	if name == "" {
		return errors.New("name: must not be empty")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return errors.New("name: must not start or end with '-'")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return errors.New("name: must start with a letter")
			}
		case r == '-':
			if i > 0 && name[i-1] == '-' {
				return errors.New("name: must not contain '--'")
			}
		default:
			return fmt.Errorf("name: invalid character %q (kebab-case only: a-z, 0-9, -)", r)
		}
	}
	return nil
}

func kebabToPascal(name string) string {
	var b strings.Builder
	capitalize := true
	for _, r := range name {
		if r == '-' {
			capitalize = true
			continue
		}
		if capitalize {
			b.WriteRune(unicode.ToUpper(r))
			capitalize = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func kebabToSnake(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

// goReservedTrailingTokens are tokens Go treats specially when they
// appear as the *last* underscore-separated segment of a .go filename.
// Source: `go tool dist list` (every GOOS + GOARCH) plus the test
// convention. With any of these as the trailing token a file is
// either test-only (`_test.go`) or build-tagged for one OS/arch and
// silently omitted everywhere else -- a scaffold landing in either
// state fails opaquely on the user's first run.
//
// Maintenance note: Go adds new GOOS/GOARCH values rarely. When that
// happens this list goes stale; the failure mode is "scaffolder lets
// a name through that bites the user later," not a wrong-output bug,
// so it's caught by user reports and added here.
var goReservedTrailingTokens = map[string]bool{
	"test": true,
	// GOOS
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true,
	"js": true, "linux": true, "nacl": true, "netbsd": true,
	"openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
	"windows": true, "zos": true,
	// GOARCH
	"386": true, "amd64": true, "amd64p32": true, "arm": true,
	"arm64": true, "arm64be": true, "armbe": true, "loong64": true,
	"mips": true, "mips64": true, "mips64le": true, "mips64p32": true,
	"mips64p32le": true, "mipsle": true, "ppc": true, "ppc64": true,
	"ppc64le": true, "riscv": true, "riscv64": true, "s390": true,
	"s390x": true, "sparc": true, "sparc64": true, "wasm": true,
}

// goJobFilename turns a pipeline name into the .go file that will
// hold its scaffold, dodging every way Go would silently exclude the
// file from the build:
//
//   - Files starting with `_` or `.` are ignored by `go build`
//     entirely. Prefix `pipeline_` in that case.
//   - Files ending in `_test.go` are compiled only under `go test`,
//     and `_<goos>.go` / `_<goarch>.go` / `_<goos>_<goarch>.go` are
//     build-tagged. The trailing-segment check against
//     goReservedTrailingTokens covers all of these uniformly: when
//     the last `_`-separated token is reserved, append `_pipeline`.
//     Single-token names like `test` or `linux` keep their plain
//     filename -- Go only triggers the `name_<goos>.go` rule when
//     there's an underscore-separator before the trailing token.
//
// All transforms preserve the user-chosen pipeline name in
// pipelines.yaml; only the on-disk filename is adjusted.
func goJobFilename(name string) string {
	snake := kebabToSnake(name)
	if strings.HasPrefix(snake, "_") || strings.HasPrefix(snake, ".") {
		snake = "pipeline_" + snake
	}
	if parts := strings.Split(snake, "_"); len(parts) >= 2 {
		last := parts[len(parts)-1]
		if goReservedTrailingTokens[last] {
			snake += "_pipeline"
		}
	}
	return snake + ".go"
}

// scaffoldGoMinimal emits a Plan-returning pipeline with one stubbed
// node. Default template: smallest viable shape that still teaches
// the canonical Plan() entry-point so editing means "add another
// sparkwing.Job(plan, ...) line", not "refactor a one-step pipeline into Plan()".
func scaffoldGoMinimal(sparkwingDir, name, group string, hidden bool, short string, bootstrapped bool) error {
	return scaffoldGoFromTemplate(sparkwingDir, name, group, hidden, short, minimalTemplate, bootstrapped)
}

// scaffoldGoBuildTestDeploy emits the canonical 3-node CI Plan:
// build -> test -> deploy. All three Run bodies are stubbed but
// pass, so a fresh scaffold + `wing <name>` succeeds end-to-end and
// the editor sees the shape the moment they open the file.
func scaffoldGoBuildTestDeploy(sparkwingDir, name, group string, hidden bool, short string, bootstrapped bool) error {
	return scaffoldGoFromTemplate(sparkwingDir, name, group, hidden, short, buildTestDeployTemplate, bootstrapped)
}

// scaffoldGoFromTemplate is the shared write path. Each template is
// a Go-source string with {{NAME}}, {{STRUCT}}, {{SHORTLIT}}
// placeholders -- SHORTLIT is the strconv.Quote'd literal so user-
// supplied shorts containing quotes don't break the generated file.
func scaffoldGoFromTemplate(sparkwingDir, name, group string, hidden bool, short, tmpl string, bootstrapped bool) error {
	struct_ := kebabToPascal(name)
	file := filepath.Join(sparkwingDir, "jobs", goJobFilename(name))
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("refusing to overwrite %s\n  pick a different --name, or delete the file first if you want to regenerate", file)
	}
	if short == "" {
		short = "TODO: one-line description of " + name
	}
	body := strings.NewReplacer(
		"{{STRUCT}}", struct_,
		"{{NAME}}", name,
		"{{SHORTLIT}}", strconv.Quote(short),
	).Replace(tmpl)
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		return err
	}
	if err := appendPipelinesYAML(sparkwingDir, name, struct_, group, hidden); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(sparkwingDir), file)
	if err != nil {
		rel = file
	}
	if bootstrapped {
		// Visual gap between the bootstrap section and the scaffold
		// section so the two `==>` headers don't run together. On a
		// second-or-later run (no bootstrap fired) the scaffold output
		// is the only output, so we skip the leading blank to avoid an
		// awkward empty first line.
		fmt.Println()
	}
	fmt.Printf("%s Creating new pipeline\n", color.Cyan("==>"))
	fmt.Printf("  %s %s\n", color.Green("+"), rel)
	fmt.Printf("  %s added %q entry to .sparkwing/pipelines.yaml\n", color.Green("+"), name)
	tidy := tidySkeleton(sparkwingDir, true)
	switch {
	case tidy.Skipped:
		// nothing
	case tidy.OK:
		fmt.Printf("  %s %s\n", color.Green("+"), color.Dim(tidy.Note))
	default:
		fmt.Printf("  %s %s\n", color.Red("x"), tidy.Note)
		if tidy.Err != "" {
			for _, line := range strings.Split(tidy.Err, "\n") {
				fmt.Printf("      %s\n", color.Dim(line))
			}
		}
	}
	fmt.Println()
	fmt.Println("tips:")
	fmt.Printf("  %s %s\n", color.Cyan("sparkwing run "+name), color.Dim("# run it"))
	fmt.Printf("  %s %s\n", color.Cyan("sparkwing docs read --topic sdk"), color.Dim("# SDK reference for editing the stub"))
	fmt.Printf("  %s %s\n", color.Cyan("sparkwing docs read --topic pipelines"), color.Dim("# pipelines.yaml + DAG concepts"))
	fmt.Printf("  %s %s\n", color.Cyan("sparkwing runs status --run <id>"), color.Dim("# inspect a previous run by id"))
	fmt.Printf("  %s %s\n", color.Cyan("sparkwing dashboard start"), color.Dim("# see runs in local dashboard"))
	fmt.Printf("  %s %s\n", color.Cyan("sparkwing info"), color.Dim("# find out more about sparkwing"))
	return nil
}

// minimalTemplate: a Plan-shaped pipeline with one stubbed node. The
// SDK is imported under the short alias `sw` -- the convention across
// every pipeline so authors see one consistent surface
// (`sw.Job(...)`, `sw.Output[T](...)` etc.). Plan() is the
// entry-point: registering nodes is `sw.Job(plan, "name",
// &Job{}).Needs(prev)`. Comments are deliberately minimal: stubs are
// a finger pointing at `sparkwing docs read --topic sdk`, not a copy
// of the SDK reference. Inline cookbooks drift; pointers don't.
const minimalTemplate = `package jobs

import (
	"context"

	sw "github.com/sparkwing-dev/sparkwing-sdk/sparkwing"
)

// {{STRUCT}} is a sparkwing pipeline. See ` + "`sparkwing docs read --topic sdk`" + ` for SDK helpers.
type {{STRUCT}} struct{ sw.Base }

func (p {{STRUCT}}) ShortHelp() string { return {{SHORTLIT}} }

// Help is the long-form description; defaults to ShortHelp until you have more to say.
func (p {{STRUCT}}) Help() string { return p.ShortHelp() }

func ({{STRUCT}}) Examples() []sw.Example {
	return []sw.Example{
		{Comment: "Run locally", Command: "wing {{NAME}}"},
	}
}

// Plan registers the pipeline's DAG on the passed-in *Plan. run
// carries run-time environment: run.Args (CLI flags), run.Git (repo
// state), run.Trigger (push/manual/schedule/webhook), run.Pipeline
// (registered name).
func ({{STRUCT}}) Plan(ctx context.Context, plan *sw.Plan, _ sw.NoInputs, run sw.RunContext) error {
	sw.Job(plan, run.Pipeline, &{{STRUCT}}Job{})
	return nil
}

type {{STRUCT}}Job struct{ sw.Base }

func (j *{{STRUCT}}Job) Work() *sw.Work {
	w := sw.NewWork()
	w.Step("run", j.run)
	return w
}

// Paths in ExecIn / BashIn / ReadFile are relative to the repo root,
// not .sparkwing/. See WorkDir().
func ({{STRUCT}}Job) run(ctx context.Context) error {
	sw.Info(ctx, "TODO: replace with your logic")
	return nil
}

func init() {
	sw.Register[sw.NoInputs]("{{NAME}}", func() sw.Pipeline[sw.NoInputs] { return &{{STRUCT}}{} })
}
`

// buildTestDeployTemplate: the canonical CI shape. Three nodes with
// classic build->test->deploy ordering. Each Run shells `echo` so
// `wing <name>` succeeds end-to-end on first invocation; the user
// fills in real commands once they see the structure pass. The
// inline DAG comment is intentional (pipeline-specific structure,
// not SDK reference) -- the SDK cookbook lives in `docs read
// --topic sdk` and the stub points there rather than copying it.
const buildTestDeployTemplate = `package jobs

import (
	"context"

	sw "github.com/sparkwing-dev/sparkwing-sdk/sparkwing"
)

// {{STRUCT}} is a build/test/deploy pipeline.
//
//   build -> test -> deploy
//
// See ` + "`sparkwing docs read --topic sdk`" + ` for SDK helpers.
type {{STRUCT}} struct{ sw.Base }

func (p {{STRUCT}}) ShortHelp() string { return {{SHORTLIT}} }

// Help is the long-form description; defaults to ShortHelp until you have more to say.
func (p {{STRUCT}}) Help() string { return p.ShortHelp() }

func ({{STRUCT}}) Examples() []sw.Example {
	return []sw.Example{
		{Comment: "Run locally", Command: "wing {{NAME}}"},
	}
}

// Plan registers the pipeline's DAG on the passed-in *Plan. run
// carries run-time environment: run.Args (CLI flags), run.Git (repo
// state), run.Trigger (push/manual/schedule/webhook), run.Pipeline
// (registered name).
func ({{STRUCT}}) Plan(ctx context.Context, plan *sw.Plan, _ sw.NoInputs, run sw.RunContext) error {
	build := sw.Job(plan, "build", &{{STRUCT}}BuildJob{})
	test := sw.Job(plan, "test", &{{STRUCT}}TestJob{}).Needs(build)
	sw.Job(plan, "deploy", &{{STRUCT}}DeployJob{}).Needs(test)
	return nil
}

type {{STRUCT}}BuildJob struct{ sw.Base }

func (j *{{STRUCT}}BuildJob) Work() *sw.Work {
	w := sw.NewWork()
	w.Step("run", j.run)
	return w
}

// Paths in .Dir() / ReadFile are relative to the repo root, not
// .sparkwing/. See WorkDir().
func ({{STRUCT}}BuildJob) run(ctx context.Context) error {
	_, err := sw.Bash(ctx, ` + "`echo \"TODO: build\"`" + `).Run()
	return err
}

type {{STRUCT}}TestJob struct{ sw.Base }

func (j *{{STRUCT}}TestJob) Work() *sw.Work {
	w := sw.NewWork()
	w.Step("run", j.run)
	return w
}

func ({{STRUCT}}TestJob) run(ctx context.Context) error {
	_, err := sw.Bash(ctx, ` + "`echo \"TODO: test\"`" + `).Run()
	return err
}

type {{STRUCT}}DeployJob struct{ sw.Base }

func (j *{{STRUCT}}DeployJob) Work() *sw.Work {
	w := sw.NewWork()
	w.Step("run", j.run)
	return w
}

func ({{STRUCT}}DeployJob) run(ctx context.Context) error {
	_, err := sw.Bash(ctx, ` + "`echo \"TODO: deploy\"`" + `).Run()
	return err
}

func init() {
	sw.Register[sw.NoInputs]("{{NAME}}", func() sw.Pipeline[sw.NoInputs] { return &{{STRUCT}}{} })
}
`

// appendPipelinesYAML tacks a new entry onto .sparkwing/pipelines.yaml
// in the same shape the existing entries use. Plain text append keeps
// the author's formatting (leading comments, spacing) intact -- a yaml
// round-trip would reflow everything. Risk: the user's file could have
// exotic yaml that we don't preserve; mitigated by the simplicity of
// the append (we only add, never modify).
func appendPipelinesYAML(sparkwingDir, name, entrypoint, group string, hidden bool) error {
	path := filepath.Join(sparkwingDir, "pipelines.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n  - name: %s\n    entrypoint: %s\n", name, entrypoint)
	if group != "" {
		fmt.Fprintf(&b, "    group: %s\n", group)
	}
	if hidden {
		b.WriteString("    hidden: true\n")
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}
