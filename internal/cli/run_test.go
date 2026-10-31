package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain turns the test binary into a server fixture when FIXTURE is set.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FIXTURE"); mode != "" {
		time.AfterFunc(30*time.Second, func() { os.Exit(9) })
		runFixture(mode)
		return
	}
	stopGrace = 400 * time.Millisecond
	if wd, err := os.Getwd(); err == nil {
		moduleDir = wd
	}
	dir, err := os.MkdirTemp("", "switchport-test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	os.Setenv(stateDirEnv, dir)
	os.Exit(m.Run())
}

func runFixture(mode string) {
	serve := func(addr string) {
		ln, err := net.Listen("tcp4", addr)
		if err != nil {
			os.Exit(1)
		}
		http.Serve(ln, http.NotFoundHandler())
	}
	switch mode {
	case "env":
		serve("127.0.0.1:" + os.Getenv("PORT"))
	case "fixed":
		serve(os.Getenv("FIXTURE_ADDR"))
	case "arg":
		serve("127.0.0.1:" + os.Getenv("FIXTURE_PORT_ARG"))
	case "exit7":
		os.Exit(7)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		serve("127.0.0.1:" + os.Getenv("PORT"))
	case "new-group":
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "FIXTURE=ignore-term")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if cmd.Start() == nil {
			cmd.Wait()
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestParseRun(t *testing.T) {
	for _, tc := range []struct {
		in   string
		name string
		port int
		argv string
	}{
		{"api -- go run .", "api", 0, "go run ."},
		{"-- vite", "my-app", 0, "vite"},
		{"api --port 3000 -- vite --port 3000", "api", 3000, "vite --port 3000"},
		{"--port 3000 api -- vite", "api", 3000, "vite"},
		{"api -- sh -c --", "api", 0, "sh -c --"},
	} {
		got, err := parseRun(strings.Fields(tc.in), "/x/My App")
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got.name != tc.name || got.port != tc.port || strings.Join(got.argv, " ") != tc.argv {
			t.Errorf("%q: got %+v", tc.in, got)
		}
	}
	for _, in := range []string{"", "api", "api --", "a b -- c", "bad_name -- c", "--port x -- c", "--port 70000 -- c"} {
		if _, err := parseRun(strings.Fields(in), "/x/app"); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
	if _, err := parseRun([]string{"--", "c"}, "/___"); err == nil || !strings.Contains(err.Error(), "pass a name") {
		t.Errorf("unusable directory: %v", err)
	}
}

func TestSubstitutePort(t *testing.T) {
	got := substitutePort([]string{"vite", "--port", "{port}", "--url=http://x:{port}/{port}"}, 42)
	want := "vite --port 42 --url=http://x:42/42"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
}

// runSwitchport runs the command with the test binary as its server.
func runSwitchport(ctx context.Context, t *testing.T, mode string, env []string, extra ...string) (int, *syncBuffer) {
	t.Helper()
	t.Setenv("FIXTURE", mode)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	stderr := &syncBuffer{}
	args := append([]string{"run", "app"}, extra...)
	args = append(args, "--", os.Args[0])
	return Main(ctx, "test", args, &bytes.Buffer{}, stderr), stderr
}

func waitFor(t *testing.T, s *syncBuffer, pattern string) string {
	t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindString(s.String()); m != "" {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q in %q", pattern, s.String())
	return ""
}

func waitAddr(t *testing.T, s *syncBuffer) string {
	t.Helper()
	m := waitFor(t, s, `\(127\.0\.0\.1:\d+\)`)
	return strings.Trim(m, "()")
}

func TestRunReturnsChildExitCode(t *testing.T) {
	code, _ := runSwitchport(t.Context(), t, "exit7", nil)
	if code != 7 {
		t.Fatalf("code %d", code)
	}
}

func TestRunReportsMissingCommand(t *testing.T) {
	stderr := &syncBuffer{}
	code := Main(t.Context(), "test", []string{"run", "app", "--", "/no/such/binary"}, &bytes.Buffer{}, stderr)
	if code != exitNotFound || !strings.Contains(stderr.String(), "no such file") {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
}

func TestRunStopsOnCancelAndFreesPort(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	t.Setenv("FIXTURE", "env")
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, "test", []string{"run", "app", "--", os.Args[0]}, &bytes.Buffer{}, stderr)
	}()
	addr := waitAddr(t, stderr)
	cancel()
	if code := <-done; code != 128+int(syscall.SIGTERM) {
		t.Fatalf("code %d", code)
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatalf("%s still open", addr)
	}
}

func TestRunForcesTreeThatIgnoresSIGTERM(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	t.Setenv("FIXTURE", "new-group")
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, "test", []string{"run", "app", "--", os.Args[0]}, &bytes.Buffer{}, stderr)
	}()
	addr := waitAddr(t, stderr)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return")
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Fatalf("%s survived", addr)
	}
}

func TestRunSubstitutesPortPlaceholder(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	t.Setenv("FIXTURE", "arg")
	script := fmt.Sprintf(`FIXTURE_PORT_ARG={port} exec %s`, os.Args[0])
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, "test", []string{"run", "app", "--", "sh", "-c", script}, &bytes.Buffer{}, stderr)
	}()
	waitAddr(t, stderr)
	cancel()
	<-done
}

func TestRunPinsPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stderr := &syncBuffer{}
	t.Setenv("FIXTURE", "env")
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, "test", []string{"run", "app", "--port", strconv.Itoa(port), "--", os.Args[0]}, &bytes.Buffer{}, stderr)
	}()
	if got, want := waitAddr(t, stderr), "127.0.0.1:"+strconv.Itoa(port); got != want {
		t.Fatalf("addr %s, want %s", got, want)
	}
	cancel()
	<-done
}

// moduleDir survives t.Chdir in tests that build the binary.
var moduleDir string

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "switchport")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/Uncool-dudes/switchport/cmd/switchport")
	cmd.Dir = moduleDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return bin
}

func underPTY(t *testing.T, delay, keys string, argv ...string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed to drive a pty")
	}
	args := append([]string{filepath.Join(moduleDir, "testdata/pty_run.py"), delay, keys}, argv...)
	out, err := exec.Command("python3", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

func TestRunGivesChildTheTerminal(t *testing.T) {
	out := underPTY(t, "0.5", `hello\n`, buildBinary(t), "run", "app", "--", "sh", "-c", `read x; echo got:$x`)
	if !strings.Contains(out, "got:hello") || !strings.Contains(out, "EXIT 0") {
		t.Fatalf("output %q", out)
	}
}

func TestRunStopsOnCtrlC(t *testing.T) {
	out := underPTY(t, "0.5", `\x03`, buildBinary(t), "run", "app", "--", "sh", "-c", `sleep 30`)
	if strings.Contains(out, "TIMEOUT") || !strings.Contains(out, "EXIT 130") {
		t.Fatalf("output %q", out)
	}
}

func TestRunReturnsTerminalToShell(t *testing.T) {
	script := fmt.Sprintf(`%s run app -- sh -c 'exit 0'; read y; echo after:$y`, buildBinary(t))
	out := underPTY(t, "0.8", `back\n`, "sh", "-c", script)
	if !strings.Contains(out, "after:back") {
		t.Fatalf("output %q", out)
	}
}
