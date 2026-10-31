// Package trust adds the local CA to the operating system's trust store.
package trust

import (
	"crypto/x509"
	"encoding/pem"
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

var errNoStore = errors.New("no supported CA store found")

func detect() (store, error) {
	for _, s := range stores {
		if _, err := os.Stat(s.dir); err != nil {
			continue
		}
		if _, err := exec.LookPath(s.refresh); err == nil {
			return s, nil
		}
	}
	return store{}, errNoStore
}

// Install copies the certificate into the system store and refreshes it. Each
// privileged command is printed first.
func Install(certPath string) error {
	s, err := detect()
	if errors.Is(err, errNoStore) {
		return fmt.Errorf("%w, add %s to your system trust yourself", err, certPath)
	}
	if err != nil {
		return err
	}
	if err := privileged("install", "-m", "0644", certPath, filepath.Join(s.dir, installedName)); err != nil {
		return err
	}
	return privileged(s.refresh)
}

// Remove undoes Install. Without a known store Install cannot have run, so
// there is nothing to undo: NixOS, for one, declares trust in its configuration.
func Remove(string) error {
	s, err := detect()
	if errors.Is(err, errNoStore) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := privileged("rm", "-f", filepath.Join(s.dir, installedName)); err != nil {
		return err
	}
	return privileged(s.refresh)
}

// Installed reports whether the system trusts this certificate. It asks the
// system pool, not the directory Install writes to, because trust can also come
// from elsewhere, such as security.pki.certificateFiles on NixOS.
func Installed(certPath string) bool {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return false
	}
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool})
	return err == nil
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
