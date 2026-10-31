package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Uncool-dudes/switchport/internal/route"
)

// TestMain turns the test binary into a concurrent writer when WRITER is set.
func TestMain(m *testing.M) {
	switch os.Getenv("WRITER") {
	case "once":
		r, err := Open(os.Getenv("DIR"))
		if err != nil {
			os.Exit(2)
		}
		if err := r.AddAlias(alias(os.Getenv("NAME"), "127.0.0.1:1"), false); err != nil {
			os.Exit(3)
		}
		return
	case "forever":
		r, err := Open(os.Getenv("DIR"))
		if err != nil {
			os.Exit(2)
		}
		for i := 0; ; i++ {
			if err := r.AddAlias(alias(fmt.Sprintf("n%d", i%40), "127.0.0.1:1"), true); err != nil {
				os.Exit(3)
			}
		}
	}
	os.Exit(m.Run())
}

func name(s string) route.Name {
	n, err := route.NewName(s)
	if err != nil {
		panic(err)
	}
	return n
}

func alias(s, addr string) route.Alias { return route.Alias{Name: name(s), Addr: addr} }

func app(s string, pid int) route.App {
	return route.App{Name: name(s), Addr: "127.0.0.1:1", PID: pid}
}

func open(t *testing.T) (*Registry, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

// deadPID returns a pid that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestStateIsPrivate(t *testing.T) {
	r, dir := open(t)
	if err := r.AddApp(app("api", os.Getpid()), false); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, routesFile): 0o600, filepath.Join(dir, lockFile): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode %o, want %o", path, got, want)
		}
	}
}

func TestEmptyRegistryLists(t *testing.T) {
	r, _ := open(t)
	got, err := r.List()
	if err != nil || len(got.Apps)+len(got.Aliases) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestAliasSurvivesReopen(t *testing.T) {
	r, dir := open(t)
	if err := r.AddAlias(alias("db", "127.0.0.1:5432"), false); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.List()
	if err != nil || len(got.Aliases) != 1 || got.Aliases[0].Addr != "127.0.0.1:5432" {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestNameConflict(t *testing.T) {
	r, _ := open(t)
	me := os.Getpid()
	if err := r.AddApp(app("api", os.Getppid()), false); err != nil {
		t.Fatal(err)
	}
	var taken *NameTakenError
	if err := r.AddApp(app("api", me), false); !errors.As(err, &taken) || taken.PID != os.Getppid() {
		t.Fatalf("err = %v", err)
	}
	if err := r.Available(name("api")); !errors.As(err, &taken) {
		t.Fatalf("Available = %v", err)
	}
	if err := r.AddAlias(alias("api", "127.0.0.1:2"), false); !errors.As(err, &taken) {
		t.Fatalf("alias err = %v", err)
	}
	if err := r.AddApp(app("api", me), true); err != nil {
		t.Fatal(err)
	}
	got, _ := r.List()
	if len(got.Apps) != 1 || got.Apps[0].PID != me {
		t.Fatalf("%+v", got)
	}
}

func TestDeadOwnerFreesName(t *testing.T) {
	r, _ := open(t)
	if err := r.AddApp(app("api", deadPID(t)), false); err != nil {
		t.Fatal(err)
	}
	if err := r.Available(name("api")); err != nil {
		t.Fatal(err)
	}
	if err := r.AddApp(app("api", os.Getpid()), false); err != nil {
		t.Fatal(err)
	}
}

func TestReplacedWrapperCannotRemoveSuccessor(t *testing.T) {
	r, _ := open(t)
	me := os.Getpid()
	if err := r.AddApp(app("api", me), false); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveApp(name("api"), me+1); err != nil {
		t.Fatal(err)
	}
	got, _ := r.List()
	if len(got.Apps) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestDeadWrapperLeavesListButStaysForPrune(t *testing.T) {
	r, _ := open(t)
	if err := r.AddApp(app("api", deadPID(t)), false); err != nil {
		t.Fatal(err)
	}
	got, _ := r.List()
	if len(got.Apps) != 0 {
		t.Fatalf("listed %+v", got)
	}
	stale, err := r.Stale()
	if err != nil || len(stale) != 1 {
		t.Fatalf("stale %+v, %v", stale, err)
	}
	if err := r.RemoveStale(); err != nil {
		t.Fatal(err)
	}
	if stale, _ = r.Stale(); len(stale) != 0 {
		t.Fatalf("stale %+v", stale)
	}
}

func TestConcurrentProcessesLoseNoRoutes(t *testing.T) {
	r, dir := open(t)
	const writers = 50
	cmds := make([]*exec.Cmd, writers)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0])
		cmds[i].Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "WRITER=once", "DIR="+dir, "NAME=a"+strconv.Itoa(i))
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != writers {
		t.Fatalf("%d routes, want %d", len(got.Aliases), writers)
	}
}

// Go documents neither rename atomicity nor flock release on death, so this
// proves what the design relies on.
func TestWriterKilledMidWriteLeavesValidFileAndFreeLock(t *testing.T) {
	r, dir := open(t)
	for range 25 {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "WRITER=forever", "DIR="+dir)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()

		if _, err := r.List(); err != nil {
			t.Fatalf("file damaged: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- r.AddAlias(alias("probe", "127.0.0.1:2"), true) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("lock still held after its holder was killed")
		}
	}
}

func TestRejectsNewerSchema(t *testing.T) {
	r, dir := open(t)
	os.WriteFile(filepath.Join(dir, routesFile), []byte(`{"version":99}`), 0o600)
	if _, err := r.List(); err == nil {
		t.Fatal("want error")
	}
}

func TestRejectsInvalidStoredName(t *testing.T) {
	r, dir := open(t)
	data, _ := json.Marshal(map[string]any{"version": 1, "aliases": []map[string]string{{"name": "bad name", "addr": "x"}}})
	os.WriteFile(filepath.Join(dir, routesFile), data, 0o600)
	if _, err := r.List(); err == nil {
		t.Fatal("want error")
	}
}
