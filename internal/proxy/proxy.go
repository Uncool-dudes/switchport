// Package proxy forwards requests to backends by hostname.
package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	loopHeader    = "X-Switchport"
	localhostTLD  = "localhost"
	holdRetryWait = 50 * time.Millisecond
	grpcType      = "application/grpc"
	tlsProbeWait  = 3 * time.Second
)

// Backend is where a hostname's requests go.
type Backend struct {
	Addr string // host:port, as observed, so it may be IPv6
	H2C  bool   // always speak HTTP/2 cleartext to this backend
}

// Lookup finds the backend for a lowercased hostname.
type Lookup func(host string) (Backend, bool)

// Options configure a Handler.
type Options struct {
	TLDs   []string        // hostname suffixes this proxy serves
	Port   string          // public port, sent as X-Forwarded-Port
	Hold   time.Duration   // how long a request waits for a starting backend
	Routes func() []string // hostnames listed on the not-found page
}

// New returns a handler that forwards to the backends lookup finds.
func New(lookup Lookup, o Options) http.Handler {
	h2c := new(http.Protocols)
	h2c.SetUnencryptedHTTP2(true)
	p := &handler{lookup: lookup, opts: o}
	// One transport cannot do both: with HTTP/1 enabled, http:// never uses h2c.
	p.http1 = p.reverseProxy(&http.Transport{})
	p.h2c = p.reverseProxy(&http.Transport{Protocols: h2c})
	// Without ForceAttemptHTTP2 this offers only HTTP/1.1, which WebSocket
	// upgrades need. Only gRPC gets the HTTP/2 transport.
	p.tls = p.reverseProxy(&http.Transport{TLSClientConfig: backendTLS()})
	p.tlsH2 = p.reverseProxy(&http.Transport{TLSClientConfig: backendTLS(), ForceAttemptHTTP2: true})
	return p
}

// backendTLS skips verification because backends are always loopback addresses
// and dev servers present self-signed certificates.
func backendTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true}
}

func (p *handler) reverseProxy(t *http.Transport) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{Rewrite: p.rewrite, Transport: t, ModifyResponse: p.checkScheme, ErrorHandler: p.backendError}
}

type handler struct {
	lookup                 Lookup
	opts                   Options
	http1, h2c, tls, tlsH2 *httputil.ReverseProxy
	schemes                sync.Map // addr to speaksTLS result
}

type targetKey struct{}

type target struct {
	backend     Backend
	secure      bool
	rewriteHost bool
}

func (p *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(loopHeader) != "" {
		writeError(w, r, http.StatusLoopDetected, "loop_detected", nil)
		return
	}
	host := hostname(r.Host)
	if !p.owns(host) {
		writeError(w, r, http.StatusMisdirectedRequest, "misdirected", nil)
		return
	}
	backend, ok := p.lookup(host)
	if !ok {
		writeError(w, r, http.StatusNotFound, "unknown_route", p.routes())
		return
	}
	p.waitReady(r.Context(), backend.Addr)
	secure := !backend.H2C && p.backendUsesTLS(r.Context(), backend.Addr)
	t := target{backend: backend, secure: secure, rewriteHost: !strings.HasSuffix(host, "."+localhostTLD)}
	r = r.WithContext(context.WithValue(r.Context(), targetKey{}, t))
	grpc := strings.HasPrefix(r.Header.Get("Content-Type"), grpcType)
	switch {
	case secure && grpc:
		p.tlsH2.ServeHTTP(w, r)
	case secure:
		p.tls.ServeHTTP(w, r)
	case backend.H2C || grpc:
		p.h2c.ServeHTTP(w, r)
	default:
		p.http1.ServeHTTP(w, r)
	}
}

// backendUsesTLS asks each address once, because the probe is not free: plain
// servers log it as a bad request, and a request waits for it. checkScheme and
// backendError forget the answer when it may have gone stale.
func (p *handler) backendUsesTLS(ctx context.Context, addr string) bool {
	if known, ok := p.schemes.Load(addr); ok {
		return known.(bool)
	}
	secure, answered := speaksTLS(ctx, addr)
	if answered {
		p.schemes.Store(addr, secure)
	}
	return secure
}

