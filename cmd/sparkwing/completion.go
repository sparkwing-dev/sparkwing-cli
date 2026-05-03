// `sparkwing completion <bash|zsh|fish>` emits a shell-completion
// script to stdout. The user sources it from their shell init:
//
//	# bash
//	source <(sparkwing completion bash)
//
//	# zsh
//	source <(sparkwing completion zsh)
//
//	# fish
//	sparkwing completion fish | source
//
// Hand-rolled rather than generated because sparkwing doesn't use
// cobra. All human-facing behavior (top-level subcommand list,
// verb lists under tokens/profiles/users/jobs/pipelines, flag names
// + descriptions per leaf) is driven off help_registry.go so tab
// completion and --help describe the same command tree.
//
// Scope:
//   - top-level subcommand menu (`sparkwing <TAB>`)
//   - verb menus under parent commands (`sparkwing tokens <TAB>`)
//   - pipeline-name completion for `sparkwing run <TAB>` and `wing <TAB>`
//   - profile-name completion for `--on <TAB>`
//   - flag completion for every leaf (`sparkwing <cmd> --<TAB>`) with
//     descriptions + [required]/[conditional] tags pulled from the
//     registry
//
// Adding a new command? Declare it in help_registry.go (and its
// parent's Subcommands slice). Completion picks it up automatically
// -- no edit to this file is required.
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	flag "github.com/spf13/pflag"

	"github.com/sparkwing-dev/sparkwing-cli/pkg/pipelines"
	"github.com/sparkwing-dev/sparkwing-sdk/profile"
)

func runCompletion(args []string) error {
	fs := flag.NewFlagSet(cmdCompletion.Path, flag.ContinueOnError)
	shell := fs.String("shell", "", "shell to emit completion for (bash | zsh | fish)")
	if err := parseAndCheck(cmdCompletion, fs, args); err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		PrintHelp(cmdCompletion, os.Stderr)
		return fmt.Errorf("completion: unexpected positional %q (use --shell)", fs.Arg(0))
	}
	if *shell == "" {
		PrintHelp(cmdCompletion, os.Stderr)
		return errors.New("completion: --shell is required (bash | zsh | fish)")
	}
	switch *shell {
	case "bash":
		fmt.Print(renderBash())
	case "zsh":
		fmt.Print(renderZsh())
	case "fish":
		fmt.Print(renderFish())
	default:
		return fmt.Errorf("completion: unknown shell %q (expected bash|zsh|fish)", *shell)
	}
	return nil
}

// ---------------------------------------------------------------
// Hidden helper subcommands called by the completion scripts.
// All three print one entry per line and exit 0 quietly; completion
// scripts rely on empty output to mean "nothing to offer" and must
// not crash on missing config.
// ---------------------------------------------------------------

// runInternalCompleteProfiles prints one profile name per line. Used
// when the user tabs after `--on`.
func runInternalCompleteProfiles(_ []string) error {
	path, err := profile.DefaultPath()
	if err != nil {
		return nil //nolint:nilerr // silent failure is correct for completion context
	}
	cfg, err := profile.Load(path)
	if err != nil {
		return nil //nolint:nilerr
	}
	for _, n := range cfg.Names() {
		fmt.Println(n)
	}
	return nil
}

// runInternalCompletePipelines walks up from cwd to locate
// .sparkwing/pipelines.yaml and prints "name\tdescription" per
// pipeline for `sparkwing run <TAB>` / `wing <TAB>`. The
// description is the pipeline's Help() text (truncated to one
// line) when the describe-cache has it, falling back to the
// trigger summary when no description is available.
func runInternalCompletePipelines(_ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return nil //nolint:nilerr
	}
	shortByName := map[string]string{}
	helpByName := map[string]string{}
	if sparkwingDir, ok := walkUpForSparkwing(cwd); ok {
		if schema, serr := readDescribeCache(sparkwingDir); serr == nil {
			for _, dp := range schema {
				if dp.Short != "" {
					shortByName[dp.Name] = dp.Short
				}
				if dp.Help != "" {
					helpByName[dp.Name] = dp.Help
				}
			}
		}
	}
	// Output format is 4 tab-separated columns: name, group, type, desc.
	//   - group is a section header (user-set via `group:` / `# group:`,
	//     or falls back to "Pipelines" / "Commands" / "Scripts")
	//   - type is one of "pipeline" (triggered yaml entry), "command"
	//     (manual-only yaml entry), "script" (.sh file). Rendered as
	//     a colored aligned column in zsh so readers can scan the
	//     trigger model at a glance.
	//   - desc is the single-line description
	// Entries are sorted alphabetically within each group; pipelines
	// and commands intermix inside a shared group.
	type completionRow struct {
		name  string
		group string
		kind  string // "pipeline", "command", "script"
		desc  string
	}
	var rows []completionRow
	pipelineNames := map[string]struct{}{}
	if _, cfg, derr := pipelines.Discover(cwd); derr == nil && cfg != nil {
		for _, p := range cfg.Pipelines {
			pipelineNames[p.Name] = struct{}{}
			if p.Hidden {
				continue
			}
			defaultGroup := "Manual"
			if hasAutoTrigger(p.On) {
				defaultGroup = "Triggered"
			}
			group := p.Group
			if group == "" {
				group = defaultGroup
			}
			rows = append(rows, completionRow{
				name:  p.Name,
				group: group,
				desc:  shortPipelineHint(shortByName[p.Name], helpByName[p.Name], p),
			})
		}
	}
	// Group by section in first-seen order (so yaml ordering +
	// script discovery order drive section order), sort alphabetically
	// within each section.
	groupOrder := []string{}
	byGroup := map[string][]completionRow{}
	for _, r := range rows {
		if _, seen := byGroup[r.group]; !seen {
			groupOrder = append(groupOrder, r.group)
		}
		byGroup[r.group] = append(byGroup[r.group], r)
	}
	for _, g := range groupOrder {
		list := byGroup[g]
		sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
		for _, r := range list {
			fmt.Printf("%s\t%s\t%s\t%s\n",
				strings.ReplaceAll(r.name, "\t", " "),
				r.group,
				r.kind,
				strings.ReplaceAll(r.desc, "\t", " "))
		}
	}
	return nil
}

