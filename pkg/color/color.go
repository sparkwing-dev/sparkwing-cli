// Package color provides ANSI color helpers for pipeline output.
//
//	fmt.Println(color.Green("deployed %s", version))
//	fmt.Println(color.Dim("skipping %s", name))
//	fmt.Printf("status: %s %s\n", color.Bold("PASS"), color.Dim(duration))
//
// Color emission auto-detects: enabled only when stdout is a TTY and
// neither NO_COLOR nor CI is set. Agents (Claude Code, Cursor, etc.)
// and pipes get plain text. CLICOLOR_FORCE=1 / SPARKWING_FORCE_COLOR=1
// re-enables for the rare case the user wants color through a pipe.
package color

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// enabled is computed once at process start. Pure functions of env +
// the original stdout fd, so the result is stable for the lifetime of
// the process.
var enabled = detectEnabled()

func detectEnabled() bool {
	// Force-on overrides everything; useful for rare "I'm piping but
	// I want colors" cases (e.g. `sparkwing ... | less -R`).
	if os.Getenv("CLICOLOR_FORCE") == "1" || os.Getenv("SPARKWING_FORCE_COLOR") == "1" {
		return true
	}
	// no-color.org standard: any non-empty NO_COLOR disables.
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	// CI / agent runners: usually log-only, no terminal.
	if os.Getenv("CI") != "" {
		return false
	}
	// Default: TTY only. Catches agents (no TTY), pipes, files.
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// SetEnabled overrides the auto-detected setting. Mostly for tests
// and the rare downstream caller that wants explicit control.
func SetEnabled(on bool) { enabled = on }

// Enabled reports whether color output is currently emitted.
func Enabled() bool { return enabled }

func apply(code string, args ...any) string {
	if len(args) == 0 {
		return ""
	}
	text := fmt.Sprint(args[0])
	if len(args) > 1 {
		text = fmt.Sprintf(text, args[1:]...)
	}
	if !enabled {
		return text
	}
	return code + text + "\033[0m"
}

func Red(args ...any) string     { return apply("\033[31m", args...) }
func Green(args ...any) string   { return apply("\033[32m", args...) }
func Yellow(args ...any) string  { return apply("\033[33m", args...) }
func Blue(args ...any) string    { return apply("\033[34m", args...) }
func Magenta(args ...any) string { return apply("\033[35m", args...) }
func Cyan(args ...any) string    { return apply("\033[36m", args...) }
func Bold(args ...any) string    { return apply("\033[1m", args...) }
func Dim(args ...any) string     { return apply("\033[2m", args...) }