// speaksTLS tries a handshake, the only signal available: on a new connection
// the client talks first, so a server reveals nothing until then. The wait is
// long because a dev server can block its event loop for seconds, and giving up
// on a TLS backend turns into a 502. answered is false when nothing listened.
func speaksTLS(ctx context.Context, addr string) (secure, answered bool) {
	ctx, cancel := context.WithTimeout(ctx, tlsProbeWait)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, false
	}
	defer conn.Close()
	return tls.Client(conn, backendTLS()).HandshakeContext(ctx) == nil, true
}

// checkScheme catches a TLS server that took over a port: it can answer plain
// HTTP with a valid 400, so no transport error reaches backendError.
func (p *handler) checkScheme(res *http.Response) error {
	t := res.Request.Context().Value(targetKey{}).(target)
	if !t.secure && res.StatusCode == http.StatusBadRequest {
		p.schemes.Delete(t.backend.Addr)
	}
	return nil
}

func (p *handler) routes() []string {
	if p.opts.Routes == nil {
		return nil
	}
	return p.opts.Routes()
}

func hostname(hostport string) string {
	host := strings.ToLower(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func (p *handler) owns(host string) bool {
	for _, tld := range p.opts.TLDs {
		if strings.HasSuffix(host, "."+tld) {
			return true
		}
	}
	return false
}

// waitReady holds a request until its backend accepts connections. It runs
// here, not in Transport.DialContext, because Transport dials on a context
// detached from the request and would keep retrying after the client left.
func (p *handler) waitReady(ctx context.Context, addr string) {
	var d net.Dialer
	deadline := time.Now().Add(p.opts.Hold)
	for time.Now().Before(deadline) {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(holdRetryWait):
		}
	}
}

func (t target) scheme() string {
	if t.secure {
		return "https"
	}
	return "http"
}

func (p *handler) rewrite(r *httputil.ProxyRequest) {
	t := r.In.Context().Value(targetKey{}).(target)
	r.Out.URL.Scheme = t.scheme()
	r.Out.URL.Host = t.backend.Addr
	r.Out.Host = r.In.Host
	r.SetXForwarded()
	r.Out.Header.Set("X-Forwarded-Port", p.opts.Port)
	r.Out.Header.Set(loopHeader, "1")
	if t.rewriteHost {
		p.rewriteForHostChecks(r, t)
	}
}

// rewriteForHostChecks presents a request as local, because dev servers reject
// hostnames they do not know. Those checks stop malicious pages reaching dev
// servers, so an Origin outside our own TLDs passes through unchanged and the
// backend still refuses it.
func (p *handler) rewriteForHostChecks(r *httputil.ProxyRequest, t target) {
	_, port, err := net.SplitHostPort(t.backend.Addr)
	if err != nil {
		return
	}
	local := "localhost:" + port
	r.Out.Host = local
	origin := r.In.Header.Get("Origin")
	if origin == "" {
		return
	}
	if u, err := url.Parse(origin); err == nil && p.owns(hostname(u.Host)) {
		r.Out.Header.Set("Origin", t.scheme()+"://"+local)
	}
}

func (p *handler) backendError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if t, ok := r.Context().Value(targetKey{}).(target); ok {
		p.schemes.Delete(t.backend.Addr)
	}
	writeError(w, r, http.StatusBadGateway, "bad_gateway", nil)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code string, routes []string) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<h1>%d %s</h1>", status, html.EscapeString(code))
		if len(routes) > 0 {
			fmt.Fprint(w, "<ul>")
			for _, route := range routes {
				escaped := html.EscapeString(route)
				fmt.Fprintf(w, `<li><a href="https://%s/">%s</a></li>`, escaped, escaped)
			}
			fmt.Fprint(w, "</ul>")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding a struct of strings cannot fail, and headers are already sent.
	_ = json.NewEncoder(w).Encode(struct {
		Error  string   `json:"error"`
		Routes []string `json:"routes,omitempty"`
	}{code, routes})
}