// runInternalCompleteFlags prints "--flag\tdescription" per flag
// registered on the command whose argv path equals args.
//
// Called by zsh/bash when the current word starts with "--". The
// completion script reconstructs the argv (dropping flags already on
// the line) and passes it as positional arguments, e.g.
//
//	sparkwing _complete-flags tokens create
//
// Missing or unknown paths produce empty output so the shell falls
// through to its default behavior.
func runInternalCompleteFlags(args []string) error {
	if len(args) == 0 {
		return nil
	}
	// Walk from longest prefix to shortest so multi-word leaves like
	// "tokens create" win over "tokens" when both could match, and so
	// "run build-test-deploy" still resolves to the "run" leaf once
	// the exact-length match fails. Without this, typing
	// `sparkwing run <pipeline> --<TAB>` offered nothing because the
	// pipeline name was being treated as part of the leaf key.
	//
	// Output format is tab-separated so the shell can split it into
	// three columns for rendering:
	//
	//	--flag<TAB>GroupName<TAB>Description
	//
	// Empty GroupName defaults to "Other" on the shell side, matching
	// the help-registry behavior. Flags emit in the cmd.GroupOrder
	// order so zsh renders "Source", "System", "Other" in the same
	// order --help uses.
	leaves := leafCommands()
	for n := len(args); n >= 1; n-- {
		key := strings.Join(args[:n], " ")
		cmd, ok := leaves[key]
		if !ok {
			continue
		}
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
		// Preserve the group-grouping order from the registry so the
		// shell's `_describe` per-group iteration matches --help.
		groups := groupFlagsForHelp(flags, cmd.GroupOrder)
		for _, g := range groups {
			for _, f := range g.flags {
				desc := requirementTag(f.Required, f.RequiredWhen) + f.Desc
				desc = strings.ReplaceAll(desc, "\t", " ")
				group := strings.ReplaceAll(g.name, "\t", " ")
				fmt.Printf("--%s\t%s\t%s\n", f.Name, group, desc)
			}
		}
		return nil
	}
	return nil
}

// requirementTag returns a leading "[required] " / "[conditional] "
// / "[optional] " marker for tab-completion descriptions. Every flag
// gets a tag so the menu never shows a row without one. Plain text:
// ANSI color codes in compadd description strings confuse zsh's
// column-width tracking and duplicate rows on menu redraws in small
// terminals, so we skip the coloring. Terminals render plain text
// reliably regardless of size.
func requirementTag(required bool, requiredWhen string) string {
	switch {
	case required:
		return "[required] "
	case requiredWhen != "":
		return "[conditional] "
	default:
		return "[optional] "
	}
}

// runInternalCompletePipelineFlags emits the typed flags for one
// pipeline as "--name\tGroup\tDescription" lines, pulled from the
// describe cache. Used when the user types `wing <pipeline> --<TAB>`
// to merge pipeline-specific flags with wing-owned flags in the
// shell completion menu.
//
// Silent on cache miss (no cache file yet, unknown pipeline). The
// caller falls back to wing-owned flags only, which matches user
// expectation that "the first time you tab after editing a pipeline,
// typed flags may not appear yet".
func runInternalCompletePipelineFlags(args []string) error {
	if len(args) != 1 {
		return nil
	}
	pipelineName := args[0]
	cwd, err := os.Getwd()
	if err != nil {
		return nil //nolint:nilerr
	}
	// Walk up to find .sparkwing/, same as findSparkwingDir but
	// silent on failure (a completion request outside a sparkwing
	// repo has nothing to offer).
	sparkwingDir, ok := walkUpForSparkwing(cwd)
	if !ok {
		return nil
	}
	schema, err := pipelineFlagsFromCache(sparkwingDir, pipelineName)
	if err == nil && len(schema) > 0 {
		for _, a := range schema {
			// Group name is "Pipeline Args" so zsh renders the section
			// header distinctly from wing-owned "Source" / "System" groups.
			group := "Pipeline Args"
			desc := requirementTag(a.Required, "") + a.Desc
			if a.Desc == "" {
				desc = requirementTag(a.Required, "") + a.Type
			}
			desc = strings.ReplaceAll(desc, "\t", " ")
			fmt.Printf("--%s\t%s\t%s\n", a.Name, group, desc)
		}
		return nil
	}
	return nil
}

