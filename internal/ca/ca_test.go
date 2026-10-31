package ca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func load(t *testing.T) (*CA, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := Load(dir, []string{"localhost", "test"})
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

func leafFor(t *testing.T, c *CA, name string) *tls.Certificate {
	t.Helper()
	leaf, err := c.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestPersistsAcrossLoads(t *testing.T) {
	first, dir := load(t)
	second, err := Load(dir, []string{"localhost", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Root().Equal(second.Root()) {
		t.Fatal("a second load made a new CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(first.Root())
	leaf := leafFor(t, second, "api.localhost")
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.localhost"}); err != nil {
		t.Fatal(err)
	}
}

func TestKeyIsOwnerOnly(t *testing.T) {
	_, dir := load(t)
	info, err := os.Stat(filepath.Join(dir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}

func TestRefusesKeyReadableByOthers(t *testing.T) {
	_, dir := load(t)
	os.Chmod(filepath.Join(dir, keyFile), 0o644)
	_, err := Load(dir, []string{"localhost"})
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentHandshakesMintOnce(t *testing.T) {
	c, _ := load(t)
	var wg sync.WaitGroup
	got := make([]*tls.Certificate, 32)
	for i := range got {
		wg.Go(func() {
			got[i], _ = c.GetCertificate(&tls.ClientHelloInfo{ServerName: "API.localhost"})
		})
	}
	wg.Wait()
	for _, leaf := range got {
		if leaf == nil || leaf != got[0] {
			t.Fatal("minted more than once")
		}
	}
}

func TestRefusesForeignNames(t *testing.T) {
	c, _ := load(t)
	for _, name := range []string{"example.com", "localhost", "localhost.evil", ""} {
		if _, err := c.GetCertificate(&tls.ClientHelloInfo{ServerName: name}); err == nil {
			t.Errorf("minted %q", name)
		}
	}
}

func TestNameConstraintsHoldInGoVerifier(t *testing.T) {
	c, _ := load(t)
	roots := x509.NewCertPool()
	roots.AddCert(c.Root())
	foreign, err := c.mint("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.example.com"}); err == nil {
		t.Fatal("constrained CA signed a foreign name")
	}
}

// Asks macOS itself, with no keychain change.
func TestMacOSAcceptsConstrainedCA(t *testing.T) {
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("macOS only")
	}
	c, _ := load(t)
	dir := t.TempDir()
	leafPath := filepath.Join(dir, "leaf.pem")
	for name, want := range map[string]bool{"a.localhost": true, "a.b.test": true, "api.example.com": false} {
		leaf, err := c.mint(name)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(leafPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]}), 0o600)
		err = exec.Command("security", "verify-cert", "-c", leafPath, "-r", c.Path(), "-p", "ssl", "-s", name, "-L", "-n").Run()
		if (err == nil) != want {
			t.Errorf("%s: pass=%v, want %v", name, err == nil, want)
		}
	}
}
