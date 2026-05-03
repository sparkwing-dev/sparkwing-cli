// `sparkwing info` is the agent-facing entrypoint command: one
// invocation that answers "what is sparkwing, am I inside a project,
// what can I do next" without requiring any other prior knowledge.
// It is the canonical first command an agent runs after install.
//
// Output modes mirror `sparkwing pipeline list`: default human-aligned
// table; --json (or -o json) for structured agent consumption; -o
// plain emits one next-step command per line for shell pipelines.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	flag "github.com/spf13/pflag"
	"golang.org/x/mod/semver"

	"github.com/sparkwing-dev/sparkwing-cli/pkg/color"
)

// Info is the JSON shape of `sparkwing info`. Stable contract: agents
// parse this directly. Field renames here are breaking changes.
type Info struct {
	About     string         `json:"about"`
	Version   InfoVersion    `json:"version"`
	Binary    string         `json:"binary"`
	Project   InfoProject    `json:"project"`
	Toolchain InfoToolchain  `json:"toolchain"`
	NextSteps []InfoNextStep `json:"next_steps"`
	// ForAgents lists the machine-readable surfaces (commands,
	// JSON variants, paste blocks) split off from NextSteps so the
	// human verbs and the agent discovery list don't compete in one
	// pile. JSON consumers can branch on the two lists directly.
	ForAgents    []InfoNextStep `json:"for_agents,omitempty"`
	Tips         []InfoTip      `json:"tips,omitempty"`
	Docs         InfoDocs       `json:"docs"`
	FirstRunNote string         `json:"first_run_note"`
}

// InfoTip is a context-aware suggestion. Each gate runs locally
// (file existence, process check) or with a cheap network probe
// (~3s timeout, fail-soft). Tips only render when their gate
// fires, so seasoned users see a clean card while new installs
// see relevant nudges. ID is stable so agents can branch on it.
type InfoTip struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Command string `json:"command,omitempty"`
	Note    string `json:"note,omitempty"`
}

// InfoVersion separates the raw version string (what runtime/debug
// gives us) from the parsed semver and the build provenance. Agents
// reading the JSON should branch on BuildType / IsRelease rather
// than string-matching the Installed field, since "+dirty" suffixes
// and "(devel)" sentinels make raw equality brittle.
type InfoVersion struct {
	// Installed is the literal version string the running binary
	// reports (runtime/debug.Main.Version). Examples: "v0.41.8",
	// "v0.41.7+dirty", "(devel)", "(unknown)".
	Installed string `json:"installed"`

	// Semver is the canonical X.Y.Z form with any "+dirty" or
	// other suffix stripped. Empty for non-semver builds (devel,
	// unknown). Use this when comparing against a target version.
	Semver string `json:"semver,omitempty"`

	// IsRelease is true when this binary is a clean published
	// release (semver tag with no dirty marker). Releases come
	// from sparkwing.dev/releases/* or `go install ...@vX.Y.Z`.
	IsRelease bool `json:"is_release"`

	// IsDirty is true when the binary was built from a working
	// tree with uncommitted changes. Implies the version may not
	// match what's actually compiled in.
	IsDirty bool `json:"is_dirty"`

	// BuildType labels the provenance succinctly:
	//   "release"      -- clean tagged build (the default for
	//                     anyone who installed via install.sh)
	//   "local-clean"  -- built from local source, working tree
	//                     was clean at build time
	//   "local-dirty"  -- built from local source with
	//                     uncommitted changes
	//   "devel"        -- runtime/debug returned "(devel)"; happens
	//                     for `go run` or builds without VCS info
	//   "unknown"      -- runtime/debug had no Main.Version at all
	BuildType string `json:"build_type"`

	// HumanLabel is a one-phrase explanation of BuildType suitable
	// for display next to the version in the human card. Empty
	// for plain releases (no extra explanation needed).
	HumanLabel string `json:"human_label,omitempty"`
}

// InfoDocs surfaces every avenue an agent can use to read sparkwing
// docs: offline via the embedded `sparkwing docs ...` verbs, online
// via sparkwing.dev, or as a single-file dump (llms-full.txt) for
// "load the whole corpus into context with one fetch."
type InfoDocs struct {
	CLI      string `json:"cli"`       // command to read docs from the binary
	Web      string `json:"web"`       // human-readable site
	LLMsFull string `json:"llms_full"` // single-file corpus URL
	LLMsTXT  string `json:"llms_txt"`  // llms.txt index URL
}