// walkUpForSparkwing mirrors findSparkwingDir but returns a bool
// rather than surfacing the "no .sparkwing/ found" error -- the
// completion path wants silent fallthrough, not an error dump.
func walkUpForSparkwing(start string) (string, bool) {
	dir := start
	for {
		candidate := strings.TrimRight(dir, "/") + "/.sparkwing"
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			if _, err := os.Stat(candidate + "/main.go"); err == nil {
				return candidate, true
			}
		}
		parent := strings.TrimRight(dir, "/")
		if idx := strings.LastIndex(parent, "/"); idx >= 0 {
			parent = parent[:idx]
			if parent == "" {
				parent = "/"
			}
		} else {
			return "", false
		}
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// runInternalCompleteHint identifies the longest leaf-command prefix
// of args and prints "placeholder\trequirement\tdescription" for the
// next positional argument the user hasn't typed yet. Completion
// scripts use this to render a non-clickable hint in zsh's menu when
// the cursor is at a positional slot, so the operator sees
// "<name> [required] Dashboard username" after typing `users add `
// with nothing after it.
//
// Returns empty output when args doesn't match a known leaf OR when
// all positionals are already supplied -- completion scripts then
// fall through to whatever other source (verbs, pipelines) applies.
func runInternalCompleteHint(args []string) error {
	leaves := leafCommands()
	// Try longest prefix first -- leaves can be multi-word
	// ("tokens create" vs "tokens"), so the longest match wins.
	for n := len(args); n >= 1; n-- {
		key := strings.Join(args[:n], " ")
		cmd, ok := leaves[key]
		if !ok {
			continue
		}
		typed := len(args) - n
		if typed >= len(cmd.PosArgs) {
			return nil
		}
		p := cmd.PosArgs[typed]
		req := "optional"
		if p.Required {
			req = "required"
		}
		fmt.Printf("%s\t%s\t%s\n",
			strings.ReplaceAll(p.Name, "\t", " "),
			req,
			strings.ReplaceAll(p.Desc, "\t", " "))
		return nil
	}
	return nil
}

// runInternalCompleteVerbs prints "name\tsynopsis" per subcommand of
// the given parent path. Called by completion when the user is at
// the second word of a two-level command (e.g. `sparkwing tokens <TAB>`).
func runInternalCompleteVerbs(args []string) error {
	key := strings.Join(args, " ")
	parents := parentCommands()
	cmd, ok := parents[key]
	if !ok {
		return nil
	}
	for _, s := range completableSubcommands(cmd) {
		fmt.Printf("%s\t%s\n",
			strings.ReplaceAll(s.Name, "\t", " "),
			strings.ReplaceAll(s.Synopsis, "\t", " "))
	}
	return nil
}

// hasAutoTrigger reports whether a pipeline has any trigger that
// fires without a human typing the command -- push, webhook,
// schedule, deploy-of, or git-hook. ManualTrigger doesn't count; it
// is the explicit "manual only" marker and behaves the same as no
// triggers at all for invocation purposes.
func hasAutoTrigger(t pipelines.Triggers) bool {
	return t.Push != nil ||
		t.Webhook != nil ||
		t.Schedule != "" ||
		t.Deploy != nil ||
		t.PreHook != nil ||
		t.PostHook != nil
}

// shortPipelineHint returns the single-line description rendered next
// to a pipeline name at tab-complete time. Preference order: the
// pipeline's ShortHelp() (single-line hint) when present, else a
// flattened truncation of Help(), else the trigger summary from
// pipelines.yaml as a last-resort fallback for stale caches.
func shortPipelineHint(short, help string, p pipelines.Pipeline) string {
	if s := flattenOneLine(short); s != "" {
		return s
	}
	if h := flattenOneLine(help); h != "" {
		return h
	}
	if t := summarizePipelineTriggers(p.On); t != "" {
		return t
	}
	return "pipeline"
}

// flattenOneLine collapses whitespace runs (including newlines) into
// single spaces and truncates at 80 chars with a trailing ellipsis.
// Sentence-boundary truncation would drop short lead sentences like
// "Read-only." that tell the reader nothing; fitting as much prose as
// one terminal row holds is more useful at completion time.
func flattenOneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	const maxLen = 80
	if len(s) > maxLen {
		s = strings.TrimSpace(s[:maxLen-1]) + "…"
	}
	return s
}

// summarizePipelineTriggers condenses the On: block into a compact
// phrase. Example outputs: "push=main", "webhook=/x", "manual".
func summarizePipelineTriggers(t pipelines.Triggers) string {
	var bits []string
	if t.Push != nil {
		if len(t.Push.Branches) > 0 {
			bits = append(bits, "push="+strings.Join(t.Push.Branches, ","))
		} else {
			bits = append(bits, "push")
		}
	}
	if t.Webhook != nil {
		bits = append(bits, "webhook="+t.Webhook.Path)
	}
	if t.Schedule != "" {
		bits = append(bits, "schedule="+t.Schedule)
	}
	if t.Deploy != nil {
		bits = append(bits, "deploy")
	}
	if t.PreHook != nil {
		bits = append(bits, "pre-commit")
	}
	if t.PostHook != nil {
		bits = append(bits, "pre-push")
	}
	if t.Manual != nil && len(bits) == 0 {
		bits = append(bits, "manual")
	}
	return strings.Join(bits, " ")
}

