package trust

import "os/exec"

const systemRootsKeychain = "/System/Library/Keychains/SystemRootCertificates.keychain"

// SystemRoots returns the operating system's root certificates as PEM.
func SystemRoots() ([]byte, error) {
	return exec.Command("security", "find-certificate", "-a", "-p", systemRootsKeychain).Output()
}
