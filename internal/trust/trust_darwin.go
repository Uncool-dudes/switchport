// Package trust adds the local CA to the operating system's trust store.
package trust

import (
	"crypto/sha1"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func keychain() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Keychains", "login.keychain-db"), nil
}

// Install trusts the certificate in the login keychain. macOS shows its own
// prompt, so this needs no root.
func Install(certPath string) error {
	kc, err := keychain()
	if err != nil {
		return err
	}
	return run("security", "add-trusted-cert", "-r", "trustRoot", "-p", "ssl", "-k", kc, certPath)
}

// Remove undoes Install. It deletes by fingerprint, so a certificate that only
// shares the name is left alone.
func Remove(certPath string) error {
	if err := run("security", "remove-trusted-cert", certPath); err != nil {
		return err
	}
	fingerprint, err := sha1Hex(certPath)
	if err != nil {
		return err
	}
	kc, err := keychain()
	if err != nil {
		return err
	}
	return run("security", "delete-certificate", "-Z", fingerprint, kc)
}

// Installed reports whether macOS trusts the certificate for TLS.
func Installed(certPath string) bool {
	return exec.Command("security", "verify-cert", "-c", certPath, "-L", "-p", "ssl").Run() == nil
}

// sha1Hex is the fingerprint form the security tool identifies certificates by.
func sha1Hex(certPath string) (string, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("%s is not a PEM certificate", certPath)
	}
	return fmt.Sprintf("%X", sha1.Sum(block.Bytes)), nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