type InfoProject struct {
	Found        bool             `json:"found"`
	SparkwingDir string           `json:"sparkwing_dir,omitempty"`
	Pipelines    InfoPipelinesSum `json:"pipelines,omitempty"`
	// HowToScaffold is populated only when Found is false; it is the
	// exact command an agent should suggest to the user when no
	// .sparkwing/ exists yet.
	HowToScaffold string `json:"how_to_scaffold,omitempty"`
}

// InfoPipelinesSum summarizes the pipelines in this repo:
// total count, how many are auto-triggered (push/webhook/schedule)
// vs manual-only, and the list of declared groups.
type InfoPipelinesSum struct {
	Total     int      `json:"total"`
	Triggered int      `json:"triggered"`
	Manual    int      `json:"manual"`
	Groups    []string `json:"groups,omitempty"`
}

type InfoToolchain struct {
	Go InfoGoToolchain `json:"go"`
}

type InfoGoToolchain struct {
	Found    bool   `json:"found"`
	Version  string `json:"version,omitempty"`
	Required string `json:"required"`
}

type InfoNextStep struct {
	Command string `json:"command"`
	Purpose string `json:"purpose"`
}

// parseInfoVersion turns runtime/debug's Main.Version into the
// structured InfoVersion shape. Recognized inputs (with how
// runtime/debug produces them):
//
//   - "v0.41.8"           -> release, clean
//   - "v0.41.8+dirty"     -> local build with uncommitted changes
//   - "(devel)"           -> built from local source, no version pin
//   - "(unknown)"         -> debug.ReadBuildInfo had nothing to say
//
// Anything else (e.g. "v0.41.8-rc1") is treated as a release if it
// passes the strict-vX.Y.Z prefix shape, dirty if it doesn't.
func parseInfoVersion(raw string) InfoVersion {
	v := InfoVersion{Installed: raw}
	switch raw {
	case "(devel)":
		v.BuildType = "devel"
		v.HumanLabel = "local source build (no version pin)"
		return v
	case "(unknown)", "":
		v.BuildType = "unknown"
		v.HumanLabel = "version metadata missing"
		return v
	}
	dirty := strings.Contains(raw, "+dirty")
	clean := raw
	if idx := strings.IndexAny(clean, "+-"); idx >= 0 {
		clean = clean[:idx]
	}
	parts := strings.Split(strings.TrimPrefix(clean, "v"), ".")
	if len(parts) == 3 {
		v.Semver = clean
	}
	switch {
	case dirty:
		v.IsDirty = true
		v.BuildType = "local-dirty"
		v.HumanLabel = "local build with uncommitted changes"
	case v.Semver != "":
		v.IsRelease = true
		v.BuildType = "release"
	default:
		v.BuildType = "local-clean"
		v.HumanLabel = "local build (no semver tag)"
	}
	return v
}

const (
	// infoAbout is the one-paragraph "what is sparkwing" tagline
	// shown at the top of the info card. Two short sentences:
	// what it is + where pipelines live + how they're triggered.
	// New users running `sparkwing info` should know what they're
	// looking at without leaving the card.
	infoAbout = "Self-hosted CI/CD platform. Pipelines are Go programs registered " +
		"in `.sparkwing/`, triggered by webhook / schedule / manual run, executed " +
		"on Kubernetes (or locally for dev). https://sparkwing.dev"

	infoFirstRunNote = "The first run of a pipeline (e.g. `sparkwing run release`) compiles " +
		".sparkwing/ from source and downloads Go module dependencies. That can take " +
		"30-90s the first time; subsequent runs hit the on-disk binary cache and start " +
		"instantly."

	infoGoRequirement = "Go-pipeline path requires the Go toolchain on PATH."
)

// infoBat is the mascot rendered above the info card. Indentation is
// load-bearing: the top-left "\" tail hangs off the speech bubble that
// printBatsay draws above it, so don't dedent.
const infoBat = `      /\                        /\
     / \'.__     /\_/\     __.'/ \
    (    '-.___( o   o )___.-'    )
     '-._  __  __'---'__  __  _.-'
         \/  \/         \/  \/`

