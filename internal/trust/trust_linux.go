// Package trust adds the local CA to the operating system's trust store.
package trust

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const installedName = "switchport-ca.crt"

// store is one distribution family's trust directory and refresh command.
type store struct {
	dir     string
	refresh string
}

// stores covers Debian, Arch, Fedora, and openSUSE families, in that order.
var stores = []store{
	{"/usr/local/share/ca-certificates", "update-ca-certificates"},
	{"/etc/ca-certificates/trust-source/anchors", "update-ca-trust"},
	{"/etc/pki/ca-trust/source/anchors", "update-ca-trust"},
	{"/etc/pki/trust/anchors", "update-ca-certificates"},
}

func detect() (store, error) {
	for _, s := range stores {
		if _, err := os.Stat(s.dir); err != nil {
			continue
		}
		if _, err := exec.LookPath(s.refresh); err == nil {
			return s, nil
		}
	}
	return store{}, errors.New("no supported CA store found, add the certificate to your system trust manually")
}

// Install copies the certificate into the system store and refreshes it. Each
// privileged command is printed first.
func Install(certPath string) error {
	s, err := detect()
	if err != nil {
		return err
	}
	if err := privileged("install", "-m", "0644", certPath, filepath.Join(s.dir, installedName)); err != nil {
		return err
	}
	return privileged(s.refresh)
}

// Remove undoes Install.
func Remove(string) error {
	s, err := detect()
	if err != nil {
		return err
	}
	if err := privileged("rm", "-f", filepath.Join(s.dir, installedName)); err != nil {
		return err
	}
	return privileged(s.refresh)
}

// Installed reports whether this exact certificate is in the system store.
func Installed(certPath string) bool {
	s, err := detect()
	if err != nil {
		return false
	}
	want, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	got, err := os.ReadFile(filepath.Join(s.dir, installedName))
	return err == nil && bytes.Equal(want, got)
}

func privileged(args ...string) error {
	if os.Geteuid() != 0 {
		args = append([]string{"sudo"}, args...)
	}
	fmt.Fprintln(os.Stderr, "+", args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
