// Hand-rolled help + flag-validation framework. Sparkwing doesn't
// use cobra, so this file plays cobra's role: every leaf command
// declares its shape as a Command value in help_registry.go, its
// handler calls parseAndCheck to parse flags + validate
// dependencies, and -h/--help anywhere in the arg list prints the
// standardized help page.
//
// Goals:
//   - Uniform help output: SYNOPSIS, USAGE, DESCRIPTION, ARGUMENTS,
//     grouped FLAGS sections, EXAMPLES.
//   - Every flag carries [required] / [optional] / [required when X]
//     annotations so operators can scan quickly.
//   - Flag dependencies declared once (RequiresFlags, ConflictsWith)
//     both render in help AND produce readable error messages like
//     "--run was set but --older-than must not be used with it".
//   - Flag groups control rendering order (Input / Filter / Output /
//     System / Other) so related knobs cluster together and --help
//     lands at the bottom in Other.
//
// Handlers still own the pflag.FlagSet (so they pick the right typed
// destination -- string, bool, duration, slice). cmd.Flags mirrors
// the registration for help/validation purposes; the two must agree
// in name + semantics, and keeping them side-by-side in the handler
// file keeps drift cheap to spot in review.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	flag "github.com/spf13/pflag"
)

// FlagType is the typed-binding hint that bindFlags reads to decide
// which pflag registration to call. Specs left at the zero value
// ("") are treated as untyped: bindFlags ignores them so the
// handler can still register manually with pflag (the migration
// path while ports happen incrementally).
type FlagType string

const (
	FlagString      FlagType = "string"
	FlagBool        FlagType = "bool"
	FlagInt         FlagType = "int"
	FlagInt64       FlagType = "int64"
	FlagDuration    FlagType = "duration"
	FlagStringSlice FlagType = "stringSlice"
)

// FlagSpec is the metadata record for one CLI flag. It is NOT the
// pflag registration; the handler still declares the typed pflag
// destination. FlagSpec feeds: help rendering, the dependency
// checker, and (via help_registry.go) the shell-completion script.
type FlagSpec struct {
	Name     string // long name without leading dashes, e.g. "on"
	Short    string // single char without leading dash, "" if none
	Argument string // "<name>" for value-taking flags, "" for booleans
	Desc     string // one-line description
	Group    string // "Input", "Filter", "Output", "System", "Other"

	// Required = always required. Error on absence: "--X is required".
	Required bool

	// RequiredWhen renders as "[required when <text>]" in the help
	// flags column. Use for conditional requirements that can't be
	// expressed by RequiresFlags alone ("required when targeting
	// prod", "required unless --run is set").
	RequiredWhen string

	// RequiresFlags lists flags that must also be set IF this one is.
	// Validation emits: "--X was set but --Y is required with it".
	RequiresFlags []string

	// ConflictsWith lists flags that must NOT be set if this one is.
	// Validation emits: "--X and --Y cannot be used together".
	ConflictsWith []string

	// Default is the cosmetic "(default: foo)" string shown in help.
	// Use it when a typed Default would over-specify (e.g. "latest"
	// for a version flag whose true default is server-resolved). When
	// DefaultValue is set on a typed flag, this can be left empty;
	// the renderer derives a display string from DefaultValue.
	Default string

	// Type is the typed-binding hint. When set, bindFlags registers
	// this flag with pflag automatically. Leave "" to opt out (manual
	// pflag registration in the handler still works).
	Type FlagType

	// DefaultValue is the typed default bindFlags hands to pflag.
	// Must match Type:
	//   FlagString       -> string
	//   FlagBool         -> bool
	//   FlagInt          -> int
	//   FlagInt64        -> int64
	//   FlagDuration     -> time.Duration (or string parseable as one)
	//   FlagStringSlice  -> []string
	// Nil = the type's zero value.
	DefaultValue any

	// Hidden flags do not render in --help, do not appear in
	// tab-completion menus, but are still parsed and validated. Use
	// for deprecated-but-still-honored flags or internal escape
	// hatches.
	Hidden bool
}

// PosArg is one positional argument, shown in USAGE + rendered in
// the ARGUMENTS section with its own [required] / [optional] tag.
type PosArg struct {
	Name     string // "<pipeline>" or "<id>" -- include brackets
	Desc     string
	Required bool
}

