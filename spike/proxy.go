// Throwaway stdlib-only viability spike for the devroute data plane.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}

func newCA(permitted []string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "devroute spike CA"},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomains:         permitted,
		PermittedDNSDomainsCritical: len(permitted) > 0,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key}, nil
}

func (c *CA) leaf(name string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 90),
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

type Route struct {
	Port int
	H2C  bool
	// Host is the backend address; empty means 127.0.0.1.
	Host string
	// RewriteHost sends localhost:<port> as Host for dev servers that block
	// unknown hosts. X-Forwarded-Host still carries the public name.
	RewriteHost bool
}

func (r Route) Addr() string {
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(r.Port))
}

type routeKey struct{}

const loopHeader = "X-Devroute"

type Proxy struct {
	ca   *CA
	tlds []string
	hold time.Duration

	mu     sync.RWMutex
	routes map[string]Route

	certMu sync.Mutex
	certs  map[string]*tls.Certificate
	Mints  atomic.Int64

	Dialing atomic.Int64
	h1, h2c *httputil.ReverseProxy
}

func NewProxy(ca *CA, tlds []string, hold time.Duration) *Proxy {
	p := &Proxy{ca: ca, tlds: tlds, hold: hold, routes: map[string]Route{}, certs: map[string]*tls.Certificate{}}
	h2cProtocols := new(http.Protocols)
	h2cProtocols.SetUnencryptedHTTP2(true)
	p.h1 = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		Transport:    &http.Transport{},
		ErrorHandler: p.backendError,
	}
	p.h2c = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		Transport:    &http.Transport{Protocols: h2cProtocols},
		ErrorHandler: p.backendError,
	}
	return p
}

func (p *Proxy) Set(name string, r Route) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.routes[name] = r
}

func (p *Proxy) Delete(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.routes, name)
}

func (p *Proxy) lookup(name string) (Route, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.routes[name]
	return r, ok
}

func (p *Proxy) ownsHost(host string) bool {
	for _, tld := range p.tlds {
		if strings.HasSuffix(host, "."+tld) {
			return true
		}
	}
	return false
}

func (p *Proxy) TLSConfig() *tls.Config {
	return &tls.Config{GetCertificate: p.getCertificate}
}

func (p *Proxy) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.ToLower(hello.ServerName)
	if !p.ownsHost(name) {
		return nil, fmt.Errorf("no certificate for %q", name)
	}
	p.certMu.Lock()
	defer p.certMu.Unlock()
	if c, ok := p.certs[name]; ok {
		return c, nil
	}
	c, err := p.ca.leaf(name)
	if err != nil {
		return nil, err
	}
	p.certs[name] = c
	p.Mints.Add(1)
	return c, nil
}

// waitReady holds a request until its backend accepts connections. It runs in
// the handler, not Transport.DialContext, because Transport dials on a context
// detached from the request and would keep retrying after the client left.
func (p *Proxy) waitReady(ctx context.Context, addr string) {
	p.Dialing.Add(1)
	defer p.Dialing.Add(-1)
	var d net.Dialer
	deadline := time.Now().Add(p.hold)
	for time.Now().Before(deadline) {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// loopbackOnly drops non-loopback peers before the TLS handshake, which is
// required when macOS forces a wildcard bind for an unprivileged :443.
type loopbackOnly struct{ net.Listener }

func (l loopbackOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok && a.IP.IsLoopback() {
			return c, nil
		}
		c.Close()
	}
}

func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	rt := r.In.Context().Value(routeKey{}).(Route)
	r.Out.URL.Scheme = "http"
	r.Out.URL.Host = rt.Addr()
	r.Out.Host = r.In.Host
	r.SetXForwarded()
	if rt.RewriteHost {
		r.Out.Host = "localhost:" + strconv.Itoa(rt.Port)
		if r.Out.Header.Get("Origin") != "" {
			r.Out.Header.Set("Origin", "http://"+r.Out.Host)
		}
	}
	r.Out.Header.Set("X-Forwarded-Port", "443")
	r.Out.Header.Set(loopHeader, "1")
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code string) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<h1>%d %s</h1>", status, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, code)
}