func runInfo(args []string) error {
	fs := flag.NewFlagSet(cmdInfo.Path, flag.ContinueOnError)
	// Default empty so resolveOutputFormat can distinguish "user
	// passed -o table" from "no -o passed at all" -- the latter must
	// not conflict with --json. resolveOutputFormat treats "" as
	// "fallback to table" only when --json is absent.
	output := fs.StringP("output", "o", "", "output format: table | json | plain (default: table)")
	asJSON := fs.Bool("json", false, "alias for --output json")
	forAgent := fs.Bool("for-agent", false, "emit a paste-ready block for CLAUDE.md / AGENTS.md (no ANSI, no extras)")
	firstTime := fs.Bool("first-time", false, "print the post-install onboarding card (used by install.sh; re-runnable any time)")
	if err := parseAndCheck(cmdInfo, fs, args); err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		PrintHelp(cmdInfo, os.Stderr)
		return fmt.Errorf("info: unexpected positional %q (info takes no positional args)", fs.Arg(0))
	}

	// --for-agent emits a copy-pasteable block meant for CLAUDE.md or
	// AGENTS.md. It overrides any --output / --json setting because the
	// use case is "give me text I can paste" -- mixing it with -o json
	// would produce the structured card and miss the point.
	if *forAgent {
		printAgentBlock()
		return nil
	}

	// --first-time prints the post-install onboarding card. install.sh
	// invokes the freshly-installed binary with this flag so the script
	// stays a thin shim and the content travels with each release.
	// Re-runnable any time -- users can rediscover the steps without
	// hunting for a copy of the installer output.
	if *firstTime {
		printFirstTimeCard()
		return nil
	}

	format, err := resolveOutputFormat(*output, *asJSON, cmdInfo.Path)
	if err != nil {
		return err
	}

	// JSON consumers are agents in practice; tilt next-steps toward
	// discovery automatically.
	info := gatherInfo(format == "json")

	switch format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	case "plain":
		// Plain mode is the "pipe me into something" shape: one
		// next-step command per line so a shell wrapper or agent can
		// `head -n1` for the most-likely thing to run next.
		for _, ns := range info.NextSteps {
			fmt.Println(ns.Command)
		}
		return nil
	default:
		printInfoTable(info)
		return nil
	}
}

// printAgentBlock emits the AGENTS.md / CLAUDE.md paste block. Plain
// text, no ANSI, framed by horizontal rules so a copy-paste lands
// cleanly in a markdown doc. Static -- no per-repo state -- because
// the block is meant to be valid regardless of which sparkwing repo
// the agent enters next; anything project-specific would mislead.
func printAgentBlock() {
	fmt.Println("<!-- Sparkwing context for AI agents. Paste into CLAUDE.md or AGENTS.md and commit. Refresh after major sparkwing upgrades via `sparkwing info --for-agent`. -->")
	fmt.Println()
	fmt.Println("This repo uses **sparkwing** for CI/CD (https://sparkwing.dev). Pipelines are Go")
	fmt.Println("programs in `.sparkwing/`. Ask the binary, don't scrape the repo:")
	fmt.Println()
	fmt.Println("- `sparkwing info --json` -- context: binary, project, next steps (start here)")
	fmt.Println("- `sparkwing commands` -- full CLI surface as JSON (every verb + every flag)")
	fmt.Println("- `sparkwing pipeline list --json` -- this repo's pipelines")
	fmt.Println("- `sparkwing run <name>` -- run a pipeline (`wing <name>` is a human alias; agents prefer `sparkwing run`)")
	fmt.Println("- `sparkwing docs read --topic <slug>` -- offline docs; full corpus: https://sparkwing.dev/llms-full.txt")
}

