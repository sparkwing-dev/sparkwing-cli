// Command sparkwing-local-ws is a thin wrapper around
// pkg/localws.Run. Preserved as a standalone binary so a user can
// opt into running the dev server as a separate process (docker
// container, systemd unit, etc.) without the sparkwing CLI in the
// same address space.
//
// The laptop default is 'sparkwing dashboard start', which detaches
// a supervisor that calls localws.Run in-process — no separate
// binary required.
//
// Usage:
//
//	sparkwing-local-ws                     # 127.0.0.1:4343, ~/.sparkwing
//	sparkwing-local-ws --addr 0.0.0.0:4343
//	sparkwing-local-ws --home /tmp/sw      # isolated state dir
package main

import (
	"context"
	"fmt"
	"os"

	flag "github.com/spf13/pflag"

	"github.com/sparkwing-dev/sparkwing-cli/pkg/localws"
)

func main() {
	fs := flag.NewFlagSet("sparkwing-local-ws", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:4343", "bind address")
	home := fs.String("home", "",
		"sparkwing state directory (default: $SPARKWING_HOME or ~/.sparkwing)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if err := localws.Run(context.Background(), localws.Options{
		Addr: *addr,
		Home: *home,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "sparkwing-local-ws:", err)
		os.Exit(1)
	}
}