func (p *Proxy) backendError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	writeError(w, r, http.StatusBadGateway, "bad_gateway")
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(loopHeader) != "" {
		writeError(w, r, http.StatusLoopDetected, "loop_detected")
		return
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !p.ownsHost(host) {
		writeError(w, r, http.StatusMisdirectedRequest, "misdirected")
		return
	}
	rt, ok := p.lookup(host)
	if !ok {
		writeError(w, r, http.StatusNotFound, "unknown_route")
		return
	}
	p.waitReady(r.Context(), rt.Addr())
	r = r.WithContext(context.WithValue(r.Context(), routeKey{}, rt))
	if rt.H2C {
		p.h2c.ServeHTTP(w, r)
		return
	}
	p.h1.ServeHTTP(w, r)
}

// answerDNS builds a reply to one query: A records under tlds get 127.0.0.1.
func answerDNS(q []byte, tlds []string) ([]byte, bool) {
	if len(q) < 12 {
		return nil, false
	}
	var labels []string
	i := 12
	for {
		if i >= len(q) {
			return nil, false
		}
		n := int(q[i])
		if n == 0 {
			i++
			break
		}
		if n > 63 || i+1+n > len(q) {
			return nil, false
		}
		labels = append(labels, string(q[i+1:i+1+n]))
		i += 1 + n
	}
	if i+4 > len(q) {
		return nil, false
	}
	qtype := binary.BigEndian.Uint16(q[i:])
	name := strings.ToLower(strings.Join(labels, "."))
	end := i + 4

	owned := false
	for _, tld := range tlds {
		owned = owned || strings.HasSuffix(name, "."+tld)
	}
	resp := append([]byte(nil), q[:end]...)
	resp[2] = 0x84 | (q[2] & 0x01) // QR, AA, copy RD
	resp[3] = 0
	binary.BigEndian.PutUint16(resp[6:], 0)
	binary.BigEndian.PutUint16(resp[8:], 0)
	binary.BigEndian.PutUint16(resp[10:], 0)
	if !owned {
		resp[3] = 3 // NXDOMAIN
		return resp, true
	}
	if qtype != 1 {
		return resp, true // NOERROR, no data
	}
	binary.BigEndian.PutUint16(resp[6:], 1)
	resp = append(resp, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 5, 0, 4, 127, 0, 0, 1)
	return resp, true
}

func serveDNS(pc net.PacketConn, tlds []string) {
	buf := make([]byte, 512)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp, ok := answerDNS(buf[:n], tlds); ok {
			pc.WriteTo(resp, addr)
		}
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "dns" {
		serveLoggedDNS(os.Args[2], os.Args[3:])
		return
	}
	ca, err := newCA([]string{"test"})
	if err != nil {
		log.Fatal(err)
	}
	p := NewProxy(ca, []string{"test"}, 30*time.Second)
	pc, err := net.ListenPacket("udp", "127.0.0.1:53535")
	if err != nil {
		log.Fatal(err)
	}
	go serveDNS(pc, []string{"test"})
	srv := &http.Server{Addr: "127.0.0.1:8443", Handler: p, TLSConfig: p.TLSConfig()}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// serveLoggedDNS prints each query so a test can see whether the OS reached it.
func serveLoggedDNS(addr string, tlds []string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		resp, ok := answerDNS(buf[:n], tlds)
		if !ok {
			continue
		}
		var labels []string
		for i := 12; i < n && buf[i] != 0; i += int(buf[i]) + 1 {
			labels = append(labels, string(buf[i+1:i+1+int(buf[i])]))
		}
		fmt.Printf("QUERY %s\n", strings.Join(labels, "."))
		pc.WriteTo(resp, from)
	}
}
