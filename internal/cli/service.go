package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

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
	def := serviceDefinition(exe, filepath.Join(dir, "daemon.log"))
	if err := os.WriteFile(path, []byte(def), 0o644); err != nil {
		return "", err
	}
	return path, loadService(path)
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
