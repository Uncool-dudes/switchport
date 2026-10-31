// Package ca is the local certificate authority behind switchport's HTTPS.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	certFile   = "ca.pem"
	keyFile    = "ca-key.pem"
	lockFile   = "ca.lock"
	commonName = "switchport local CA"
	serialBits = 128
	caYears    = 10
	leafDays   = 90
	keyMode    = 0o600
	backdate   = time.Hour
)

// CA signs a certificate per hostname. Its private key stays on disk with
// owner-only access; leaf keys only ever live in memory, so there is nothing to
// renew: a restart mints new ones.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	path string
	tlds []string

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// Load reads the CA in dir, creating it on first use. tlds become X.509 name
// constraints, so a leaked key cannot sign for real domains.
func Load(dir string, tlds []string) (*CA, error) {
	lock, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, keyMode)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock CA: %w", err)
	}

	certPath, keyPath := filepath.Join(dir, certFile), filepath.Join(dir, keyFile)
	info, err := os.Stat(keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		if err := create(certPath, keyPath, tlds); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if info.Mode().Perm()&^keyMode != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %o), run chmod 600 on it", keyPath, info.Mode().Perm())
	}

	cert, key, err := read(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	for _, tld := range tlds {
		if !slices.Contains(cert.PermittedDNSDomains, tld) {
			return nil, fmt.Errorf("the CA cannot sign for .%s, run switchport uninstall and then setup with this TLD", tld)
		}
	}
	return &CA{cert: cert, key: key, path: certPath, tlds: tlds, leaves: map[string]*tls.Certificate{}}, nil
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), serialBits))
}

func create(certPath, keyPath string, tlds []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	sn, err := serial()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:                sn,
		Subject:                     pkix.Name{CommonName: commonName},
		NotBefore:                   now.Add(-backdate),
		NotAfter:                    now.AddDate(caYears, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomains:         tlds,
		PermittedDNSDomainsCritical: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The key goes last, because its presence is what marks the CA as complete.
	if err := writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return err
	}
	return writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func read(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, fmt.Errorf("damaged CA files in %s", filepath.Dir(certPath))
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected CA key type %T", parsed)
	}
	return cert, key, nil
}

// Path is the root certificate file, for trust stores and client tools.
func (c *CA) Path() string { return c.path }

// Root is the parsed root certificate.
func (c *CA) Root() *x509.Certificate { return c.cert }

func (c *CA) owns(name string) bool {
	for _, tld := range c.tlds {
		if strings.HasSuffix(name, "."+tld) {
			return true
		}
	}
	return false
}

// GetCertificate implements tls.Config.GetCertificate. It mints once per name
// and refuses names outside the configured TLDs.
func (c *CA) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(hello.ServerName)
	if !c.owns(name) {
		return nil, fmt.Errorf("no certificate for %q", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[name]; ok {
		return leaf, nil
	}
	leaf, err := c.mint(name)
	if err != nil {
		return nil, err
	}
	c.leaves[name] = leaf
	return leaf, nil
}

func (c *CA) mint(name string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.AddDate(0, 0, leafDays),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: parsed}, nil
}