// printFirstTimeCard prints the post-install onboarding card. Called
// from install.sh immediately after the binary lands, and re-runnable
// via `sparkwing info --first-time`. Static content -- the user just
// installed and almost certainly has no .sparkwing/ project yet, so
// project introspection would mislead. ANSI codes auto-disable on
// non-TTY (pkg/color), so the output reads cleanly when piped or
// captured to scrollback.
//
// Layout convention: section headers are bold; commands (the things
// users will copy/run) are cyan; comments (the "why" that follows
// `#`) are dim. Mirrors printInfoTable's palette so the two cards
// feel like the same surface.
func printFirstTimeCard() {
	tip := func(cmd, pad, note string) string {
		return color.Cyan(cmd) + pad + color.Dim("# "+note)
	}

	fmt.Println(color.Bold("Welcome to sparkwing!"))
	fmt.Println()
	fmt.Println(color.Bold("Next steps:"))
	fmt.Println("  1. cd into a code repo")
	fmt.Println("  2. " + tip("sparkwing pipeline new --name release", "      ", "bootstrap .sparkwing/ + a minimal pipeline"))
	fmt.Println("  3. " + tip("sparkwing run release", "                      ", "run it - first time downloads dependencies"))
	fmt.Println("  4. " + tip("sparkwing docs read --topic sdk", "            ", "or https://sparkwing.dev/sdk"))
	fmt.Println()
	fmt.Println("  For a build/test/deploy DAG instead:")
	fmt.Println("    " + color.Cyan("sparkwing pipeline new --name release --template build-test-deploy"))
	fmt.Println()
	fmt.Println(color.Bold("Tips:"))
	fmt.Println("  - " + tip("wing <pipeline>", "                            ", "human shortcut for 'sparkwing run <pipeline>'"))
	fmt.Println("  - " + tip("sparkwing dashboard start", "                  ", "run the dashboard locally to see runs"))
	fmt.Println("  - " + tip("sparkwing info", "                             ", "surveys the current repo + suggests next commands"))
	fmt.Println("  - " + tip("sparkwing info --first-time", "                ", "to see this message again"))
	cmpCmd, cmpNote := firstTimeCompletionHint()
	fmt.Println("  - " + tip(cmpCmd, "   ", cmpNote))
}

// firstTimeCompletionHint returns the shell-specific tab-complete
// one-liner split into command + note so the caller can color the
// two parts independently. Mirrors install.sh's old $SHELL detection
// so the hint matches whatever rc file the user actually loads.
// Unknown shells get the generic `completion --help` pointer rather
// than a guess that would land in the wrong file.
func firstTimeCompletionHint() (cmd, note string) {
	base := os.Getenv("SHELL")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "bash":
		return "echo 'source <(sparkwing completion --shell bash)' >> ~/.bashrc", "enable cli tab auto completion"
	case "zsh":
		return "echo 'source <(sparkwing completion --shell zsh)' >> ~/.zshrc", "enable cli tab auto completion"
	case "fish":
		return "sparkwing completion --shell fish > ~/.config/fish/completions/sparkwing.fish", "enable cli tab auto completion"
	default:
		return "sparkwing completion --help", "enable cli tab auto completion (bash | zsh | fish)"
	}
}

// gatherInfo never errors: every field has a sensible "not found"
// fallback so the command is always informative even outside a
// sparkwing project, on a machine without Go, or built from source.
// An agent reading the JSON should treat all fields as best-effort.
//
// agentMode tilts NextSteps toward discovery (docs, commands,
// list --json) instead of operational steps. Set true when the
// caller is JSON-piping or passed --for-agent.
func gatherInfo(agentMode bool) Info {
	binary, _ := os.Executable()
	info := Info{
		About:   infoAbout,
		Version: parseInfoVersion(installedVersion()),
		Binary:  binary,
		Docs: InfoDocs{
			CLI:      "sparkwing docs list / read --topic <slug> / all",
			Web:      "https://sparkwing.dev/docs/",
			LLMsFull: "https://sparkwing.dev/llms-full.txt",
			LLMsTXT:  "https://sparkwing.dev/llms.txt",
		},
		ForAgents:    infoForAgents,
		FirstRunNote: infoFirstRunNote,
		Toolchain: InfoToolchain{
			Go: InfoGoToolchain{Required: infoGoRequirement},
		},
	}

	if goVer := goToolchainVersion(); goVer != "" {
		info.Toolchain.Go.Found = true
		info.Toolchain.Go.Version = goVer
	}

	cwd, err := os.Getwd()
	if err == nil {
		if sparkwingDir, ok := walkUpForSparkwing(cwd); ok {
			info.Project.Found = true
			info.Project.SparkwingDir = sparkwingDir
			// Pipeline enumeration is best-effort. A repo with a
			// broken pipelines.yaml still gets a useful info card.
			if pipelineList, perr := gatherPipelinesCatalog(false); perr == nil {
				info.Project.Pipelines = summarizePipelines(pipelineList)
			}
		}
	}
	if !info.Project.Found {
		info.Project.HowToScaffold = "sparkwing pipeline new --name release"
	}

	info.NextSteps = nextStepsFor(info, agentMode)
	info.Tips = gatherTips(info)
	return info
}