// ---------------------------------------------------------------
// Registry -> completion model helpers. Keeping these centralized
// means the zsh/bash/fish generators all describe the same tree.
// ---------------------------------------------------------------

// leafCommands returns every leaf command (no Subcommands) keyed by
// its argv path: the Command's Path field with the leading
// "sparkwing " stripped. Wing's own Path is just "wing" (no
// prefix), so it appears keyed as "wing".
//
// Hidden:true commands are still returned because they remain
// dispatchable -- the surfacing decision belongs to the caller
// (`sparkwing commands` filters them; tab-complete keeps them so
// `wing <TAB>` still works).
//
// Derived from allCommands so adding a new Command anywhere in
// help_registry.go AND the allCommands slice automatically updates
// completion. The TestAllCommandsAreRegistered guard catches any
// new Command that's not in the slice -- so the "remembering to
// update completion.go" failure mode is gone.
func leafCommands() map[string]Command {
	out := make(map[string]Command, len(allCommands))
	for _, c := range allCommands {
		if len(c.Subcommands) > 0 {
			continue
		}
		key := strings.TrimPrefix(c.Path, "sparkwing ")
		// The top-level "sparkwing" itself isn't a leaf -- it has
		// Subcommands -- so this branch is just defensive.
		if key == "sparkwing" {
			continue
		}
		out[key] = *c
	}
	return out
}

// parentCommands returns every Command with a Subcommands list keyed
// by argv path. Empty-string key is the top-level `sparkwing`. Like
// leafCommands(), derived from allCommands so registering a new
// parent in help_registry.go is the single touch point.
func parentCommands() map[string]Command {
	out := make(map[string]Command, len(allCommands))
	for _, c := range allCommands {
		if len(c.Subcommands) == 0 {
			continue
		}
		key := strings.TrimPrefix(c.Path, "sparkwing ")
		if key == "sparkwing" {
			key = ""
		}
		out[key] = *c
	}
	return out
}

// topLevelSubcommands returns the top-level subcommand list in a
// stable rendering order, with HideFromComplete entries dropped so
// rarely-typed verbs (commands, completion) don't clutter
// tab-complete. Drawn from cmdSparkwing.Subcommands.
func topLevelSubcommands() []SubcommandRef {
	return completableSubcommands(cmdSparkwing)
}

