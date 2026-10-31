package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Uncool-dudes/switchport/internal/route"
)

func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(t.Context(), "test", args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func freshState(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv(stateDirEnv, dir)
	return dir
}

func TestListJSONIsStable(t *testing.T) {
	freshState(t)
	reg, err := openRegistry()
	if err != nil {
		t.Fatal(err)
	}
	api, _ := route.NewName("api")
	pid := os.Getpid()
	if err := reg.AddApp(route.App{Name: api, Addr: "[::1]:5173", Worktree: "feat-x", PID: pid}, false); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := cli(t, "alias", "db", "5432"); code != 0 {
		t.Fatal(code)
	}
	code, out, _ := cli(t, "list", "--json")
	want := fmt.Sprintf(`{
  "version": 1,
  "routes": [
    {
      "name": "api",
      "url": "https://api.localhost",
      "addr": "[::1]:5173",
      "type": "app",
      "pid": %d,
      "worktree": "feat-x"
    },
    {
      "name": "db",
      "url": "https://db.localhost",
      "addr": "127.0.0.1:5432",
      "type": "alias"
    }
  ]
}
`, pid)
	if code != 0 || out != want {
		t.Fatalf("code %d, got:\n%s", code, out)
	}
}

func TestListJSONEmptyIsAnArray(t *testing.T) {
	freshState(t)
	_, out, _ := cli(t, "list", "--json")
	var v struct{ Routes []any }
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Routes == nil {
		t.Fatalf("%q, %v", out, err)
	}
}

func TestListTable(t *testing.T) {
	freshState(t)
	cli(t, "alias", "db", "5432")
	_, out, _ := cli(t, "list")
	if !regexp.MustCompile(`db\s+https://db\.localhost\s+127\.0\.0\.1:5432\s+-\s+alias\s+-`).MatchString(out) {
		t.Fatalf("%q", out)
	}
}

func TestAliasLifecycle(t *testing.T) {
	freshState(t)
	if code, out, _ := cli(t, "alias", "db", "5432"); code != 0 || out != "https://db.localhost\n" {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, stderr := cli(t, "alias", "db", "5433"); code != 1 || !strings.Contains(stderr, "--force") {
		t.Fatalf("%d %q", code, stderr)
	}
	if code, _, _ := cli(t, "alias", "db", "5433", "--force"); code != 0 {
		t.Fatal(code)
	}
	if _, out, _ := cli(t, "list"); !strings.Contains(out, "5433") {
		t.Fatalf("%q", out)
	}
	if code, _, _ := cli(t, "alias", "--remove", "db"); code != 0 {
		t.Fatal(code)
	}
	if code, _, _ := cli(t, "get", "db"); code != 1 {
		t.Fatal(code)
	}
}

func TestAliasRejectsBadInput(t *testing.T) {
	freshState(t)
	for _, args := range [][]string{{"alias", "db"}, {"alias", "db", "x"}, {"alias", "db", "0"}, {"alias", "db", "70000"}, {"alias", "--remove"}} {
		if code, _, _ := cli(t, args...); code != exitUsage {
			t.Errorf("%v: code %d", args, code)
		}
	}
	if code, _, _ := cli(t, "alias", "bad name", "1"); code != 1 {
		t.Errorf("code %d", code)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGetUsesWorktreeThenFallsBack(t *testing.T) {
	freshState(t)
	main := t.TempDir()
	gitIn(t, main, "init", "-q", "-b", "main")
	gitIn(t, main, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x")
	linked := filepath.Join(t.TempDir(), "wt")
	gitIn(t, main, "worktree", "add", "-q", "-b", "fix-ui", linked)
	t.Chdir(linked)

	cli(t, "alias", "api", "1000")
	code, out, stderr := cli(t, "get", "api")
	if code != 0 || out != "https://api.localhost\n" || !strings.Contains(stderr, "main checkout") {
		t.Fatalf("fallback: %d %q %q", code, out, stderr)
	}
	cli(t, "alias", "fix-ui.api", "1001")
	if code, out, stderr = cli(t, "get", "api"); code != 0 || out != "https://fix-ui.api.localhost\n" || stderr != "" {
		t.Fatalf("worktree: %d %q %q", code, out, stderr)
	}
	if _, out, _ = cli(t, "get", "api", "--no-worktree"); out != "https://api.localhost\n" {
		t.Fatalf("no-worktree: %q", out)
	}
}

func TestGetWaitsForRoute(t *testing.T) {
	freshState(t)
	go func() {
		time.Sleep(200 * time.Millisecond)
		reg, err := openRegistry()
		if err != nil {
			t.Error(err)
			return
		}
		name, _ := route.NewName("late")
		reg.AddAlias(route.Alias{Name: name, Addr: "127.0.0.1:1"}, false)
	}()
	if code, out, stderr := cli(t, "get", "late", "--wait", "3s"); code != 0 || out != "https://late.localhost\n" {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	if code, _, _ := cli(t, "get", "never", "--wait", "150ms"); code != 1 {
		t.Fatalf("code %d", code)
	}
}

func TestSecondRunWithSameNameFails(t *testing.T) {
	freshState(t)
	bin := buildBinary(t)
	cmd, addr := startWrapper(t, bin)
	defer stopWrapper(cmd)

	dup := exec.Command(bin, "run", "app", "--", os.Args[0])
	dup.Env = append(os.Environ(), "FIXTURE=env")
	out, err := dup.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "--force") {
		t.Fatalf("%v: %s", err, out)
	}
	if !dialable(addr) {
		t.Fatal("first app stopped")
	}
}

func TestForceTakesNameAndLastOwnerCleansUp(t *testing.T) {
	freshState(t)
	bin := buildBinary(t)
	first, _ := startWrapper(t, bin)
	defer stopWrapper(first)
	second, addr := startWrapper(t, bin, "--force")
	if _, out, _ := cli(t, "list"); !strings.Contains(out, addr) {
		t.Fatalf("%q does not list %s", out, addr)
	}
	stopWrapper(first)
	if _, out, _ := cli(t, "list"); !strings.Contains(out, addr) {
		t.Fatalf("old wrapper removed the new route: %q", out)
	}
	stopWrapper(second)
	if _, out, _ := cli(t, "list"); strings.Contains(out, addr) {
		t.Fatalf("route left behind: %q", out)
	}
}

// startWrapper runs the real binary around a fixture server and returns once
// its route exists.
func startWrapper(t *testing.T, bin string, flags ...string) (*exec.Cmd, string) {
	t.Helper()
	args := append([]string{"run", "app"}, flags...)
	cmd := exec.Command(bin, append(args, "--", os.Args[0])...)
	cmd.Env = append(os.Environ(), "FIXTURE=env", "GORACE=atexit_sleep_ms=0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	re := regexp.MustCompile(`\((127\.0\.0\.1:\d+)\)`)
	for {
		select {
		case l := <-lines:
			if m := re.FindStringSubmatch(l); m != nil {
				return cmd, m[1]
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no route line from wrapper")
		}
	}
}

func stopWrapper(cmd *exec.Cmd) {
	if cmd.ProcessState != nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	cmd.Wait()
}

func dialable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestKilledWrapperClearsRouteAndPruneStopsServer(t *testing.T) {
	freshState(t)
	bin := buildBinary(t)
	cmd, addr := startWrapper(t, bin)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()

	if _, out, _ := cli(t, "list"); strings.Contains(out, addr) {
		t.Fatalf("stale route listed: %q", out)
	}
	if !dialable(addr) {
		t.Fatal("server should outlive a killed wrapper until prune")
	}
	code, out, _ := cli(t, "prune")
	if code != 0 || !strings.Contains(out, "pruned app") {
		t.Fatalf("%d %q", code, out)
	}
	deadline := time.Now().Add(3 * time.Second)
	for dialable(addr) {
		if time.Now().After(deadline) {
			t.Fatal("server survived prune")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, out, _ := cli(t, "prune"); out != "" {
		t.Fatalf("second prune: %q", out)
	}
}

func TestRunAddsWorktreePrefix(t *testing.T) {
	freshState(t)
	bin, err := filepath.Abs(buildBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	main := t.TempDir()
	gitIn(t, main, "init", "-q", "-b", "main")
	gitIn(t, main, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x")
	linked := filepath.Join(t.TempDir(), "wt")
	gitIn(t, main, "worktree", "add", "-q", "-b", "fix-ui", linked)
	t.Chdir(linked)
	cmd, _ := startWrapper(t, bin)
	defer stopWrapper(cmd)
	if _, out, _ := cli(t, "get", "app"); out != "https://fix-ui.app.localhost\n" {
		t.Fatalf("%q", out)
	}
}
