// Package cli implements the switchport command line.
package cli

import (
	"context"
	"fmt"
	"io"
)

const usage = `usage:
  switchport run [name] [--port n] -- <command>
  switchport version
`

const (
	exitUsage    = 2
	exitNotFound = 127
)

// Main runs one command and returns its process exit code.
func Main(ctx context.Context, version string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "switchport", version)
		return 0
	case "run":
		return runCommand(ctx, args[1:], stderr)
	}
	fmt.Fprintf(stderr, "switchport: unknown command %q\n%s", args[0], usage)
	return exitUsage
}