// gatherTips runs each tip gate and returns the applicable ones.
// Each gate must be cheap and fail-soft: no blocking shell-outs, no
// hard errors. Network probes (release-behind) get a short timeout
// and silently skip on failure -- an offline laptop sees the rest of
// the card without latency or noise.
func gatherTips(info Info) []InfoTip {
	var tips []InfoTip

	if t, ok := tipTabComplete(); ok {
		tips = append(tips, t)
	}
	if t, ok := tipDashboardNotRunning(); ok {
		tips = append(tips, t)
	}
	if t, ok := tipAgentBlockMissing(info); ok {
		tips = append(tips, t)
	}
	if t, ok := tipCLIBehindLatest(info); ok {
		tips = append(tips, t)
	}

	return tips
}

// tipTabComplete suggests a one-liner sourcing completion in the
// user's shell rc. Gate: $SHELL is bash/zsh/fish AND none of the
// shell's plausible init files contains a "sparkwing completion"
// reference. Earlier versions only checked the canonical rc
// (~/.bashrc / ~/.zshrc); users who source completion from
// .zprofile, .zshenv, or any *.zsh* in $HOME got a false-positive
// "not set up" tip. Now we scan a broader set so the tip only
// fires when completion is genuinely missing.
func tipTabComplete() (InfoTip, bool) {
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	base := shell
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	home, _ := os.UserHomeDir()

	switch base {
	case "bash":
		if completionConfigured(bashInitCandidates(home)) {
			return InfoTip{}, false
		}
		return InfoTip{
			ID:      "tab-complete",
			Title:   "Tab-complete is not set up",
			Command: "echo 'source <(sparkwing completion --shell bash)' >> ~/.bashrc",
		}, true
	case "zsh":
		if completionConfigured(zshInitCandidates(home)) {
			return InfoTip{}, false
		}
		return InfoTip{
			ID:      "tab-complete",
			Title:   "Tab-complete is not set up",
			Command: "echo 'source <(sparkwing completion --shell zsh)' >> ~/.zshrc",
		}, true
	case "fish":
		rc := home + "/.config/fish/completions/sparkwing.fish"
		if _, err := os.Stat(rc); err == nil {
			return InfoTip{}, false
		}
		return InfoTip{
			ID:      "tab-complete",
			Title:   "Tab-complete is not set up",
			Command: "sparkwing completion --shell fish > ~/.config/fish/completions/sparkwing.fish",
		}, true
	}
	// Unknown shell -- skip rather than guess wrong.
	return InfoTip{}, false
}

// bashInitCandidates returns the bash files that conventionally
// source rc-style customization. Login + non-login both covered;
// unusual layouts like .bashrc.local are scanned too so a user
// who stashed `source <(sparkwing completion ...)` there isn't
// falsely told tab-complete is missing.
func bashInitCandidates(home string) []string {
	return []string{
		home + "/.bashrc",
		home + "/.bash_profile",
		home + "/.profile",
		home + "/.bashrc.local",
	}
}

// zshInitCandidates is the zsh equivalent. Order doesn't matter;
// completionConfigured returns true on the first hit. .zshrc_profile
// and .zshrc.local are non-standard but seen in the wild.
func zshInitCandidates(home string) []string {
	return []string{
		home + "/.zshrc",
		home + "/.zprofile",
		home + "/.zshenv",
		home + "/.zlogin",
		home + "/.zshrc.local",
		home + "/.zshrc_profile",
	}
}

// completionConfigured reports whether any of the candidate files
// contains a "sparkwing completion" line. Unreadable / missing
// files don't count toward "configured" -- only a positive hit
// suppresses the tip. Scans up to a few KB per file, which is
// trivial for shell rc sizes.
func completionConfigured(candidates []string) bool {
	for _, path := range candidates {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(body), "sparkwing completion") {
			return true
		}
	}
	return false
}

// tipDashboardNotRunning fires when no live dashboard PID is found.
// Cheap: just reads $SPARKWING_HOME/dashboard.pid and probes the PID.
func tipDashboardNotRunning() (InfoTip, bool) {
	dp, err := resolveDashboardPaths("")
	if err != nil {
		return InfoTip{}, false
	}
	if _, alive := readLivePID(dp.pid); alive {
		return InfoTip{}, false
	}
	return InfoTip{
		ID:      "dashboard",
		Title:   "Local dashboard is not running",
		Command: "sparkwing dashboard start",
		Note:    "runs at http://127.0.0.1:4343",
	}, true
}