// parentNames returns the sorted list of parent-command names (just
// the first-token parents, for the zsh case dispatch).
func parentNames() []string {
	var out []string
	for k := range parentCommands() {
		if k == "" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------
// Shell script generators.
// ---------------------------------------------------------------

// renderBash emits a bash completion script. bash's compgen has no
// per-item description, so the menu is name-only. It still provides
// tree-awareness (subcommand names, verb names under parents, flag
// names per leaf) -- enough to navigate.
func renderBash() string {
	var b strings.Builder
	b.WriteString(`# sparkwing bash completion
_sparkwing_complete() {
    local cur prev words cword
    _init_completion || return

    # Reconstruct the command argv (everything except the current
    # word and any word starting with '-'). The result is the path
    # we feed to the internal completion helpers.
    local -a swpath
    local w
    local i
    for (( i=1; i<cword; i++ )); do
        w="${words[i]}"
        [[ "$w" == -* ]] && continue
        swpath+=("$w")
    done

    # Flag completion: current word starts with '-'.
    if [[ "$cur" == -* ]]; then
        local -a out
        mapfile -t out < <(sparkwing _complete-flags "${swpath[@]}" 2>/dev/null | cut -f1)
        COMPREPLY=( $(compgen -W "${out[*]}" -- "$cur") )
        return
    fi

    # Value completion for --on profile names.
    if [[ "$prev" == "--on" ]]; then
        local names
        names=$(sparkwing _complete-profiles 2>/dev/null)
        COMPREPLY=( $(compgen -W "$names" -- "$cur") )
        return
    fi

    # Value completion for --pipeline pipeline names.
    if [[ "$prev" == "--pipeline" ]]; then
        local names
        names=$(sparkwing _complete-pipelines 2>/dev/null | cut -f1)
        COMPREPLY=( $(compgen -W "$names" -- "$cur") )
        return
    fi

    # Subcommand / verb: ask the binary what children are legal at
    # this depth. Empty path -> top-level subcommands. sparkwing has
    # no positional args anywhere -- every leaf takes only named
    # flags -- so we stop after verbs. Value completion for specific
    # flags (above: --on, --pipeline) happens before this block.
    local -a kids
    mapfile -t kids < <(sparkwing _complete-verbs "${swpath[@]}" 2>/dev/null | cut -f1)
    if (( ${#kids[@]} > 0 )); then
        COMPREPLY=( $(compgen -W "${kids[*]}" -- "$cur") )
        return
    fi
}
complete -F _sparkwing_complete sparkwing

# wing is the pipeline-runner symlink; first positional is a pipeline.
_wing_complete() {
    local cur
    _get_comp_words_by_ref cur
    local pipes
    pipes=$(sparkwing _complete-pipelines 2>/dev/null | cut -f1)
    COMPREPLY=( $(compgen -W "$pipes" -- "$cur") )
}
complete -F _wing_complete wing
`)
	return b.String()
}

// renderZsh emits a zsh completion script. zsh is the target shell
// for "rich" completions -- every item carries a description, flags
// are listed per-leaf with their registry descriptions, and the
// menu uses compadd -l so shared descriptions (e.g. every demo-*
// pipeline tagged "demo") don't collapse into grid cells.
func renderZsh() string {
	var b strings.Builder
	b.WriteString(`#compdef sparkwing
# sparkwing zsh completion
# Usage:
#   autoload -U compinit; compinit
#   source <(sparkwing completion zsh)

# Enable group-name rendering and a format for group descriptions so
# compadd's -X explanation text is shown as a bold/colored header
# instead of just "<groupname>:". Scoped to the sparkwing/wing
# completion contexts so the user's other completions stay untouched.
# %d is the -X explanation (which itself contains %F/%B/etc. prompt
# escapes we emit from _sparkwing_complete_flags); %b%f reset bold
# and foreground color so the matches below render in the default
# style. Without this zstyle, compadd -X prints the raw explanation
# unformatted.
zstyle ':completion:*:sparkwing:*:descriptions' format '%d%b%f'
zstyle ':completion:*:wing:*:descriptions'      format '%d%b%f'
zstyle ':completion:*:sparkwing:*'              group-name ''
zstyle ':completion:*:wing:*'                   group-name ''
# list-colors was tried here for coloring [pipeline]/[command]/[script]
# and [required]/[optional] markers but had no effect: zsh's list-colors
# matches against the match name (compadd -a), not the display string
# (compadd -d), and our kind/required markers live in the display.
# Coloring would require restructuring so the marker is part of the
# match name, which changes what's inserted on <Enter>. Not worth it.

_sparkwing() {
    local -a swpath
    local w
    local i
    # Rebuild the non-flag argv path up to (but not including) the
    # current word. Feeds the _complete-* hidden helpers.
    for (( i=2; i<CURRENT; i++ )); do
        w="${words[i]}"
        [[ "$w" == -* ]] && continue
        swpath+=("$w")
    done

    # Flag completion: current word starts with '-'.
    if [[ "${words[CURRENT]}" == -* ]]; then
        _sparkwing_complete_flags "${swpath[@]}"
        return
    fi

    # Value completion: --on <TAB> -> profile names.
    if [[ ${CURRENT} -ge 2 && "${words[CURRENT-1]}" == "--on" ]]; then
        local -a profs
        profs=( ${(f)"$(sparkwing _complete-profiles 2>/dev/null)"} )
        _describe -t profiles 'profile' profs
        return
    fi

    # Value completion: --pipeline <TAB> -> pipeline names (the catalog
    # of pipelines + scripts). Fires anywhere --pipeline precedes the
    # cursor, so describe/explain/new all share the same menu.
    if [[ ${CURRENT} -ge 2 && "${words[CURRENT-1]}" == "--pipeline" ]]; then
        _sparkwing_complete_pipelines
        return
    fi

    # Positional completion for "sparkwing run <TAB>". Run is the
    # one verb in the sparkwing surface that takes the pipeline as a
    # positional rather than via --pipeline; this branch makes its
    # tab-complete match the wing shortcut. Fires when the path so
    # far is exactly ["run"] and we haven't typed a pipeline name
    # yet (so words[2] is the cursor or empty).
    if (( ${#swpath[@]} == 1 )) && [[ "${swpath[1]}" == "run" ]]; then
        _sparkwing_complete_pipelines
        return
    fi

    # Otherwise: try verbs / subcommands at this depth. If that
    # yields nothing (we're past the leaf), fall through to a
    # positional-argument hint + flag list so the operator sees both
    # what's expected next AND what flags the leaf accepts. Flags are
    # included even when words[CURRENT] isn't '-*' so that a bare
    # <TAB> after a leaf reveals the flag menu without forcing the
    # operator to type '-' first.
    if ! _sparkwing_complete_verbs "${swpath[@]}"; then
        _sparkwing_positional_hint "${swpath[@]}"
        _sparkwing_complete_flags "${swpath[@]}"
    fi
}

# _sparkwing_positional_hint renders a non-clickable hint in the
# completion menu describing the next positional argument the cursor
# is sitting on. Uses zsh's _message so the text shows up but TAB
# doesn't try to auto-select a value (there is no enumerable set).
_sparkwing_positional_hint() {
    local line name req desc
    line=$(sparkwing _complete-hint "$@" 2>/dev/null)
    if [[ -z "$line" ]]; then
        return 1
    fi
    name="${line%%$'\t'*}"
    line="${line#*$'\t'}"
    req="${line%%$'\t'*}"
    desc="${line#*$'\t'}"
    if [[ -n "$desc" ]]; then
        _message -r "$name  [$req]  $desc"
    else
        _message -r "$name  [$req]"
    fi
    return 0
}

# _sparkwing_complete_verbs queries the binary for legal children at
# the given path and renders them with per-item descriptions. Empty
# path -> top-level subcommands.
_sparkwing_complete_verbs() {
    local -a names descs
    local line name desc
    while IFS= read -r line; do
        name="${line%%$'\t'*}"
        desc="${line#*$'\t'}"
        names+=("$name")
        if [[ -z "$desc" || "$desc" == "$name" ]]; then
            descs+=("$name")
        else
            descs+=("${(r:14:: :)name}  $desc")
        fi
    done < <(sparkwing _complete-verbs "$@" 2>/dev/null)
    if (( ${#names[@]} > 0 )); then
        compadd -l -d descs -a names
        return 0
    fi
    return 1
}

# _sparkwing_complete_flags queries the binary for flag names +
# descriptions (including [required]/[conditional] prefixes) for the
# leaf at the given path. Falls through silently when no leaf matches.
_sparkwing_complete_flags() {
    # Helper emits three tab-separated fields per line:
    #     --flag<TAB>GroupName<TAB>Description
    # We bucket by GroupName in insertion order so zsh's _describe
    # renders one labeled section per group -- operators see the same
    # "Source", "System", "Other" headers the --help page uses.
    local -A _sw_group_names _sw_group_descs
    local -a _sw_group_order
    local line name group desc label

    _sw_absorb_flag_line() {
        name="${line%%$'\t'*}"
        line="${line#*$'\t'}"
        group="${line%%$'\t'*}"
        desc="${line#*$'\t'}"
        [[ -z "$group" ]] && group="Other"
        if [[ -z "${_sw_group_names[$group]-}" ]]; then
            _sw_group_order+=("$group")
            _sw_group_names[$group]=""
            _sw_group_descs[$group]=""
        fi
        _sw_group_names[$group]+="$name"$'\n'
        if [[ -z "$desc" || "$desc" == "$name" ]]; then
            label="$name"
        else
            # Truncate desc so the rendered row never exceeds terminal
            # width. All descs are plain text now (requirementTag
            # stopped emitting ANSI), so simple byte-length math is
            # correct -- no ANSI-strip step needed. Keeping ANSI out
            # of compadd display strings also prevents the duplicated-
            # row corruption zsh's completion engine hits on small-
            # terminal menu redraws.
            local _sw_indent=24
            local _sw_max=$(( ${COLUMNS:-80} - _sw_indent - 1 ))
            if (( _sw_max > 10 )) && (( ${#desc} > _sw_max )); then
                desc="${desc[1,_sw_max-1]}…"
            fi
            label="${(r:22:: :)name}  $desc"
        fi
        _sw_group_descs[$group]+="$label"$'\n'
    }

    # Pipeline-specific flags go FIRST so the "Pipeline Args" group
    # renders at the top of the menu -- operators can tab-cycle
    # directly to the per-pipeline knobs without scrolling past the
    # wing-owned plumbing (--on/--from/--config/...) every time.
    # Two invocation paths carry a pipeline name at a known
    # position:
    #   wing <pipeline> --<TAB>          leaf "wing", pipeline at $2
    #   sparkwing run <pipeline> --<TAB> leaf "run",  pipeline at $2
    # (Pre-v0.42 there was also "pipelines run <pipeline>" with the
    # pipeline at $3; that path is gone.)
    if (( $# >= 2 )) && [[ "$1" == "wing" ]]; then
        while IFS= read -r line; do
            _sw_absorb_flag_line
        done < <(sparkwing _complete-pipeline-flags "$2" 2>/dev/null)
    elif (( $# >= 2 )) && [[ "$1" == "run" ]]; then
        while IFS= read -r line; do
            _sw_absorb_flag_line
        done < <(sparkwing _complete-pipeline-flags "$2" 2>/dev/null)
    fi

    while IFS= read -r line; do
        _sw_absorb_flag_line
    done < <(sparkwing _complete-flags "$@" 2>/dev/null)

    local g
    local -a gnames gdescs
    for g in "${_sw_group_order[@]}"; do
        # Split the \n-joined buffers back into arrays. ${(f)...} on a
        # trailing-newline string yields an empty tail element, so we
        # drop it with the "(@)" parameter flag not-being-available
        # workaround: slice off the trailing entry when it's empty.
        gnames=( "${(@f)_sw_group_names[$g]}" )
        gdescs=( "${(@f)_sw_group_descs[$g]}" )
        # Drop the trailing empty element that ${(@f)...} leaves
        # behind when the source string ends in \n. Using [[ ]] here
        # because -z is a string test, not an arithmetic operator --
        # inside (( )) it never fires and the empty element leaks
        # into compadd, which some zsh configs render as a blank
        # selectable row (or silently drop the whole group).
        (( ${#gnames[@]} > 0 )) && [[ -z "${gnames[-1]}" ]] && gnames=( "${gnames[@]:0:-1}" )
        (( ${#gdescs[@]} > 0 )) && [[ -z "${gdescs[-1]}" ]] && gdescs=( "${gdescs[@]:0:-1}" )
        (( ${#gnames[@]} == 0 )) && continue
        # Sanitize group name into a zsh tag (spaces -> dashes, lower).
        local tag="${(L)g// /-}"
        # Bold + colored group header with a leading glyph so sections
        # are visually distinct in the completion menu. "Pipeline Args"
        # gets cyan (the group operators interact with most often);
        # everything else gets a muted magenta. The %B/%F/%f/%b prompt
        # escapes only render after the zstyle ':completion:*'
        # descriptions format we set at source-time (see the top of
        # this script); without that, compadd -X prints the raw
        # explanation untouched.
        local header_color="magenta"
        [[ "$g" == "Pipeline Args" || "$g" == "Script Args" ]] && header_color="cyan"
        compadd -l -X "%F{${header_color}}%B▸ ${g}%b%f" -J "$tag" -d gdescs -a gnames
    done
}

# _sparkwing_complete_pipelines is shared by 'sparkwing run <TAB>'
# and 'wing <TAB>'. Entries come back tab-separated as four
# columns -- name, group, kind, desc -- where kind is the
# pipeline-trigger model (currently always "pipeline"; the
# column persists for backward compat with the helper output).
# Bucket by group in first-seen order and render each row as:
# name-padded desc.
_sparkwing_complete_pipelines() {
    local -A _sw_p_names _sw_p_raw _sw_p_displays
    local -a _sw_p_order
    local line name group kind desc
    # First pass: collect rows and find the widest name across all
    # entries. One global width keeps every section aligned to the
    # same column (no staggering between groups). Capped at 24 so a
    # single pathologically-long name doesn't starve the desc column.
    local _sw_name_width=0
    local _sw_name_cap=30
    while IFS= read -r line; do
        name="${line%%$'\t'*}"
        line="${line#*$'\t'}"
        group="${line%%$'\t'*}"
        line="${line#*$'\t'}"
        kind="${line%%$'\t'*}"
        desc="${line#*$'\t'}"
        [[ -z "$group" ]] && group="Pipelines"
        if [[ -z "${_sw_p_names[$group]-}" ]]; then
            _sw_p_order+=("$group")
            _sw_p_names[$group]=""
            _sw_p_raw[$group]=""
            _sw_p_displays[$group]=""
        fi
        _sw_p_names[$group]+="$name"$'\n'
        # Stash raw row for the render pass using unit-separator
        # bytes -- never appear in names/descs in practice.
        _sw_p_raw[$group]+="$name"$'\x1f'"$kind"$'\x1f'"$desc"$'\n'
        local _sw_len=${#name}
        (( _sw_len > _sw_name_cap )) && _sw_len=$_sw_name_cap
        (( _sw_len > _sw_name_width )) && _sw_name_width=$_sw_len
    done < <(sparkwing _complete-pipelines 2>/dev/null)

    # Second pass: render every row at the single global width.
    local g nm k d col padded padded_name label
    for g in "${_sw_p_order[@]}"; do
        local raw="${_sw_p_raw[$g]}"
        local -a rows=( "${(@f)raw}" )
        (( ${#rows[@]} > 0 )) && [[ -z "${rows[-1]}" ]] && rows=( "${rows[@]:0:-1}" )
        for row in "${rows[@]}"; do
            nm="${row%%$'\x1f'*}"
            row="${row#*$'\x1f'}"
            k="${row%%$'\x1f'*}"
            d="${row#*$'\x1f'}"

            # Pad the bracketed kind to a fixed visible width for
            # column alignment. ANSI escapes would color these nicely
            # but zsh's completion engine miscounts their width when
            # the menu has to redraw (small terminals + tab cycling),
            # which leaves duplicated rows in the scrollback. Plain
            # text is reliable; color can come back later via
            # zstyle list-colors once the patterns are worked out.
            col="${(r:10:: :)${:-[$k]}}"

            if (( ${#nm} >= _sw_name_width )); then
                padded_name="$nm"
            else
                padded_name="${(r:$_sw_name_width:: :)nm}"
            fi
            if [[ -z "$d" || "$d" == "$nm" ]]; then
                label="$padded_name  $col"
            else
                # Indent = name_width + 2 + 10 + 2
                local _sw_indent=$(( _sw_name_width + 14 ))
                local _sw_max=$(( ${COLUMNS:-80} - _sw_indent - 1 ))
                if (( _sw_max > 10 )) && (( ${#d} > _sw_max )); then
                    d="${d[1,_sw_max-1]}…"
                fi
                label="$padded_name  $col  $d"
            fi
            _sw_p_displays[$g]+="$label"$'\n'
        done
    done

    # Third pass: feed each group to compadd. Do not redeclare g
    # here -- it was already declared above, and typeset g (which
    # local g expands to) with no value and an existing local
    # prints g=<current value> to stdout, which corrupts completion.
    # Re-use the existing local.
    local -a gnames gdisps
    for g in "${_sw_p_order[@]}"; do
        gnames=( "${(@f)_sw_p_names[$g]}" )
        gdisps=( "${(@f)_sw_p_displays[$g]}" )
        (( ${#gnames[@]} > 0 )) && [[ -z "${gnames[-1]}" ]] && gnames=( "${gnames[@]:0:-1}" )
        (( ${#gdisps[@]} > 0 )) && [[ -z "${gdisps[-1]}" ]] && gdisps=( "${gdisps[@]:0:-1}" )
        (( ${#gnames[@]} == 0 )) && continue
        local tag="${(L)g// /-}"
        # Both Pipelines and Scripts render in magenta; cyan is
        # reserved for the "Pipeline Args" / "Script Args" flag
        # groups so operators visually distinguish "what can I run"
        # from "what flags apply".
        compadd -l -X "%F{magenta}%B▸ ${g}%b%f" -J "$tag" -d gdisps -a gnames
    done
}

# wing <TAB>: first positional is a pipeline name; everything else
# is forwarded to the user's pipeline binary so we don't try to
# complete it.
_wing() {
    # 'wing -<TAB>' (flag at position 1, before any pipeline name)
    # -> wing-owned flags. Kept first so the '-' prefix wins over
    # the pipeline-name completer (which would match nothing and
    # let zsh fall through to unpredictable default behavior).
    if [[ $CURRENT -eq 2 && "${words[CURRENT]}" == -* ]]; then
        _sparkwing_complete_flags "wing"
        return
    fi
    # 'wing <TAB>' -> pipeline names.
    if [[ $CURRENT -eq 2 ]]; then
        _sparkwing_complete_pipelines
        return
    fi
    # 'wing <pipeline> --on <TAB>' -> profile names (matches the
    # sparkwing-level completion's --on handling). Kept first so
    # value-of-flag completion wins over generic flag listing.
    if [[ ${CURRENT} -ge 3 && "${words[CURRENT-1]}" == "--on" ]]; then
        local -a profs
        profs=( ${(f)"$(sparkwing _complete-profiles 2>/dev/null)"} )
        _describe -t profiles 'profile' profs
        return
    fi
    # Everything else after a pipeline name -> wing-owned flags
    # (--on/--from/--config/--help) merged with the pipeline's
    # typed flags from the describe cache. Offered on bare <TAB> as
    # well as '--<TAB>' so operators don't have to type '-' first.
    _sparkwing_complete_flags "wing" "${words[2]}"
}

compdef _sparkwing sparkwing
compdef _wing wing
`)
	return b.String()
}

// renderFish emits a fish completion script. fish's `complete -d`
// takes a native description, and its condition DSL
// (__fish_seen_subcommand_from) makes per-leaf flag completion
// cheap to express -- we just emit one `complete` line per flag per
// leaf. Verbose in the generated script, but idiomatic for fish.
func renderFish() string {
	var b strings.Builder
	b.WriteString(`# sparkwing fish completion
# Usage: sparkwing completion fish | source
#   (or write to ~/.config/fish/completions/sparkwing.fish)

function __sparkwing_profiles
    sparkwing _complete-profiles 2>/dev/null
end

function __sparkwing_pipelines
    # Columns: name, group, desc. Fish wants name\tdesc, so skip col 2.
    sparkwing _complete-pipelines 2>/dev/null | awk -F '\t' '{print $1"\t"$3}'
end

function __sparkwing_has_path
    # True if the current (unfinished) token stream matches the
    # given sequence of words exactly. Used to gate flag completions
    # to a specific leaf command.
    set -l tokens (commandline -opc)
    set -l want $argv
    if test (count $tokens) -lt (math (count $want) + 1)
        return 1
    end
    for i in (seq 1 (count $want))
        if test "$tokens[(math $i + 1)]" != "$want[$i]"
            return 1
        end
    end
    return 0
end

# Top-level subcommands.
`)
	for _, s := range topLevelSubcommands() {
		fmt.Fprintf(&b,
			"complete -c sparkwing -f -n 'not __fish_seen_subcommand_from %s' -a %q -d %q\n",
			joinSubcommandNames(topLevelSubcommands()), s.Name, s.Synopsis)
	}
	b.WriteString("\n# Verbs under parent commands.\n")
	for _, parent := range parentNames() {
		parentCmd := parentCommands()[parent]
		for _, s := range completableSubcommands(parentCmd) {
			fmt.Fprintf(&b,
				"complete -c sparkwing -f -n '__fish_seen_subcommand_from %s' -a %q -d %q\n",
				parent, s.Name, s.Synopsis)
		}
	}
	b.WriteString("\n# Pipeline names for `sparkwing run`.\n")
	b.WriteString(`complete -c sparkwing -f -n '__sparkwing_has_path run' -a '(__sparkwing_pipelines)'
`)
	b.WriteString("\n# Flags per leaf, pulled from the registry.\n")

	// Emit flags for every leaf, gated on the argv path.
	// Sort keys for stable output.
	leaves := leafCommands()
	keys := make([]string, 0, len(leaves))
	for k := range leaves {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		cmd := leaves[k]
		flags := append([]FlagSpec(nil), cmd.Flags...)
		if !hasFlagNamed(flags, "help") {
			flags = append(flags, helpFlag)
		}
		for _, f := range flags {
			desc := f.Desc
			switch {
			case f.Required:
				desc = "[required] " + desc
			case f.RequiredWhen != "":
				desc = "[conditional] " + desc
			}
			// Fish long-flag uses -l; short via -s. Value flags
			// (Argument != "") take -r so fish suggests an arg.
			line := fmt.Sprintf(
				"complete -c sparkwing -n '__sparkwing_has_path %s' -l %s",
				k, f.Name)
			if f.Short != "" {
				line += " -s " + f.Short
			}
			if f.Argument != "" {
				line += " -r"
			}
			line += fmt.Sprintf(" -d %q", desc)
			b.WriteString(line + "\n")
		}
	}

	// `wing` gets pipeline completion on its first positional.
	b.WriteString("\n# wing binary: first positional is a pipeline name.\n")
	b.WriteString(`complete -c wing -f -n 'not __fish_seen_subcommand_from (sparkwing _complete-pipelines 2>/dev/null | awk -F "\\t" "{print \\$1}")' -a '(__sparkwing_pipelines)'
`)
	return b.String()
}

// joinSubcommandNames helps the fish generator produce the "not
// __fish_seen_subcommand_from X Y Z" guard on top-level completions
// so names don't keep showing up once the user has already picked
// one.
func joinSubcommandNames(subs []SubcommandRef) string {
	parts := make([]string, len(subs))
	for i, s := range subs {
		parts[i] = s.Name
	}
	return strings.Join(parts, " ")
}
