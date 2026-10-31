package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type spawned struct {
	cmd    *exec.Cmd
	tr     *Tracker
	log    string
	exited chan error
}

func spawn(t *testing.T, dir string, env []string, name string, args ...string) *spawned {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logPath := filepath.Join(t.TempDir(), "child.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &spawned{cmd: cmd, tr: NewTracker(cmd.Process.Pid), log: logPath, exited: make(chan error, 1)}
	go func() { s.exited <- cmd.Wait() }()
	t.Cleanup(func() { s.tr.Signal(syscall.SIGKILL) })
	return s
}

func have(t *testing.T, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skip("missing " + b)
		}
	}
}

const pyHTTP = `
import http.server, socket, sys, threading
class S(http.server.BaseHTTPRequestHandler):
    def do_HEAD(self): self.send_response(200); self.end_headers()
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(sys.argv[2].encode())
    def log_message(self, *a): pass
def serve(fam, host, port):
    class V(http.server.ThreadingHTTPServer): address_family = fam
    V((host, port), S).serve_forever()
`

func TestDiscoverScenarios(t *testing.T) {
	have(t, "python3")
	dir := t.TempDir()
	py := filepath.Join(dir, "srv.py")
	body := pyHTTP + `
mode, tag = sys.argv[1], sys.argv[2]
import os, time
a = sys.argv[3:]
if mode == "fixed4": serve(socket.AF_INET, a[0], int(a[1]))
if mode == "fixed6": serve(socket.AF_INET6, a[0], int(a[1]))
if mode == "env": serve(socket.AF_INET, "127.0.0.1", int(os.environ["PORT"]))
if mode == "random":
    serve(socket.AF_INET, "127.0.0.1", 0)
if mode == "raw_then_http":
    r = socket.socket(); r.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); r.bind(("127.0.0.1", int(a[0]))); r.listen()
    threading.Thread(target=lambda: [r.accept()[0].close() for _ in iter(int, 1)], daemon=True).start()
    time.sleep(0.15)
    serve(socket.AF_INET, "127.0.0.1", int(a[1]))
if mode == "two_http":
    threading.Thread(target=serve, args=(socket.AF_INET, "127.0.0.1", int(a[0])), daemon=True).start()
    serve(socket.AF_INET, "127.0.0.1", int(a[1]))
if mode == "raw_only":
    r = socket.socket(); r.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); r.bind(("127.0.0.1", int(a[0]))); r.listen()
    while True: r.accept()[0].close()
if mode == "daemon":
    if os.fork(): os._exit(0)
    os.setsid()
    if os.fork(): os._exit(0)
    open(a[1], "w").write(str(os.getpid()))
    serve(socket.AF_INET, "127.0.0.1", int(a[0]))
`
	os.WriteFile(py, []byte(body), 0o644)

	goDir := filepath.Join(dir, "gosrv")
	os.MkdirAll(goDir, 0o755)
	os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module gosrv\n\ngo 1.27\n"), 0o644)
	os.WriteFile(filepath.Join(goDir, "main.go"), []byte(`package main
import ("fmt";"net/http")
func main(){ http.ListenAndServe("127.0.0.1:18707", http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){fmt.Fprint(w,"go-run")})) }
`), 0o644)

	sh := func(s string) []string { return []string{"sh", "-c", s} }
	pyc := func(a ...string) []string { return append([]string{"python3", py}, a...) }

	cases := []struct {
		name     string
		cmd      []string
		dir      string
		honors   bool
		wantBody string
		wantErr  error
		settle   time.Duration
		timeout  time.Duration
		note     string
	}{
		{name: "honors PORT", cmd: pyc("env", "env-ok"), honors: true, wantBody: "env-ok"},
		{name: "fixed 127.0.0.1", cmd: pyc("fixed4", "v4", "127.0.0.1", "18701"), wantBody: "v4"},
		{name: "wildcard 0.0.0.0", cmd: pyc("fixed4", "any", "0.0.0.0", "18702"), wantBody: "any"},
		{name: "::1 only", cmd: pyc("fixed6", "v6", "::1", "18703"), wantBody: "v6"},
		{name: "random port", cmd: pyc("random", "rnd"), wantBody: "rnd"},
		{name: "go run grandchild", cmd: []string{"go", "run", "."}, dir: goDir, wantBody: "go-run", timeout: 60 * time.Second},
		{name: "shell pipeline", cmd: sh("python3 " + py + " fixed4 piped 127.0.0.1 18708 | cat"), wantBody: "piped"},
		{name: "starts after 2s", cmd: sh("sleep 2; exec python3 " + py + " fixed4 late 127.0.0.1 18709"), wantBody: "late"},
		{name: "raw port before HTTP", cmd: pyc("raw_then_http", "app", "18710", "18711"), wantBody: "app"},
		{name: "two HTTP ports", cmd: pyc("two_http", "dual", "18712", "18713"), wantBody: "dual", note: "ambiguous"},
		{name: "exits at once", cmd: sh("exit 3"), wantErr: ErrChildExited},
		{name: "never listens", cmd: sh("sleep 30"), wantErr: ErrNoListener, timeout: 1500 * time.Millisecond},
		{name: "non-HTTP only", cmd: pyc("raw_only", "x", "18714"), note: "routes with HTTP=false"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assigned := freePort(t)
			env := []string{"PORT=" + strconv.Itoa(assigned)}
			if !c.honors {
				env = nil
			}
			cdir := c.dir
			if cdir == "" {
				cdir = dir
			}
			s := spawn(t, cdir, env, c.cmd[0], c.cmd[1:]...)
			o := DiscoverOpts{Assigned: assigned, Settle: 300 * time.Millisecond, Timeout: 8 * time.Second, Exited: s.exited}
			if c.settle > 0 {
				o.Settle = c.settle
			}
			if c.timeout > 0 {
				o.Timeout = c.timeout
			}
			d, err := Discover(context.Background(), s.tr, o)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v", err)
				}
				t.Logf("RESULT %-22s -> %v", c.name, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("RESULT %-22s -> %-16s http=%-5v %6dms polls=%d candidates=%d %s", c.name, d.Addr(), d.HTTP, d.Elapsed.Milliseconds(), d.Polls, len(d.Candidates), c.note)
			if c.wantBody == "" {
				return
			}
			res, err := http.Get("http://" + d.Addr() + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			if string(b) != c.wantBody {
				t.Fatalf("body %q", b)
			}
			s.tr.Signal(syscall.SIGTERM)
			select {
			case <-s.exited:
			case <-time.After(2 * time.Second):
				t.Fatal("group did not exit")
			}
			time.Sleep(100 * time.Millisecond)
			if conn, err := net.DialTimeout("tcp", d.Addr(), 200*time.Millisecond); err == nil {
				conn.Close()
				t.Fatalf("%s still listening after group kill", d.Addr())
			}
		})
	}

	t.Run("port already taken", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:18715")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		s := spawn(t, dir, nil, "python3", py, "fixed4", "x", "127.0.0.1", "18715")
		_, err = Discover(context.Background(), s.tr, DiscoverOpts{Assigned: 1, Settle: 100 * time.Millisecond, Timeout: 5 * time.Second, Exited: s.exited})
		if !errors.Is(err, ErrChildExited) {
			t.Fatalf("err = %v", err)
		}
		t.Logf("RESULT %-22s -> %v", "port already taken", err)
	})

	t.Run("daemonizes out of group", func(t *testing.T) {
		pidFile := filepath.Join(dir, "daemon.pid")
		s := spawn(t, dir, nil, "python3", py, "daemon", "d", "18716", pidFile)
		t.Cleanup(func() {
			if b, err := os.ReadFile(pidFile); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		})
		_, err := Discover(context.Background(), s.tr, DiscoverOpts{Assigned: 1, Settle: 100 * time.Millisecond, Timeout: 3 * time.Second, Exited: s.exited})
		t.Logf("RESULT %-22s -> %v (known limit)", "daemonizes out of group", err)
		if err == nil {
			t.Fatal("expected a miss")
		}
	})
}

func TestListenersCost(t *testing.T) {
	s := spawn(t, t.TempDir(), nil, "sleep", "30")
	start := time.Now()
	const n = 20
	for range n {
		if _, err := s.tr.Listeners(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("RESULT Listeners() cost %v per call", time.Since(start)/n)
}

var _ = fmt.Sprint
