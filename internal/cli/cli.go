// Package cli implements the switchport command line.
package cli

import (
	"context"
	"fmt"
	"io"
)

const usage = `usage:
  switchport run [name] [--port n] [--force] [--h2c] -- <command>
  switchport list [--json]
  switchport get <name> [--no-worktree] [--wait duration]
  switchport alias <name> <port> [--force] [--h2c]
  switchport alias --remove <name>
  switchport prune
  switchport trust [--remove]
  switchport daemon run|status|stop
  switchport version
`

const (
	exitUsage    = 2
	exitNotFound = 127
)

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "switchport:", err)
	return 1
}

func failUsage(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "switchport: %v\n%s", err, usage)
	return exitUsage
}

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
	case "list":
		return listCommand(args[1:], stdout, stderr)
	case "get":
		return getCommand(ctx, args[1:], stdout, stderr)
	case "alias":
		return aliasCommand(args[1:], stdout, stderr)
	case "prune":
		return pruneCommand(stdout, stderr)
	case "trust":
		return trustCommand(args[1:], stdout, stderr)
	case "daemon":
		return daemonCommand(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "switchport: unknown command %q\n%s", args[0], usage)
	return exitUsage
}