// tipAgentBlockMissing fires only inside a sparkwing project, when
// neither CLAUDE.md nor AGENTS.md exists at the project root. The
// suggestion is to run `sparkwing info --for-agent` and paste the
// output into one of those files so AI tools have working context.
func tipAgentBlockMissing(info Info) (InfoTip, bool) {
	if !info.Project.Found {
		return InfoTip{}, false
	}
	root := info.Project.SparkwingDir
	// SparkwingDir points at .sparkwing/; the agent files live one up.
	if i := strings.LastIndex(root, "/.sparkwing"); i >= 0 {
		root = root[:i]
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if _, err := os.Stat(root + "/" + name); err == nil {
			return InfoTip{}, false
		}
	}
	return InfoTip{
		ID:      "agent-block",
		Title:   "No CLAUDE.md / AGENTS.md in this repo",
		Command: "sparkwing info --for-agent",
		Note:    "paste the output into CLAUDE.md so AI tools have sparkwing context",
	}, true
}

// tipCLIBehindLatest probes sparkwing.dev/releases/latest with a
// short timeout. Fails silently when offline (the gate's negative).
// Skipped for non-release builds (devel / dirty) since "behind" is
// meaningless for a local build.
func tipCLIBehindLatest(info Info) (InfoTip, bool) {
	if !info.Version.IsRelease || info.Version.Semver == "" {
		return InfoTip{}, false
	}
	latest, err := fetchLatestRelease()
	if err != nil || latest == "" {
		return InfoTip{}, false
	}
	if !isSemver(info.Version.Semver) || !isSemver(latest) {
		return InfoTip{}, false
	}
	if !semverBehind(info.Version.Semver, latest) {
		return InfoTip{}, false
	}
	return InfoTip{
		ID:      "cli-behind",
		Title:   "A newer sparkwing release is available",
		Command: "sparkwing version update --cli",
		Note:    "installed " + info.Version.Semver + " → latest " + latest,
	}, true
}

// semverBehind returns true when current < latest under semver
// ordering. Wrapper kept tiny so callers read clearly; reaches
// straight into x/mod/semver via the import below.
func semverBehind(current, latest string) bool {
	return semver.Compare(current, latest) < 0
}

// goToolchainVersion shells out to `go version`. Returns the bare
// version token (e.g. "go1.25.0") or "" if Go isn't on PATH. We don't
// bake-in the build-time toolchain version because the user-facing
// answer is "what compiler will run when sparkwing tries to compile
// my .sparkwing/?" -- that's the version on PATH right now, not the
// one that built the CLI.
func goToolchainVersion() string {
	bin, err := exec.LookPath("go")
	if err != nil {
		return ""
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		return ""
	}
	// `go version` prints e.g. "go version go1.25.0 darwin/arm64".
	// The second token is the one users recognize.
	fields := strings.Fields(string(out))
	if len(fields) >= 3 {
		return fields[2]
	}
	return strings.TrimSpace(string(out))
}

func summarizePipelines(list []Pipeline) InfoPipelinesSum {
	out := InfoPipelinesSum{Total: len(list)}
	groupSet := map[string]struct{}{}
	for _, p := range list {
		if len(p.Triggers) > 0 {
			out.Triggered++
		} else {
			out.Manual++
		}
		if p.Group != "" {
			groupSet[p.Group] = struct{}{}
		}
	}
	for g := range groupSet {
		out.Groups = append(out.Groups, g)
	}
	sort.Strings(out.Groups)
	return out
}

