package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const serviceUnit = "switchport.service"

func servicePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", serviceUnit), nil
}

func serviceDefinition(exe, _ string) string {
	return fmt.Sprintf(`[Unit]
Description=switchport daemon

[Service]
ExecStart=%s daemon run
Restart=always

[Install]
WantedBy=default.target
`, exe)
}

func loadService(string) error {
	return exec.Command("systemctl", "--user", "enable", "--now", serviceUnit).Run()
}

func unloadService() error {
	return exec.Command("systemctl", "--user", "disable", "--now", serviceUnit).Run()
}
