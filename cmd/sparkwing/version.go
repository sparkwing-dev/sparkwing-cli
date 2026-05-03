// `sparkwing version` is the kubectl-style version surface: one
// command shows the CLI version, the latest published release
// (network-checked, ~3s timeout), the SDK pin in the current
// repo's .sparkwing/go.mod, and any sparks libraries declared
// alongside it.
//
// Designed to be both human-readable (table) and agent-readable
// (--json with structured "behind" booleans). The network fetch
// degrades gracefully: timeout / offline / DNS failure produces
// a non-empty `latest_fetch_error` field rather than a hard
// command failure -- the local pieces of the report are still
// useful when latest is unknown.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	flag "github.com/spf13/pflag"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/sparkwing-dev/sparkwing-cli/pkg/color"
)

// VersionReport is the JSON shape of `sparkwing version --json`.
type VersionReport struct {
	CLI            InfoVersion     `json:"cli"`
	LatestRelease  string          `json:"latest_release,omitempty"`
	LatestFetchErr string          `json:"latest_fetch_error,omitempty"`
	Behind         bool            `json:"behind"`
	Project        *VersionProject `json:"project,omitempty"`
}

// VersionProject collects per-repo version info: the SDK pin in
// .sparkwing/go.mod and any sparks library pins. Nil when the
// command is run outside a sparkwing project.
//
// SDKReplace is set when go.mod has a `replace
// github.com/koreyGambill/sparkwing => ...` directive (the
// sparkwing pipeline's own .sparkwing/ does this; some consumers do
// it temporarily for local development). When present, the
// SDKPin is the placeholder "v0.0.0" and the "behind" check
// against the pin is meaningless, so we report the replace
// target instead and skip the behind comparison.
type VersionProject struct {
	SparkwingDir string     `json:"sparkwing_dir"`
	SDKPin       string     `json:"sdk_pin,omitempty"`
	SDKReplace   string     `json:"sdk_replace,omitempty"`
	SDKBehind    bool       `json:"sdk_behind"`
	Sparks       []SparkPin `json:"sparks,omitempty"`
}

// SparkPin is one koreyGambill/sparks-* module declared in the
// project's go.mod (and presumably its sparks.yaml).
type SparkPin struct {
	Module  string `json:"module"`
	Version string `json:"version"`
}

// versionLatestURL is the public pointer file the install.sh
// download flow uses. Plain text, single line, no caching beyond
// the no-cache headers we set on upload.
const versionLatestURL = "https://sparkwing.dev/releases/latest"

// versionFetchTimeout caps the latest-release lookup. Short enough
// that an offline laptop still gets a useful command in seconds,
// long enough that a slow link succeeds.
const versionFetchTimeout = 3 * time.Second

// sdkModulePath is the canonical Go module path for the sparkwing
// SDK. Used both for the SDK-pin lookup and for distinguishing the
// SDK from sparks-* sibling modules.
const sdkModulePath = "github.com/sparkwing-dev/sparkwing-sdk"

func runVersion(args []string) error {
	// `sparkwing version update ...` is the unified updater verb. Any
	// other arg is treated as flags for the show-version path; bare
	// `sparkwing version` prints the composite report.
	if len(args) > 0 && args[0] == "update" {
		return runVersionUpdate(args[1:])
	}
	fs := flag.NewFlagSet(cmdVersion.Path, flag.ContinueOnError)
	var output string
	fs.StringVarP(&output, "output", "o", "table", "table | json | plain")
	asJSON := fs.Bool("json", false, "alias for --output json")
	offline := fs.Bool("offline", false, "skip the network fetch for latest release")
	if err := parseAndCheck(cmdVersion, fs, args); err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("version: unexpected positional %q", fs.Arg(0))
	}
	if *asJSON {
		output = "json"
	}

	report := gatherVersionReport(*offline)

	switch strings.ToLower(output) {
	case "json", "":
		if output == "" {
			output = "table"
		}
		if output == "json" {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(report)
		}
	case "plain":
		// One semver per line. Useful as `$(sparkwing version -o
		// plain | head -n1)` to grab the CLI version in scripts.
		fmt.Println(report.CLI.Installed)
		if report.LatestRelease != "" {
			fmt.Println(report.LatestRelease)
		}
		return nil
	}
	printVersionTable(report)
	return nil
}

func gatherVersionReport(offline bool) VersionReport {
	r := VersionReport{
		CLI: parseInfoVersion(installedVersion()),
	}
	if !offline {
		latest, err := fetchLatestRelease()
		if err != nil {
			r.LatestFetchErr = err.Error()
		} else {
			r.LatestRelease = latest
		}
	}
	if r.CLI.Semver != "" && r.LatestRelease != "" {
		r.Behind = semver.Compare(r.CLI.Semver, r.LatestRelease) < 0
	}
	if proj := gatherVersionProject(r.LatestRelease); proj != nil {
		r.Project = proj
	}
	return r
}