// nextStepsFor curates the recommended next commands. The ordering is
// the priority order: an agent piping `sparkwing info -o plain | head
// -n1` should land on the single most useful command for the current
// state.
//
// Two modes:
//
//   - agent first-run (agentMode=true OR project not found): leads
//     with discovery (docs, commands, list --json) so an agent that
//     just got handed the binary can self-onboard in three calls.
//   - human / project (agentMode=false AND project found): leads
//     with operational verbs (list, describe, run) since the agent's
//     already onboarded by the time it's running info inside a
//     repo.
func nextStepsFor(info Info, agentMode bool) []InfoNextStep {
	if !info.Project.Found {
		// No .sparkwing/ here. Most likely a fresh install -- point
		// at --first-time for the full numbered onboarding (welcome,
		// tips, completion hint, edit-the-stub step) and show only
		// the canonical fast-path commands inline. Avoids
		// re-listing --first-time's whole step set in two places.
		// The build/test/deploy template alt is in --first-time.
		// `commands` is in For agents.
		return []InfoNextStep{
			{Command: "sparkwing info --first-time", Purpose: "post-install onboarding card: full numbered scaffold steps + tips"},
			{Command: "sparkwing pipeline new --name release", Purpose: "auto-bootstrap .sparkwing/ + scaffold a single-node pipeline"},
			{Command: "sparkwing run release", Purpose: "run the scaffolded pipeline"},
		}
	}
	// In a project. Operational verbs only -- machine-readable
	// surfaces (pipeline list --json, commands, etc.) live in the
	// For agents section, returned alongside next_steps in the JSON
	// shape so agents can branch on them directly. agentMode no
	// longer carves a separate discovery list.
	_ = agentMode
	return []InfoNextStep{
		{Command: "sparkwing pipeline list", Purpose: "see every pipeline this repo defines"},
		{Command: "sparkwing pipeline describe --name <name>", Purpose: "full metadata for one pipeline"},
		{Command: "sparkwing run <name>", Purpose: "run a pipeline (humans: `wing <name>` is the same thing)"},
		{Command: "sparkwing pipeline new --name <name>", Purpose: "scaffold a new pipeline"},
		{Command: "sparkwing docs list", Purpose: "browse embedded docs (offline)"},
		{Command: "sparkwing dashboard start", Purpose: "start the local dashboard at http://127.0.0.1:4343"},
	}
}

// infoForAgents lists the machine-readable surfaces an agent reaches
// for. Static -- it's reference content, not state-derived. Rendered
// as its own section in the table card and emitted on the Info struct
// so JSON consumers see it as `for_agents`. Replaces the prior
// "Other modes" block (which mixed agent surfaces with the human
// `--first-time` onboarding card; `--first-time` is now surfaced
// only in no-project Next steps, where it actually helps).
var infoForAgents = []InfoNextStep{
	{Command: "sparkwing commands", Purpose: "full CLI surface as JSON (every verb + every flag)"},
	{Command: "sparkwing info --json", Purpose: "machine-readable copy of this card (alias: -o json)"},
	{Command: "sparkwing info --for-agent", Purpose: "paste-ready block for CLAUDE.md / AGENTS.md"},
	{Command: "sparkwing pipeline list --json", Purpose: "this repo's pipelines as JSON"},
	{Command: "sparkwing <verb> --help --json", Purpose: "any verb's spec as JSON"},
}

