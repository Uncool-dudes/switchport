// Package worktree derives a route prefix from a linked git worktree.
package worktree

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const maxLabel = 63

var invalid = regexp.MustCompile(`[^a-z0-9]+`)

// Prefix returns a DNS label for dir's branch if dir is a linked worktree, and
// an empty string for a main checkout or anywhere outside git.
func Prefix(ctx context.Context, dir string) string {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	f := strings.Fields(out)
	if len(f) != 3 || filepath.Clean(f[0]) == filepath.Clean(f[1]) {
		return ""
	}
	if f[2] != "HEAD" {
		return Label(f[2])
	}
	// A detached worktree has no branch, so its directory is the stable name.
	return Label(filepath.Base(dir))
}

// Label turns a branch name into a valid DNS label.
func Label(branch string) string {
	label := invalid.ReplaceAllString(strings.ToLower(branch), "-")
	label = strings.Trim(label, "-")
	if len(label) > maxLabel {
		label = strings.TrimRight(label[:maxLabel], "-")
	}
	return label
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