// gatherVersionProject walks up from cwd to find .sparkwing/, then
// parses .sparkwing/go.mod for the SDK pin and any sparks-* pins.
// Returns nil when no .sparkwing/ is found so the report's Project
// field stays absent in JSON.
func gatherVersionProject(latest string) *VersionProject {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	sparkwingDir, ok := walkUpForSparkwing(cwd)
	if !ok {
		return nil
	}
	gomodPath := filepath.Join(sparkwingDir, "go.mod")
	body, err := os.ReadFile(gomodPath)
	if err != nil {
		return &VersionProject{SparkwingDir: sparkwingDir}
	}
	mf, err := modfile.Parse(gomodPath, body, nil)
	if err != nil {
		return &VersionProject{SparkwingDir: sparkwingDir}
	}

	proj := &VersionProject{SparkwingDir: sparkwingDir}
	// Pass 1: collect requires.
	for _, req := range mf.Require {
		mod := req.Mod.Path
		ver := req.Mod.Version
		switch {
		case mod == sdkModulePath:
			proj.SDKPin = ver
		case strings.HasPrefix(mod, "github.com/koreyGambill/sparks-"):
			proj.Sparks = append(proj.Sparks, SparkPin{Module: mod, Version: ver})
		}
	}
	// Pass 2: replace directives. A replace on the SDK means the
	// require version is meaningless ("v0.0.0" or similar); record
	// the replace target so the user sees the truth instead of a
	// fake "behind" indicator.
	for _, rep := range mf.Replace {
		if rep.Old.Path != sdkModulePath {
			continue
		}
		target := rep.New.Path
		if rep.New.Version != "" {
			target = target + "@" + rep.New.Version
		}
		proj.SDKReplace = target
		// Don't flag behind when there's a replace; the pin is
		// not what's being compiled.
		break
	}
	if proj.SDKReplace == "" && proj.SDKPin != "" && latest != "" && isSemver(proj.SDKPin) {
		proj.SDKBehind = semver.Compare(proj.SDKPin, latest) < 0
	}
	sort.Slice(proj.Sparks, func(i, j int) bool { return proj.Sparks[i].Module < proj.Sparks[j].Module })
	return proj
}

// fetchLatestRelease pulls https://sparkwing.dev/releases/latest --
// the same single-line pointer install.sh consults -- and returns
// the trimmed contents. Bounded by versionFetchTimeout so an
// offline laptop falls back to "(unknown)" quickly.
func fetchLatestRelease() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionLatestURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, versionLatestURL)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(body))
	if !isSemver(v) {
		return "", fmt.Errorf("malformed version from %s: %q", versionLatestURL, v)
	}
	return v, nil
}

// isSemver returns true for vX.Y.Z (allowing pre-release/build
// suffixes per golang.org/x/mod/semver). Used to gate compare
// calls so a malformed string doesn't poison the "behind" boolean.
func isSemver(v string) bool {
	return semver.IsValid(v)
}

func printVersionTable(r VersionReport) {
	cv := r.CLI
	fmt.Printf("CLI\n")
	fmt.Printf("  version:  %s\n", cv.Installed)
	fmt.Printf("  build:    %s", cv.BuildType)
	if cv.HumanLabel != "" {
		fmt.Printf(" %s", color.Dim("("+cv.HumanLabel+")"))
	}
	fmt.Println()
	switch {
	case r.LatestFetchErr != "":
		fmt.Printf("  latest:   %s %s\n", color.Dim("unknown"), color.Dim("("+r.LatestFetchErr+")"))
	case r.LatestRelease == "":
		fmt.Printf("  latest:   %s\n", color.Dim("not checked (--offline)"))
	case r.Behind:
		fmt.Printf("  latest:   %s %s\n",
			r.LatestRelease,
			color.Yellow("behind"),
		)
	default:
		fmt.Printf("  latest:   %s %s\n", r.LatestRelease, color.Green("up to date"))
	}
	// Always surface the upgrade command so it's discoverable. Loud
	// when behind (default color, suggests action); dim when up-to-date
	// (still readable, doesn't shout). A reader who sees "v0.45.6 is
	// out, I'm on v0.45.3" can run the printed command without
	// hunting through docs.
	if r.Behind {
		fmt.Printf("  upgrade:  %s\n", "sparkwing version update --cli")
	} else {
		fmt.Printf("  upgrade:  %s\n", color.Dim("sparkwing version update --cli"))
	}
	fmt.Printf("  changelog: https://sparkwing.dev/CHANGELOG.md\n")
	fmt.Println()

	if r.Project == nil {
		fmt.Println(color.Dim("(no .sparkwing/ in this directory or any parent)"))
		return
	}

	p := r.Project
	if p.SDKPin == "" && p.SDKReplace == "" && len(p.Sparks) == 0 {
		fmt.Printf("Project at %s\n", p.SparkwingDir)
		fmt.Println(color.Dim("  (no go.mod pins resolved)"))
		return
	}
	fmt.Printf("Project at %s\n", p.SparkwingDir)
	switch {
	case p.SDKReplace != "":
		// The require version is a placeholder; what's actually
		// compiling is the replace target. Surface that and skip
		// the behind check entirely.
		fmt.Printf("  sdk:      %s\n", color.Dim("(replaced with "+p.SDKReplace+")"))
	case p.SDKPin != "":
		label := color.Green("up to date")
		switch {
		case r.LatestRelease == "":
			label = color.Dim("(latest unknown)")
		case p.SDKBehind:
			label = color.Yellow("behind") + " " + color.Dim("(latest "+r.LatestRelease+")")
		}
		fmt.Printf("  sdk:      %s   %s\n", p.SDKPin, label)
		if p.SDKBehind {
			fmt.Printf("  upgrade:  sparkwing version update --sdk\n")
		}
	}
	if len(p.Sparks) > 0 {
		fmt.Println("  sparks:")
		w := 0
		for _, s := range p.Sparks {
			if n := len(s.Module); n > w {
				w = n
			}
		}
		for _, s := range p.Sparks {
			fmt.Printf("    %-*s  %s\n", w, s.Module, s.Version)
		}
	}
}
