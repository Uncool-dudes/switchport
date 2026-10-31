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
	"github.com/Uncool-dudes/switchport/internal/registry"
	"github.com/Uncool-dudes/switchport/internal/route"
	"github.com/Uncool-dudes/switchport/internal/worktree"
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
	name  string
	port  int // zero means pick a free one
	force bool
	h2c   bool
	argv  []string
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
	port := fs.Int("port", 0, "")
	force := fs.Bool("force", false, "")
	h2c := fs.Bool("h2c", false, "")
	positional, err := parseMixed(fs, own)
	if err != nil {
		return runOptions{}, err
	}
	if len(positional) > 1 {
		return runOptions{}, fmt.Errorf("unexpected argument %q before --", positional[1])
	}
	if *port < 0 || *port > 65535 {
		return runOptions{}, fmt.Errorf("port %d is out of range", *port)
	}
	name := nameFromDir(dir)
	if len(positional) == 1 {
		name = positional[0]
	} else if _, err := route.NewName(name); err != nil {
		return runOptions{}, fmt.Errorf("cannot name this app from its directory, pass a name: %w", err)
	}
	if _, err := route.NewName(name); err != nil {
		return runOptions{}, err
	}
	return runOptions{name: name, port: *port, force: *force, h2c: *h2c, argv: argv}, nil
}

// parseMixed parses flags that may appear before or after positional arguments.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			positional = append(positional, args[0])
			args = args[1:]
		}
	}
	return positional, nil
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
		return fail(stderr, err)
	}
	opts, err := parseRun(args, dir)
	if err != nil {
		fmt.Fprintf(stderr, "switchport: %v\n%s", err, usage)
		return exitUsage
	}
	prefix := worktree.Prefix(ctx, dir)
	name, err := qualifiedName(prefix, opts.name)
	if err != nil {
		return fail(stderr, err)
	}
	reg, err := openRegistry()
	if err != nil {
		return fail(stderr, err)
	}
	if !opts.force {
		if err := reg.Available(name); err != nil {
			return fail(stderr, fmt.Errorf("%w, use --force to replace it", err))
		}
	}
	if err := ensureDaemon(); err != nil {
		fmt.Fprintln(stderr, "switchport: no proxy running:", err)
	}
	port := opts.port
	if port == 0 {
		if port, err = freePort(); err != nil {
			return fail(stderr, err)
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

	app := appTemplate{name: name, worktree: prefix, opts: opts, port: port}
	discoverCtx, stopDiscover := context.WithCancel(ctx)
	registered := make(chan error, 1)
	go func() {
		registered <- register(discoverCtx, stderr, reg, app, tracker, exited)
	}()

	var regErr error
	select {
	case <-exited:
	case <-ctx.Done():
	case regErr = <-registered:
		if regErr == nil {
			select {
			case <-exited:
			case <-ctx.Done():
			}
		}
	}
	stopDiscover()
	if regErr == nil {
		regErr = waitRegistered(registered)
	}
	if err := reg.RemoveApp(name, os.Getpid()); err != nil {
		fmt.Fprintln(stderr, "switchport:", err)
	}
	if err := stopTree(tracker); err != nil {
		fmt.Fprintln(stderr, "switchport:", err)
		_ = cmd.Process.Kill()
	}
	<-exited

	if regErr != nil {
		return 1
	}
	return exitCode(cmd.ProcessState)
}

type appTemplate struct {
	name     route.Name
	worktree string
	opts     runOptions
	port     int
}

// register discovers the port and adds the route. It returns an error only
// when the command must stop because it cannot own its name.
func register(ctx context.Context, stderr io.Writer, reg *registry.Registry, a appTemplate, tr *proc.Tracker, exited <-chan struct{}) error {
	r, err := proc.Discover(ctx, tr, proc.Options{
		Assigned: a.port,
		Pinned:   a.opts.port != 0,
		Settle:   discoverSettle,
		Timeout:  discoverTimeout,
		Exited:   exited,
	})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintf(stderr, "switchport: %s: %v\n", a.name, err)
		}
		return nil
	}
	procs, err := tr.Snapshot()
	if err != nil {
		fmt.Fprintln(stderr, "switchport:", err)
		return nil
	}
	err = reg.AddApp(route.App{
		Name:     a.name,
		Addr:     r.Addr(),
		H2C:      a.opts.h2c,
		Worktree: a.worktree,
		PID:      os.Getpid(),
		Procs:    procs,
	}, a.opts.force)
	if err != nil {
		fmt.Fprintf(stderr, "switchport: %v\n", err)
		return err
	}
	fmt.Fprintf(stderr, "switchport: %s -> %s (%s)\n", a.name, routeURL(a.name), r.Addr())
	if len(r.Candidates) > 1 {
		fmt.Fprintf(stderr, "switchport: %s opened %d ports, use --port to choose\n", a.name, len(r.Candidates))
	}
	return nil
}

// waitRegistered waits for a canceled register to finish, so that no route is
// added after the wrapper removes it.
func waitRegistered(registered <-chan error) error {
	select {
	case err := <-registered:
		return err
	case <-time.After(killWait):
		return nil
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
