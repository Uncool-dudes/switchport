package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const nixStore = "/nix/store/"

func serviceCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return failUsage(stderr, errors.New("service takes install or uninstall"))
	}
	switch args[0] {
	case "install":
		path, err := installService()
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, "installed", path)
		return 0
	case "uninstall":
		if err := removeService(); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	return failUsage(stderr, fmt.Errorf("unknown service command %q", args[0]))
}

func installService() (string, error) {
	path, err := servicePath()
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	onPath, _ := exec.LookPath("switchport") // not on PATH leaves exe as it is
	def := serviceDefinition(stableExecutable(exe, onPath), filepath.Join(dir, "daemon.log"))
	if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
		return "", err
	}
	return path, loadService(path)
}

// stableExecutable keeps a service off a Nix store path, which stops existing
// when an upgrade is garbage collected. A profile path on PATH survives that.
func stableExecutable(exe, onPath string) string {
	if strings.HasPrefix(exe, nixStore) && onPath != "" && !strings.HasPrefix(onPath, nixStore) {
		return onPath
	}
	return exe
}

// removeService succeeds when there is nothing to remove.
func removeService() error {
	path, err := servicePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// Unloading fails harmlessly if the service was never loaded.
	_ = unloadService()
	return os.Remove(path)
}
