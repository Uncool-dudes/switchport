package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"

	"github.com/Uncool-dudes/switchport/internal/ca"
	"github.com/Uncool-dudes/switchport/internal/daemon"
	"github.com/Uncool-dudes/switchport/internal/trust"
)

const (
	daemonEnv     = "SWITCHPORT_DAEMON" // "off" means something else runs it
	httpsPortEnv  = "SWITCHPORT_HTTPS_PORT"
	daemonDisable = "off"
)

func ensureDaemon() error {
	if os.Getenv(daemonEnv) == daemonDisable {
		return nil
	}
	dir, err := stateDir()
	if err != nil {
		return err
	}
	if _, err := openRegistry(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	_, err = daemon.Ensure(dir, []string{exe, "daemon", "run"})
	return err
}

func configuredPort() (int, error) {
	value := os.Getenv(httpsPortEnv)
	if value == "" {
		return httpsPort, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid %s %q", httpsPortEnv, value)
	}
	return port, nil
}

func daemonCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return failUsage(stderr, errors.New("daemon takes run, status, or stop"))
	}
	dir, err := stateDir()
	if err != nil {
		return fail(stderr, err)
	}
	switch args[0] {
	case "run":
		return daemonRun(ctx, dir, stderr)
	case "status":
		s, ok := daemon.ReadStatus(dir)
		if !ok {
			fmt.Fprintln(stdout, "not running")
			return 1
		}
		fmt.Fprintf(stdout, "running, pid %d, port %d\n", s.PID, s.Port)
		return 0
	case "stop":
		if err := daemon.Stop(dir); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	return failUsage(stderr, fmt.Errorf("unknown daemon command %q", args[0]))
}

func daemonRun(ctx context.Context, dir string, stderr io.Writer) int {
	reg, err := openRegistry()
	if err != nil {
		return fail(stderr, err)
	}
	authority, err := ca.Load(dir, tlds())
	if err != nil {
		return fail(stderr, err)
	}
	port, err := configuredPort()
	if err != nil {
		return fail(stderr, err)
	}
	err = daemon.Run(ctx, daemon.Options{
		Dir:      dir,
		Registry: reg,
		CA:       authority,
		TLDs:     tlds(),
		Port:     port,
		Log:      slog.New(slog.NewTextHandler(stderr, nil)),
	})
	if errors.Is(err, daemon.ErrRunning) {
		return 0
	}
	if err != nil {
		return fail(stderr, err)
	}
	return 0
}

func trustCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	remove := fs.Bool("remove", false, "")
	if _, err := parseMixed(fs, args); err != nil {
		return failUsage(stderr, err)
	}
	if _, err := openRegistry(); err != nil {
		return fail(stderr, err)
	}
	dir, err := stateDir()
	if err != nil {
		return fail(stderr, err)
	}
	authority, err := ca.Load(dir, tlds())
	if err != nil {
		return fail(stderr, err)
	}
	if *remove {
		if err := trust.Remove(authority.Path()); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if trust.Installed(authority.Path()) {
		fmt.Fprintln(stdout, "already trusted")
		return 0
	}
	if err := trust.Install(authority.Path()); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "trusted", authority.Path())
	return 0
}
