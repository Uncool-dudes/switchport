// Package proxy forwards requests to backends by hostname.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	loopHeader    = "X-Switchport"
	localhostTLD  = "localhost"
	holdRetryWait = 50 * time.Millisecond
	grpcType      = "application/grpc"
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
	p.http1 = &httputil.ReverseProxy{Rewrite: p.rewrite, Transport: &http.Transport{}, ErrorHandler: backendError}
	p.h2c = &httputil.ReverseProxy{Rewrite: p.rewrite, Transport: &http.Transport{Protocols: h2c}, ErrorHandler: backendError}
	return p
}

type handler struct {
	lookup     Lookup
	opts       Options
	http1, h2c *httputil.ReverseProxy
}

type targetKey struct{}

type target struct {
	backend     Backend
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
	t := target{backend: backend, rewriteHost: !strings.HasSuffix(host, "."+localhostTLD)}
	r = r.WithContext(context.WithValue(r.Context(), targetKey{}, t))
	if backend.H2C || strings.HasPrefix(r.Header.Get("Content-Type"), grpcType) {
		p.h2c.ServeHTTP(w, r)
		return
	}
	p.http1.ServeHTTP(w, r)
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

func (p *handler) rewrite(r *httputil.ProxyRequest) {
	t := r.In.Context().Value(targetKey{}).(target)
	r.Out.URL.Scheme = "http"
	r.Out.URL.Host = t.backend.Addr
	r.Out.Host = r.In.Host
	r.SetXForwarded()
	r.Out.Header.Set("X-Forwarded-Port", p.opts.Port)
	r.Out.Header.Set(loopHeader, "1")
	if t.rewriteHost {
		p.rewriteForHostChecks(r, t.backend.Addr)
	}
}

// rewriteForHostChecks presents a request as local, because dev servers reject
// hostnames they do not know. Those checks stop malicious pages reaching dev
// servers, so an Origin outside our own TLDs passes through unchanged and the
// backend still refuses it.
func (p *handler) rewriteForHostChecks(r *httputil.ProxyRequest, addr string) {
	_, port, err := net.SplitHostPort(addr)
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
		r.Out.Header.Set("Origin", "http://"+local)
	}
}

func backendError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
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