// Example pairs a short description with a sample invocation.
// Examples render one per block, with the description as a `#`
// comment above the command.
type Example struct {
	Desc    string
	Command string
}

// SubcommandRef is used by parent commands (sparkwing, sparkwing
// tokens, etc.) to list their children in the COMMANDS section.
// To hide a subcommand from listings, set Hidden=true on its
// Command spec; PrintHelp / completion look it up by path and
// skip the corresponding ref.
type SubcommandRef struct {
	Name     string
	Synopsis string
}

// Command is the full spec for one node in the command tree. Leaf
// commands populate Flags / PosArgs / Examples; parent commands
// populate Subcommands.
type Command struct {
	Path        string // "sparkwing tokens create" (full invocation)
	Synopsis    string // one-line summary, shown at the top of help
	Description string // multi-paragraph explanation; optional

	// Subcommands -- parent-command section. Rendered as COMMANDS.
	Subcommands []SubcommandRef

	// Leaf-command sections.
	PosArgs     []PosArg
	Flags       []FlagSpec
	Examples    []Example
	GroupOrder  []string // FLAGS group rendering order; empty = default
	UsageSuffix string   // free-form extra tokens appended to USAGE line

	// Hidden commands are omitted from their parent's COMMANDS
	// listing and tab-complete verb menu, but remain dispatchable.
	// Use for deprecated aliases ("rm" for "delete"), internal
	// helpers, and one-off escape hatches.
	Hidden bool

	// HideFromComplete keeps the command visible in --help output
	// (so humans can re-discover it) but suppresses it from shell
	// completion. Use for verbs that read well in help docs but
	// would clutter tab-complete because nobody types them
	// repeatedly: `commands` (agent-only), `completion` (shell
	// install, run once).
	HideFromComplete bool
}

// defaultGroupOrder keeps --help pinned at the bottom via "Other",
// which all auto-injected --help specs land in.
var defaultGroupOrder = []string{"Input", "Filter", "Output", "System", "Other"}

// helpFlag is appended to every command's Flags when it isn't
// already declared, so every command supports -h / --help for free.
var helpFlag = FlagSpec{
	Name:  "help",
	Short: "h",
	Desc:  "Show help for this command and exit",
	Group: "Other",
}

// errHelpRequested is returned from parseAndCheck when the user
// passed -h or --help. Handlers bail early:
//
//	if err := parseAndCheck(cmd, fs, args); err != nil {
//	    if errors.Is(err, errHelpRequested) { return nil }
//	    return err
//	}
var errHelpRequested = errors.New("help requested")

// parseAndCheck wires the shared behavior onto a handler's pflag
// FlagSet:
//
//  1. Injects --help / -h (skipped if the handler already declared
//     one, e.g. a test stub).
//  2. Silences pflag's own usage banner -- our PrintHelp owns that.
//  3. Parses args; on parse error, renders our help to stderr then
//     wraps the error with the command path so operators see which
//     subcommand complained.
//  4. Intercepts --help: prints help to stdout and returns
//     errHelpRequested so the handler short-circuits.
//  5. Validates required / requires / conflicts from cmd.Flags.
func parseAndCheck(cmd Command, fs *flag.FlagSet, args []string) error {
	// pflag's ContinueOnError returns flag.ErrHelp on -h; our
	// FlagSets are built with that mode already, but if a caller
	// passes a different mode we still want to keep going.
	fs.SetOutput(io.Discard)

	// Inject --help unless the handler did it themselves.
	if fs.Lookup("help") == nil {
		fs.BoolP("help", "h", false, helpFlag.Desc)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			renderHelp(cmd, args, os.Stdout)
			return errHelpRequested
		}
		PrintHelp(cmd, os.Stderr)
		return fmt.Errorf("%s: %w", cmd.Path, err)
	}

	// Second path for --help: some invocations pass --help as the
	// bool flag rather than triggering pflag's built-in help handler.
	if v, err := fs.GetBool("help"); err == nil && v {
		renderHelp(cmd, args, os.Stdout)
		return errHelpRequested
	}

	return validateFlagDeps(cmd, fs)
}

