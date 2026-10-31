package trust

import (
	"errors"
	"os"
)

var rootBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
}

// SystemRoots returns the operating system's root certificates as PEM.
func SystemRoots() ([]byte, error) {
	for _, path := range rootBundles {
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	return nil, errors.New("no system root bundle found")
}
