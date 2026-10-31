package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Uncool-dudes/switchport/internal/cli"
)

// version is set at build time with -ldflags -X.
var version = "dev"

func main() {
	os.Exit(run())
}

// run exists so deferred cleanup runs before os.Exit.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return cli.Main(ctx, version, os.Args[1:], os.Stdout, os.Stderr)
}
