package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain turns the test binary into a server fixture when FIXTURE is set, so
// the suite needs no language runtime beyond Go.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FIXTURE"); mode != "" {
		time.AfterFunc(30*time.Second, func() { os.Exit(9) })
		runFixture(mode, os.Getenv("FIXTURE_TAG"), strings.Split(os.Getenv("FIXTURE_ARGS"), ","))
		return
	}
	os.Exit(m.Run())
}

func runFixture(mode, tag string, args []string) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tag) })
	serve := func(network, addr string) {
		ln, err := net.Listen(network, addr)
		if err != nil {
			os.Exit(1)
		}
		http.Serve(ln, handler)
	}
	raw := func(addr string) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			os.Exit(1)
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}
	relaunch := func(attr *syscall.SysProcAttr, childMode string) {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "FIXTURE="+childMode)
		cmd.SysProcAttr = attr
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		if attr.Setsid {
			return
		}
		cmd.Wait()
	}
	switch mode {
	case "listen4":
		serve("tcp4", args[0])
	case "listen6":
		serve("tcp6", args[0])
	case "env":
		serve("tcp4", "127.0.0.1:"+os.Getenv("PORT"))
	case "delayed":
		time.Sleep(300 * time.Millisecond)
		serve("tcp4", args[0])
	case "raw":
		raw(args[0])
	case "raw-then-http":
		go raw(args[0])
		time.Sleep(100 * time.Millisecond)
		serve("tcp4", args[1])
	case "two-http":
		go serve("tcp4", args[0])
		serve("tcp4", args[1])
	case "new-group":
		relaunch(&syscall.SysProcAttr{Setpgid: true}, "listen4")
	case "detached":
		relaunch(&syscall.SysProcAttr{Setsid: true}, "delayed")
	case "exit3":
		os.Exit(3)
	case "sleep":
		time.Sleep(time.Hour)
	}
}

type child struct {
	tr     *Tracker
	exited chan struct{}
}

func start(t *testing.T, mode string, env []string, args ...string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	// The race detector otherwise delays every exit by a second.
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "FIXTURE="+mode, "FIXTURE_TAG="+t.Name(), "FIXTURE_ARGS="+strings.Join(args, ","))
	cmd.Env = append(cmd.Env, env...)
	return launch(t, cmd)
}

func launch(t *testing.T, cmd *exec.Cmd) *child {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{tr: NewTracker(cmd.Process.Pid), exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(c.exited)
	}()
	t.Cleanup(func() { c.tr.Signal(syscall.SIGKILL) })
	return c
}

