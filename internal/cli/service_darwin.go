package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

const serviceLabel = "dev.switchport.daemon"

func servicePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
}

func serviceDefinition(exe, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>daemon</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, serviceLabel, exe, logPath, logPath)
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func loadService(path string) error {
	return exec.Command("launchctl", "bootstrap", domain(), path).Run()
}

func unloadService() error {
	return exec.Command("launchctl", "bootout", domain()+"/"+serviceLabel).Run()
}
