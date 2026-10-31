package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x")
	return dir
}

func TestPrefix(t *testing.T) {
	main := repo(t)
	if got := Prefix(t.Context(), main); got != "" {
		t.Errorf("main checkout: %q", got)
	}
	if got := Prefix(t.Context(), t.TempDir()); got != "" {
		t.Errorf("outside git: %q", got)
	}
	linked := filepath.Join(t.TempDir(), "wt")
	run(t, main, "worktree", "add", "-q", "-b", "ananth/Fix_UI", linked)
	if got := Prefix(t.Context(), linked); got != "ananth-fix-ui" {
		t.Errorf("linked: %q", got)
	}
	sub := filepath.Join(linked, "cmd")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Prefix(t.Context(), sub); got != "ananth-fix-ui" {
		t.Errorf("subdirectory: %q", got)
	}
	detached := filepath.Join(t.TempDir(), "Review")
	run(t, main, "worktree", "add", "-q", "--detach", detached)
	if got := Prefix(t.Context(), detached); got != "review" {
		t.Errorf("detached: %q", got)
	}
}

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"main":                         "main",
		"feat/x":                       "feat-x",
		"--a--b--":                     "a-b",
		"UPPER_case.1":                 "upper-case-1",
		strings.Repeat("a", 62) + "-b": strings.Repeat("a", 62),
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
	}
}
