package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/Uncool-dudes/switchport/internal/ca"
	"github.com/Uncool-dudes/switchport/internal/route"
	"github.com/Uncool-dudes/switchport/internal/trust"
)

const (
	configFile = "config.json"
	bundleFile = "ca-bundle.pem"
	caFile     = "ca.pem"
	fileMode   = 0o600
)

type config struct {
	TLDs []string `json:"tlds"` // the first one is used in printed URLs
}

func loadConfig() config {
	cfg := config{TLDs: []string{defaultTLD}}
	dir, err := stateDir()
	if err != nil {
		return cfg
	}
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		return cfg
	}
	var stored config
	if json.Unmarshal(data, &stored) != nil || len(stored.TLDs) == 0 {
		return cfg
	}
	return stored
}

func tlds() []string { return loadConfig().TLDs }

// addTLD makes tld the default, keeping localhost as a fallback.
func addTLD(tld string) error {
	if _, err := route.NewName(tld); err != nil {
		return err
	}
	cfg := loadConfig()
	cfg.TLDs = append([]string{tld}, slices.DeleteFunc(cfg.TLDs, func(t string) bool { return t == tld })...)
	dir, err := stateDir()
	if err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, configFile), data, fileMode)
}

// certPaths returns the files a child needs to trust our CA, or nothing before
// the CA exists.
func certPaths() certFiles {
	dir, err := stateDir()
	if err != nil {
		return certFiles{}
	}
	caPath := filepath.Join(dir, caFile)
	if _, err := os.Stat(caPath); err != nil {
		return certFiles{}
	}
	bundle, err := ensureBundle(dir, caPath)
	if err != nil {
		return certFiles{ca: caPath}
	}
	return certFiles{ca: caPath, bundle: bundle}
}

func ensureBundle(dir, caPath string) (string, error) {
	path := filepath.Join(dir, bundleFile)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	roots, err := trust.SystemRoots()
	if err != nil {
		return "", err
	}
	own, err := os.ReadFile(caPath)
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(roots, own...), fileMode)
}

func loadCA() (*ca.CA, error) {
	if _, err := openRegistry(); err != nil {
		return nil, err
	}
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	return ca.Load(dir, tlds())
}