func (c *child) discover(t *testing.T, assigned int) (Result, error) {
	t.Helper()
	return Discover(t.Context(), c.tr, Options{
		Assigned: assigned,
		Settle:   250 * time.Millisecond,
		Timeout:  5 * time.Second,
		Exited:   c.exited,
	})
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func loop4(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

func wantBody(t *testing.T, addr string) {
	t.Helper()
	res, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if string(b) != t.Name() {
		t.Fatalf("body %q from %s", b, addr)
	}
}

func TestDiscoverFindsHonoredPort(t *testing.T) {
	assigned := freePort(t)
	c := start(t, "env", []string{"PORT=" + strconv.Itoa(assigned)})
	r, err := c.discover(t, assigned)
	if err != nil {
		t.Fatal(err)
	}
	if r.Port != assigned {
		t.Fatalf("port %d, want %d", r.Port, assigned)
	}
	wantBody(t, r.Addr())
}

func TestDiscoverFindsPortThatIgnoresAssignment(t *testing.T) {
	for name, tc := range map[string]struct {
		mode string
		host string
	}{
		"loopback":  {"listen4", "127.0.0.1"},
		"wildcard":  {"listen4", "0.0.0.0"},
		"ipv6":      {"listen6", "[::1]"},
		"new-group": {"new-group", "127.0.0.1"},
		"delayed":   {"delayed", "127.0.0.1"},
	} {
		t.Run(name, func(t *testing.T) {
			port := freePort(t)
			c := start(t, tc.mode, nil, tc.host+":"+strconv.Itoa(port))
			r, err := c.discover(t, freePort(t))
			if err != nil {
				t.Fatal(err)
			}
			if r.Port != port {
				t.Fatalf("port %d, want %d", r.Port, port)
			}
			wantBody(t, r.Addr())
		})
	}
}

func TestDiscoverFindsRandomPort(t *testing.T) {
	c := start(t, "listen4", nil, "127.0.0.1:0")
	r, err := c.discover(t, freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	wantBody(t, r.Addr())
}

func TestDiscoverSeesThroughShellPipeline(t *testing.T) {
	port := freePort(t)
	cmd := exec.Command("sh", "-c", `"$0" | cat`, os.Args[0])
	cmd.Env = append(os.Environ(), "FIXTURE=listen4", "FIXTURE_TAG="+t.Name(), "FIXTURE_ARGS="+loop4(port))
	c := launch(t, cmd)
	r, err := c.discover(t, freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	wantBody(t, r.Addr())
}

func TestDiscoverPrefersHTTPOverEarlierRawPort(t *testing.T) {
	rawPort, httpPort := freePort(t), freePort(t)
	c := start(t, "raw-then-http", nil, loop4(rawPort), loop4(httpPort))
	r, err := c.discover(t, freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	if r.Port != httpPort || !r.HTTP {
		t.Fatalf("got %+v, want HTTP port %d", r, httpPort)
	}
	if len(r.Candidates) != 2 {
		t.Fatalf("candidates %v", r.Candidates)
	}
}

func TestDiscoverPinnedAcceptsOnlyAssignedPort(t *testing.T) {
	first, second := freePort(t), freePort(t)
	c := start(t, "two-http", nil, loop4(first), loop4(second))
	r, err := Discover(t.Context(), c.tr, Options{Assigned: second, Pinned: true, Settle: time.Millisecond, Timeout: 5 * time.Second, Exited: c.exited})
	if err != nil {
		t.Fatal(err)
	}
	if r.Port != second {
		t.Fatalf("port %d, want %d", r.Port, second)
	}
}

func TestDiscoverReportsAmbiguousPorts(t *testing.T) {
	c := start(t, "two-http", nil, loop4(freePort(t)), loop4(freePort(t)))
	r, err := c.discover(t, freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	if !r.HTTP || len(r.Candidates) != 2 {
		t.Fatalf("got %+v", r)
	}
}

func TestDiscoverRoutesNonHTTPServer(t *testing.T) {
	port := freePort(t)
	c := start(t, "raw", nil, loop4(port))
	r, err := c.discover(t, freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	if r.Port != port || r.HTTP {
		t.Fatalf("got %+v", r)
	}
}

func TestDiscoverReportsExitBeforeListening(t *testing.T) {
	c := start(t, "exit3", nil)
	if _, err := c.discover(t, freePort(t)); !errors.Is(err, ErrChildExited) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverReportsPortAlreadyTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c := start(t, "listen4", nil, l.Addr().String())
	if _, err := c.discover(t, freePort(t)); !errors.Is(err, ErrChildExited) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverTimesOutWhenNothingListens(t *testing.T) {
	c := start(t, "sleep", nil)
	_, err := Discover(t.Context(), c.tr, Options{Assigned: 1, Settle: time.Millisecond, Timeout: 400 * time.Millisecond, Exited: c.exited})
	if !errors.Is(err, ErrNoListener) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverHonorsContext(t *testing.T) {
	c := start(t, "sleep", nil)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := Discover(ctx, c.tr, Options{Assigned: 1, Settle: time.Millisecond, Timeout: time.Minute, Exited: c.exited})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

// A process that detaches and whose launcher exits is a documented limit.
func TestDiscoverMissesProcessThatDetaches(t *testing.T) {
	c := start(t, "detached", nil, loop4(freePort(t)))
	if _, err := c.discover(t, freePort(t)); !errors.Is(err, ErrChildExited) {
		t.Fatalf("err = %v", err)
	}
}

func TestSignalStopsProcessInNewGroup(t *testing.T) {
	port := freePort(t)
	c := start(t, "new-group", nil, loop4(port))
	r, err := c.discover(t, freePort(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.tr.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", r.Addr(), 100*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("server survived SIGTERM to the tree")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestListenerDialHost(t *testing.T) {
	for ip, want := range map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "127.0.0.1",
		"::1":       "::1",
		"127.0.0.1": "127.0.0.1",
	} {
		if got := (Listener{IP: ip}).DialHost(); got != want {
			t.Errorf("DialHost(%q) = %q, want %q", ip, got, want)
		}
	}
}
