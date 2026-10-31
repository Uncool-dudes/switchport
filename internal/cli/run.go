package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Uncool-dudes/switchport/internal/proc"
	"github.com/Uncool-dudes/switchport/internal/route"
)

const (
	portPlaceholder  = "{port}"
	discoverSettle   = 500 * time.Millisecond
	discoverTimeout  = 2 * time.Minute
	stopPollInterval = 50 * time.Millisecond
	killWait         = time.Second
)

// stopGrace is how long a process tree gets after SIGTERM before SIGKILL.
var stopGrace = 5 * time.Second

type runOptions struct {
	name route.Name
	port int // zero means pick a free one
	argv []string
}

// parseRun splits "[name] [--port n] -- command...". The separator is required
// so that flags always belong to the command, whatever tool it is.
func parseRun(args []string, dir string) (runOptions, error) {
	sep := slices.Index(args, "--")
	if sep < 0 || sep == len(args)-1 {
		return runOptions{}, errors.New("missing command: use switchport run [name] -- <command>")
	}
	own, argv := args[:sep], args[sep+1:]

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", 0, "")
	var positional []string
	for len(own) > 0 {
		if err := fs.Parse(own); err != nil {
			return runOptions{}, err
		}
		own = fs.Args()
		if len(own) > 0 {
			positional = append(positional, own[0])
			own = own[1:]
		}
	}
	if len(positional) > 1 {
		return runOptions{}, fmt.Errorf("unexpected argument %q before --", positional[1])
	}
	if *port < 0 || *port > 65535 {
		return runOptions{}, fmt.Errorf("port %d is out of range", *port)
	}

	var name route.Name
	var err error
	if len(positional) == 1 {
		name, err = route.NewName(positional[0])
	} else {
		name, err = route.NewName(nameFromDir(dir))
		if err != nil {
			err = fmt.Errorf("cannot name this app from its directory, pass a name: %w", err)
		}
	}
	if err != nil {
		return runOptions{}, err
	}
	return runOptions{name: name, port: *port, argv: argv}, nil
}

var invalidLabelChars = regexp.MustCompile(`[^a-z0-9]+`)

func nameFromDir(dir string) string {
	base := strings.ToLower(filepath.Base(dir))
	return strings.Trim(invalidLabelChars.ReplaceAllString(base, "-"), "-")
}

// substitutePort replaces every {port} in the command. It is text only, so the
// user states how their tool takes a port and no tool needs to be recognized.
func substitutePort(argv []string, port int) []string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		out[i] = strings.ReplaceAll(arg, portPlaceholder, strconv.Itoa(port))
	}
	return out
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func runCommand(ctx context.Context, args []string, stderr io.Writer) int {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "switchport:", err)
		return 1
	}
	opts, err := parseRun(args, dir)
	if err != nil {
		fmt.Fprintf(stderr, "switchport: %v\n%s", err, usage)
		return exitUsage
	}
	port := opts.port
	if port == 0 {
		if port, err = freePort(); err != nil {
			fmt.Fprintln(stderr, "switchport:", err)
			return 1
		}
	}

	argv := substitutePort(opts.argv, port)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port))

	foreground := holdsTerminal()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: foreground}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "switchport:", err)
		return exitNotFound
	}
	if foreground {
		// Taking the terminal back from a background group raises SIGTTOU.
		signal.Ignore(syscall.SIGTTOU)
		defer restoreTerminal()
	}

	tracker := proc.NewTracker(cmd.Process.Pid)
	exited := make(chan struct{})
	go func() {
		// The exit status is read from cmd.ProcessState below.
		_ = cmd.Wait()
		close(exited)
	}()

	discoverCtx, stopDiscover := context.WithCancel(ctx)
	discovered := make(chan struct{})
	go func() {
		defer close(discovered)
		reportDiscovery(discoverCtx, stderr, opts, tracker, port, exited)
	}()

	select {
	case <-exited:
	case <-ctx.Done():
	}
	stopDiscover()
	<-discovered
	if err := stopTree(tracker); err != nil {
		fmt.Fprintln(stderr, "switchport:", err)
		_ = cmd.Process.Kill()
	}
	<-exited

	return exitCode(cmd.ProcessState)
}

func reportDiscovery(ctx context.Context, stderr io.Writer, opts runOptions, tr *proc.Tracker, port int, exited <-chan struct{}) {
	r, err := proc.Discover(ctx, tr, proc.Options{
		Assigned: port,
		Pinned:   opts.port != 0,
		Settle:   discoverSettle,
		Timeout:  discoverTimeout,
		Exited:   exited,
	})
	switch {
	case err == nil:
		fmt.Fprintf(stderr, "switchport: %s -> %s\n", opts.name, r.Addr())
		if len(r.Candidates) > 1 {
			fmt.Fprintf(stderr, "switchport: %s opened %d ports, use --port to choose\n", opts.name, len(r.Candidates))
		}
	case errors.Is(err, context.Canceled):
	default:
		fmt.Fprintf(stderr, "switchport: %s: %v\n", opts.name, err)
	}
}

// stopTree asks every remaining process to stop, forces any that stay, and
// waits for them to go, so a port is free once this returns.
func stopTree(tr *proc.Tracker) error {
	if err := tr.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	gone, err := waitGone(tr, stopGrace)
	if gone || err != nil {
		return err
	}
	if err := tr.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	_, err = waitGone(tr, killWait)
	return err
}

func waitGone(tr *proc.Tracker, limit time.Duration) (bool, error) {
	deadline := time.Now().Add(limit)
	for {
		alive, err := tr.Alive()
		if err != nil || !alive {
			return !alive, err
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(stopPollInterval)
	}
}

// exitCode follows the shell convention of 128+signal for a signaled process.
func exitCode(state *os.ProcessState) int {
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}