// validateFlagDeps walks cmd.Flags and enforces Required /
// RequiresFlags / ConflictsWith against the parsed FlagSet. Error
// messages are written in a consistent voice so operators can
// recognize flag-dependency violations at a glance.
func validateFlagDeps(cmd Command, fs *flag.FlagSet) error {
	for _, spec := range cmd.Flags {
		if fs.Lookup(spec.Name) == nil {
			// Spec-only flag the handler didn't register. Skip
			// silently -- probably documentation drift -- rather
			// than nil-panic.
			continue
		}
		changed := fs.Changed(spec.Name)
		if spec.Required && !changed {
			return fmt.Errorf("%s: --%s is required", cmd.Path, spec.Name)
		}
		if !changed {
			continue
		}
		for _, req := range spec.RequiresFlags {
			if fs.Lookup(req) == nil || !fs.Changed(req) {
				return fmt.Errorf(
					"%s: --%s was set but --%s is required with it",
					cmd.Path, spec.Name, req)
			}
		}
		for _, c := range spec.ConflictsWith {
			if fs.Lookup(c) != nil && fs.Changed(c) {
				return fmt.Errorf(
					"%s: --%s and --%s cannot be used together",
					cmd.Path, spec.Name, c)
			}
		}
	}
	return nil
}

// PrintHelp renders the full help page for cmd to w. Layout:
//
//	<Synopsis>
//
//	USAGE
//	  <path> [<args>] [flags]
//
//	DESCRIPTION
//	  <Description>
//
//	COMMANDS         (for parent commands)
//	  name   synopsis
//
//	ARGUMENTS        (when PosArgs present)
//	  <pos>  [required]  desc
//
//	<GROUP>
//	  -x, --flag <arg>   [required]  desc (implies --foo) (vs --bar)
//
//	EXAMPLES
//	  # Explanation
//	  sparkwing <cmd> --flag value
func PrintHelp(cmd Command, w io.Writer) {
	if cmd.Synopsis != "" {
		fmt.Fprintln(w, cmd.Synopsis)
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "USAGE")
	fmt.Fprint(w, "  ", cmd.Path)
	for _, a := range cmd.PosArgs {
		if a.Required {
			fmt.Fprint(w, " ", a.Name)
		} else {
			fmt.Fprint(w, " [", a.Name, "]")
		}
	}
	if len(cmd.Subcommands) > 0 {
		fmt.Fprint(w, " <subcommand>")
	}
	if len(cmd.Flags) > 0 || len(cmd.Subcommands) == 0 {
		// Always show "[flags]" on leaves (even with 0 flags, --help
		// is injected); only suppress for pure parent commands that
		// declared no flags at all.
		if len(cmd.Flags) > 0 || len(cmd.Subcommands) == 0 {
			fmt.Fprint(w, " [flags]")
		}
	}
	if cmd.UsageSuffix != "" {
		fmt.Fprint(w, " ", cmd.UsageSuffix)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	if cmd.Description != "" {
		fmt.Fprintln(w, "DESCRIPTION")
		for _, line := range strings.Split(strings.TrimRight(cmd.Description, "\n"), "\n") {
			fmt.Fprint(w, "  ", line, "\n")
		}
		fmt.Fprintln(w)
	}

	if len(cmd.Subcommands) > 0 {
		visible := visibleSubcommands(cmd)
		if len(visible) > 0 {
			fmt.Fprintln(w, "COMMANDS")
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			for _, s := range visible {
				fmt.Fprint(tw, "  ", s.Name, "\t", s.Synopsis, "\n")
			}
			_ = tw.Flush()
			fmt.Fprintln(w)
		}
	}

	if len(cmd.PosArgs) > 0 {
		fmt.Fprintln(w, "ARGUMENTS")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, a := range cmd.PosArgs {
			tag := "[optional]"
			if a.Required {
				tag = "[required]"
			}
			fmt.Fprint(tw, "  ", a.Name, "\t", tag, "\t", a.Desc, "\n")
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}

	// Grouped FLAGS sections. Inject --help if the registry didn't
	// already declare it, so every command lists it at the bottom.
	// Skip Hidden flags so they remain functional without surfacing
	// in --help output.
	var flags []FlagSpec
	for _, f := range cmd.Flags {
		if f.Hidden {
			continue
		}
		flags = append(flags, f)
	}
	if !hasFlagNamed(flags, "help") {
		flags = append(flags, helpFlag)
	}
	groups := groupFlagsForHelp(flags, cmd.GroupOrder)
	for _, g := range groups {
		fmt.Fprintln(w, strings.ToUpper(g.name))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, f := range g.flags {
			fmt.Fprint(tw, "  ", formatFlagLHS(f), "\t", formatFlagTags(f), "\t", f.Desc, "\n")
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}

	if len(cmd.Examples) > 0 {
		fmt.Fprintln(w, "EXAMPLES")
		for i, ex := range cmd.Examples {
			if ex.Desc != "" {
				fmt.Fprint(w, "  # ", ex.Desc, "\n")
			}
			fmt.Fprint(w, "  ", ex.Command, "\n")
			if i < len(cmd.Examples)-1 {
				fmt.Fprintln(w)
			}
		}
		fmt.Fprintln(w)
	}
}

// formatFlagLHS renders the left side of a flag row: "-x, --name <arg>".
// Pads to reserve the "-X, " slot when only a long form exists so the
// column of `--names` stays aligned.
func formatFlagLHS(f FlagSpec) string {
	var b strings.Builder
	if f.Short != "" {
		b.WriteString("-")
		b.WriteString(f.Short)
		b.WriteString(", ")
	} else {
		b.WriteString("    ")
	}
	b.WriteString("--")
	b.WriteString(f.Name)
	if f.Argument != "" {
		b.WriteString(" ")
		b.WriteString(f.Argument)
	}
	return b.String()
}

// formatFlagTags composes the middle column: required / conditional
// annotation + dependency hints + default. Example outputs:
//
//	[required]
//	[optional]
//	[required when --run unset]
//	[required] (implies --type)
//	[optional] (vs --run) (default: 7d)
func formatFlagTags(f FlagSpec) string {
	var parts []string
	switch {
	case f.Required:
		parts = append(parts, "[required]")
	case f.RequiredWhen != "":
		parts = append(parts, "[required "+f.RequiredWhen+"]")
	default:
		parts = append(parts, "[optional]")
	}
	if len(f.RequiresFlags) > 0 {
		parts = append(parts, "(implies --"+strings.Join(f.RequiresFlags, ", --")+")")
	}
	if len(f.ConflictsWith) > 0 {
		parts = append(parts, "(vs --"+strings.Join(f.ConflictsWith, ", --")+")")
	}
	if f.Default != "" {
		parts = append(parts, "(default: "+f.Default+")")
	}
	return strings.Join(parts, " ")
}

type flagGroup struct {
	name  string
	flags []FlagSpec
}

// groupFlagsForHelp buckets flags by FlagSpec.Group and orders the
// resulting buckets per cmd.GroupOrder (or defaultGroupOrder when
// empty). Flags within a group are rendered in insertion order so
// registry authors can control prominence ("put --type before
// --principal").
//
// Groups present in the flag set but absent from GroupOrder land at
// the end in alphabetical order -- a mild nudge to keep groupings
// in the order list.
func groupFlagsForHelp(flags []FlagSpec, order []string) []flagGroup {
	if len(order) == 0 {
		order = defaultGroupOrder
	}

	byName := map[string][]FlagSpec{}
	var seenOrder []string
	for _, f := range flags {
		g := f.Group
		if g == "" {
			g = "Other"
		}
		if _, ok := byName[g]; !ok {
			seenOrder = append(seenOrder, g)
		}
		byName[g] = append(byName[g], f)
	}

	used := map[string]bool{}
	var out []flagGroup
	for _, name := range order {
		if flags, ok := byName[name]; ok {
			out = append(out, flagGroup{name: name, flags: flags})
			used[name] = true
		}
	}
	// Append groups not in the order list, alphabetically, so new
	// groupings surface rather than getting silently dropped.
	var leftovers []string
	for _, name := range seenOrder {
		if !used[name] {
			leftovers = append(leftovers, name)
		}
	}
	sort.Strings(leftovers)
	for _, name := range leftovers {
		out = append(out, flagGroup{name: name, flags: byName[name]})
	}
	return out
}

// visibleSubcommands filters out subcommand refs whose target
// Command has Hidden=true. Looks up each child via the argv-path
// key (parent path without the "sparkwing" prefix, joined with the
// subcommand name) in the leaf + parent registries.
func visibleSubcommands(parent Command) []SubcommandRef {
	return filterSubcommands(parent, false)
}

// completableSubcommands is visibleSubcommands plus a HideFromComplete
// filter -- used by tab-complete generators so verbs marked
// HideFromComplete still appear in --help (where humans look for
// re-discovery) but stay out of the noisy tab-complete menu.
func completableSubcommands(parent Command) []SubcommandRef {
	return filterSubcommands(parent, true)
}

func filterSubcommands(parent Command, dropHideFromComplete bool) []SubcommandRef {
	leaves := leafCommands()
	parents := parentCommands()
	pp := strings.TrimPrefix(parent.Path, "sparkwing")
	pp = strings.TrimPrefix(pp, " ")
	out := make([]SubcommandRef, 0, len(parent.Subcommands))
	for _, s := range parent.Subcommands {
		key := s.Name
		if pp != "" {
			key = pp + " " + s.Name
		}
		if c, ok := leaves[key]; ok {
			if c.Hidden {
				continue
			}
			if dropHideFromComplete && c.HideFromComplete {
				continue
			}
		}
		if c, ok := parents[key]; ok {
			if c.Hidden {
				continue
			}
			if dropHideFromComplete && c.HideFromComplete {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// hasFlagNamed returns true if any spec in flags has the given long
// name. Used to avoid double-appending the auto-injected --help.
func hasFlagNamed(flags []FlagSpec, name string) bool {
	for _, f := range flags {
		if f.Name == name {
			return true
		}
	}
	return false
}

// FlagValues holds typed pointers returned by bindFlags. Keys are
// flag long names. Use the typed accessors (String / Bool / Int /
// Duration / StringSlice) at the call site so handlers stay
// readable. A missing key panics -- this means the spec was
// declared but bindFlags didn't register it (programmer error,
// not user error).
type FlagValues map[string]any

// String returns the parsed string value for the named flag.
// Panics if the flag wasn't registered with FlagString.
func (v FlagValues) String(name string) string {
	p, ok := v[name].(*string)
	if !ok {
		panic(fmt.Sprintf("FlagValues.String: %q not bound as string", name))
	}
	return *p
}

// Bool returns the parsed bool value for the named flag.
// Panics if the flag wasn't registered with FlagBool.
func (v FlagValues) Bool(name string) bool {
	p, ok := v[name].(*bool)
	if !ok {
		panic(fmt.Sprintf("FlagValues.Bool: %q not bound as bool", name))
	}
	return *p
}

// Int returns the parsed int value for the named flag.
func (v FlagValues) Int(name string) int {
	p, ok := v[name].(*int)
	if !ok {
		panic(fmt.Sprintf("FlagValues.Int: %q not bound as int", name))
	}
	return *p
}

// Int64 returns the parsed int64 value for the named flag.
func (v FlagValues) Int64(name string) int64 {
	p, ok := v[name].(*int64)
	if !ok {
		panic(fmt.Sprintf("FlagValues.Int64: %q not bound as int64", name))
	}
	return *p
}

// Duration returns the parsed time.Duration value for the named flag.
func (v FlagValues) Duration(name string) time.Duration {
	p, ok := v[name].(*time.Duration)
	if !ok {
		panic(fmt.Sprintf("FlagValues.Duration: %q not bound as duration", name))
	}
	return *p
}

// StringSlice returns the parsed []string value for the named flag.
func (v FlagValues) StringSlice(name string) []string {
	p, ok := v[name].(*[]string)
	if !ok {
		panic(fmt.Sprintf("FlagValues.StringSlice: %q not bound as stringSlice", name))
	}
	return *p
}

// bindFlags walks cmd.Flags and registers each typed flag (Type !=
// "") with fs, returning a FlagValues map keyed by long name. The
// auto-injected --help flag is owned by parseAndCheck, not bindFlags.
// Specs with Type == "" are skipped so handlers can mix bindFlags
// with manual fs.String / fs.Bool registrations during incremental
// migration.
//
// Panics on misconfiguration (unknown FlagType, DefaultValue type
// mismatch). These are programmer errors caught at first run.
func bindFlags(cmd Command, fs *flag.FlagSet) FlagValues {
	out := FlagValues{}
	for _, f := range cmd.Flags {
		if f.Type == "" {
			continue
		}
		switch f.Type {
		case FlagString:
			def := defaultAs[string](f, "")
			if f.Short != "" {
				out[f.Name] = fs.StringP(f.Name, f.Short, def, f.Desc)
			} else {
				out[f.Name] = fs.String(f.Name, def, f.Desc)
			}
		case FlagBool:
			def := defaultAs[bool](f, false)
			if f.Short != "" {
				out[f.Name] = fs.BoolP(f.Name, f.Short, def, f.Desc)
			} else {
				out[f.Name] = fs.Bool(f.Name, def, f.Desc)
			}
		case FlagInt:
			def := defaultAs[int](f, 0)
			if f.Short != "" {
				out[f.Name] = fs.IntP(f.Name, f.Short, def, f.Desc)
			} else {
				out[f.Name] = fs.Int(f.Name, def, f.Desc)
			}
		case FlagInt64:
			def := defaultAs[int64](f, 0)
			if f.Short != "" {
				out[f.Name] = fs.Int64P(f.Name, f.Short, def, f.Desc)
			} else {
				out[f.Name] = fs.Int64(f.Name, def, f.Desc)
			}
		case FlagDuration:
			var def time.Duration
			switch dv := f.DefaultValue.(type) {
			case nil:
			case time.Duration:
				def = dv
			case string:
				d, err := time.ParseDuration(dv)
				if err != nil {
					panic(fmt.Sprintf("bindFlags: --%s default %q: %v", f.Name, dv, err))
				}
				def = d
			default:
				panic(fmt.Sprintf("bindFlags: --%s DefaultValue must be time.Duration or string, got %T", f.Name, f.DefaultValue))
			}
			if f.Short != "" {
				out[f.Name] = fs.DurationP(f.Name, f.Short, def, f.Desc)
			} else {
				out[f.Name] = fs.Duration(f.Name, def, f.Desc)
			}
		case FlagStringSlice:
			def := defaultAs[[]string](f, nil)
			if f.Short != "" {
				out[f.Name] = fs.StringSliceP(f.Name, f.Short, def, f.Desc)
			} else {
				out[f.Name] = fs.StringSlice(f.Name, def, f.Desc)
			}
		default:
			panic(fmt.Sprintf("bindFlags: --%s unknown FlagType %q", f.Name, f.Type))
		}
	}
	return out
}

// defaultAs extracts a typed default from f.DefaultValue, returning
// fallback when DefaultValue is nil. Panics on type mismatch -- a
// programmer error (spec declared FlagInt but DefaultValue is a string).
func defaultAs[T any](f FlagSpec, fallback T) T {
	if f.DefaultValue == nil {
		return fallback
	}
	v, ok := f.DefaultValue.(T)
	if !ok {
		panic(fmt.Sprintf("bindFlags: --%s DefaultValue type mismatch (got %T)", f.Name, f.DefaultValue))
	}
	return v
}

// handleParentHelp renders the parent command's COMMANDS listing
// when the operator typed `sparkwing tokens --help` / `sparkwing
// tokens -h` / `sparkwing tokens help`. Only matches when the help
// token is the FIRST argument so it doesn't steal --help from a
// subcommand further down (e.g. `sparkwing tokens create --help`
// routes to the tokens-create handler, not here).
func handleParentHelp(cmd Command, args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-h", "--help", "help":
		renderHelp(cmd, args, os.Stdout)
		return true
	}
	return false
}

// renderHelp dispatches between prose help (PrintHelp) and the
// agent-friendly JSON shape (toCommandJSON), depending on whether
// the caller passed --json or -o json alongside --help. Default is
// the prose renderer that humans expect.
//
// Detection is done on raw args rather than a parsed FlagSet
// because by the time we get here the FlagSet may not have a
// --json or --output declared (most commands do, but not all).
// Args-level inspection is robust to both shapes.
func renderHelp(cmd Command, args []string, w io.Writer) {
	if wantsJSONHelp(args) {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(toCommandJSON(&cmd))
		return
	}
	PrintHelp(cmd, w)
}

// wantsJSONHelp reports whether the args contain a JSON-output
// hint: --json, --output=json, --output json, -o=json, -o json.
// Conservative on positionals (only walks named flags) so a
// positional value happening to equal "json" doesn't trigger.
func wantsJSONHelp(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			return true
		case a == "--output=json", a == "-o=json":
			return true
		case a == "--output", a == "-o":
			if i+1 < len(args) && args[i+1] == "json" {
				return true
			}
		}
	}
	return false
}