func printInfoTable(info Info) {
	v := info.Version
	// Single Environment section consolidates what used to be
	// three header blocks (version triplet, Project, Toolchain).
	// They were each 1-3 lines with sub-headers, which felt like
	// page furniture for not much content. Bold section headers +
	// cyan commands + dim asides match the first-time card palette
	// so the two surfaces feel like the same UI. ANSI codes
	// auto-disable on non-TTY (pkg/color).
	const lblW = 9 // widest label below ("sparkwing")
	row := func(label, value, dim string) {
		if dim != "" {
			fmt.Printf("  %-*s  %s %s\n", lblW, label, value, color.Dim(dim))
		} else {
			fmt.Printf("  %-*s  %s\n", lblW, label, value)
		}
	}

	if info.About != "" {
		if color.Enabled() {
			fmt.Println(batsay(info.About, 44))
			fmt.Println()
		} else {
			fmt.Println(color.Bold("About"))
			fmt.Println("  " + info.About)
			fmt.Println()
		}
	}

	fmt.Println(color.Bold("Environment"))

	buildLabel := v.BuildType
	if v.HumanLabel != "" {
		buildLabel = v.BuildType + " — " + v.HumanLabel
	}
	row("sparkwing", v.Installed, "("+buildLabel+")")

	if info.Binary != "" {
		row("binary", info.Binary, "")
	}

	// Go is the user's local toolchain, used to compile .sparkwing/
	// on first run. Make that explicit -- the prior "(required for
	// Go-pipeline path)" was ambiguous about whether it meant a
	// per-project pin or a per-machine requirement.
	if info.Toolchain.Go.Found {
		row("go", info.Toolchain.Go.Version, "(your local toolchain — used to compile .sparkwing/)")
	} else {
		row("go", color.Dim("not installed"), "(your local toolchain — needed to compile .sparkwing/)")
		fmt.Printf("  %-*s  %s\n", lblW, "", color.Cyan(goInstallHintForce()))
	}

	if info.Project.Found {
		p := info.Project.Pipelines
		noun := "pipelines"
		if p.Total == 1 {
			noun = "pipeline"
		}
		row("project", ".sparkwing/ at "+info.Project.SparkwingDir, fmt.Sprintf("(%d %s: %d triggered, %d manual)", p.Total, noun, p.Triggered, p.Manual))
		if len(p.Groups) > 0 {
			row("groups", strings.Join(p.Groups, ", "), "")
		}
	} else {
		row("project", color.Dim("no .sparkwing/ in this directory or any parent"), "")
	}
	fmt.Println()

	fmt.Println(color.Bold("Next steps"))
	printAlignedSteps(info.NextSteps)
	fmt.Println()

	if len(info.ForAgents) > 0 {
		fmt.Println(color.Bold("For agents"))
		printAlignedSteps(info.ForAgents)
		fmt.Println()
	}

	if len(info.Tips) > 0 {
		fmt.Println(color.Bold("Tips"))
		for _, t := range info.Tips {
			fmt.Printf("  %s %s\n", color.Yellow("•"), t.Title)
			if t.Command != "" {
				fmt.Printf("      %s\n", color.Cyan(t.Command))
			}
			if t.Note != "" {
				fmt.Printf("      %s\n", color.Dim(t.Note))
			}
		}
		fmt.Println()
	}

	fmt.Println(color.Bold("Docs"))
	fmt.Printf("  cli:        %s %s\n", color.Cyan(info.Docs.CLI), color.Dim("(offline, version-locked)"))
	fmt.Printf("  web:        %s\n", color.Cyan(info.Docs.Web))
	fmt.Printf("  llms-full:  %s %s\n", color.Cyan(info.Docs.LLMsFull), color.Dim("(full corpus, one fetch)"))
	fmt.Printf("  llms.txt:   %s %s\n", color.Cyan(info.Docs.LLMsTXT), color.Dim("(short index)"))
}

// batsay returns a cowsay-style speech bubble containing msg with
// the sparkwing bat below it. Width is the inside width of the
// bubble; long messages are word-wrapped to fit. The bubble's tail
// ("\" then "\") hangs from the bottom of the bubble down toward
// the bat's left wing so the two pieces read as one figure.
func batsay(msg string, width int) string {
	lines := wrapLinesAt(msg, width)
	if len(lines) == 0 {
		lines = []string{""}
	}
	var b strings.Builder
	b.WriteString(" " + strings.Repeat("_", width+2) + "\n")
	for i, line := range lines {
		left, right := "|", "|"
		switch {
		case len(lines) == 1:
			left, right = "<", ">"
		case i == 0:
			left, right = "/", "\\"
		case i == len(lines)-1:
			left, right = "\\", "/"
		}
		fmt.Fprintf(&b, "%s %-*s %s\n", left, width, line, right)
	}
	b.WriteString(" " + strings.Repeat("-", width+2) + "\n")
	b.WriteString("    \\\n")
	b.WriteString("     \\\n")
	b.WriteString(infoBat)
	return b.String()
}

// wrapLinesAt word-wraps s into lines no wider than width. A single
// word longer than width gets its own line (we don't break mid-word).
func wrapLinesAt(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out = append(out, line)
			line = w
			continue
		}
		line += " " + w
	}
	out = append(out, line)
	return out
}

// printAlignedSteps writes a two-column "command   purpose" block
// with column 1 padded to the longest command. Padding uses the
// pre-color string length; ANSI escapes wrap the printed value but
// don't shift visible alignment because every command in the column
// gets the same wrapping.
func printAlignedSteps(steps []InfoNextStep) {
	width := 0
	for _, ns := range steps {
		if n := len(ns.Command); n > width {
			width = n
		}
	}
	for _, ns := range steps {
		pad := strings.Repeat(" ", width-len(ns.Command))
		fmt.Printf("  %s%s  %s\n", color.Cyan(ns.Command), pad, color.Dim(ns.Purpose))
	}
}
