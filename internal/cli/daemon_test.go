package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type endToEnd struct {
	bin, dir string
	port     int
}

func setupEndToEnd(t *testing.T) *endToEnd {
	t.Helper()
	e := &endToEnd{bin: buildBinary(t), dir: filepath.Join(t.TempDir(), "state"), port: freeTestPort(t)}
	t.Setenv(stateDirEnv, e.dir)
	t.Setenv(daemonEnv, "")
	t.Setenv(httpsPortEnv, strconv.Itoa(e.port))
	t.Cleanup(func() { exec.Command(e.bin, "daemon", "stop").Run() })
	return e
}

func freeTestPort(t *testing.T) int {
	t.Helper()
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (e *endToEnd) client(t *testing.T) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(e.dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("bad CA")
	}
	tr := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(e.port)))
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}

func (e *endToEnd) get(t *testing.T, c *http.Client, host string) (int, string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		res, err := c.Get(fmt.Sprintf("https://%s:%d/", host, e.port))
		if err == nil {
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusNotFound || !strings.Contains(string(b), "unknown_route") || time.Now().After(deadline) {
				return res.StatusCode, string(b)
			}
		} else if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestEndToEndHTTPSThroughDaemon(t *testing.T) {
	e := setupEndToEnd(t)
	cmd, _ := startWrapper(t, e.bin)
	defer stopWrapper(cmd)
	client := e.client(t)
	// The fixture answers every path with its own 404.
	if code, _ := e.get(t, client, "app.localhost"); code != http.StatusNotFound {
		t.Fatalf("status %d", code)
	}
	res, err := client.Get(fmt.Sprintf("https://app.localhost:%d/", e.port))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Fatalf("proto %s", res.Proto)
	}

	res, err = client.Get(fmt.Sprintf("https://other.localhost:%d/", e.port))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 404 || !strings.Contains(string(b), "app.localhost") {
		t.Fatalf("%d %q", res.StatusCode, b)
	}
}

func TestGetPrintsDaemonPort(t *testing.T) {
	e := setupEndToEnd(t)
	cmd, _ := startWrapper(t, e.bin)
	defer stopWrapper(cmd)
	want := fmt.Sprintf("https://app.localhost:%d\n", e.port)
	if _, out, _ := cli(t, "get", "app"); out != want {
		t.Fatalf("%q, want %q", out, want)
	}
}

func TestConcurrentRunsStartOneDaemon(t *testing.T) {
	e := setupEndToEnd(t)
	cmds := make([]*exec.Cmd, 4)
	for i := range cmds {
		cmds[i] = exec.Command(e.bin, "run", "app"+strconv.Itoa(i), "--", os.Args[0])
		cmds[i].Env = append(os.Environ(), "FIXTURE=env", "GORACE=atexit_sleep_ms=0")
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cmds[i].Process.Signal(os.Interrupt)
			cmds[i].Wait()
		})
	}
	client := e.clientAfterStart(t)
	for i := range cmds {
		if code, _ := e.get(t, client, "app"+strconv.Itoa(i)+".localhost"); code != http.StatusNotFound {
			t.Fatalf("app%d: %d", i, code)
		}
	}
	out, err := exec.Command("sh", "-c", "pgrep -f '"+e.bin+" daemon run' | wc -l").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("%s daemons", got)
	}
}

// clientAfterStart waits for the CA that a racing daemon start creates.
func (e *endToEnd) clientAfterStart(t *testing.T) *http.Client {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(e.dir, "ca-key.pem")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return e.client(t)
}

func TestRoutesSurviveDaemonRestart(t *testing.T) {
	e := setupEndToEnd(t)
	cmd, _ := startWrapper(t, e.bin)
	defer stopWrapper(cmd)
	client := e.client(t)
	e.get(t, client, "app.localhost")

	if out, err := exec.Command(e.bin, "daemon", "stop").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, _ := exec.Command(e.bin, "daemon", "status").CombinedOutput(); !strings.Contains(string(out), "not running") {
		t.Fatalf("%q", out)
	}
	if code, _, stderr := cli(t, "list"); code != 0 {
		t.Fatalf("list needs no daemon: %q", stderr)
	}

	restart := exec.Command(e.bin, "daemon", "run")
	if err := restart.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		restart.Process.Signal(os.Interrupt)
		restart.Wait()
	}()
	client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for {
		res, err := client.Get(fmt.Sprintf("https://app.localhost:%d/", e.port))
		if err == nil {
			res.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDaemonRefusesForeignSNI(t *testing.T) {
	e := setupEndToEnd(t)
	cmd, _ := startWrapper(t, e.bin)
	defer stopWrapper(cmd)
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(e.port)), &tls.Config{ServerName: "example.com", InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		t.Fatal("handshake should fail")
	}
}
