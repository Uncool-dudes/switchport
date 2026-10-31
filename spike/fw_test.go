//go:build darwin

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fwEnv struct {
	p    *Proxy
	addr string
	pool *x509.CertPool
	c    *http.Client
}

func fwSetup(t *testing.T) *fwEnv {
	t.Helper()
	ca, err := newCA([]string{"test", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(ca, []string{"test", "localhost"}, 2*time.Second)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: p, TLSConfig: p.TLSConfig()}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	addr := ln.Addr().String()
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &fwEnv{p: p, addr: addr, pool: pool, c: &http.Client{Transport: tr, Timeout: 90 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *fwEnv) get(host, path string) (int, string) {
	req, _ := http.NewRequest("GET", "https://"+host+path, nil)
	req.Header.Set("Accept", "text/html")
	res, err := e.c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// ws performs a real RFC 6455 handshake with a browser-style Origin.
func (e *fwEnv) ws(host, path, proto string) (int, string) {
	c, err := tls.Dial("tcp", e.addr, &tls.Config{ServerName: host, RootCAs: e.pool, NextProtos: []string{"http/1.1"}})
	if err != nil {
		return 0, err.Error()
	}
	defer c.Close()
	key := make([]byte, 16)
	rand.Read(key)
	var sb strings.Builder
	fmt.Fprintf(&sb, "GET %s HTTP/1.1\r\nHost: %s\r\nOrigin: https://%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n",
		path, host, host, base64.StdEncoding.EncodeToString(key))
	if proto != "" {
		fmt.Fprintf(&sb, "Sec-WebSocket-Protocol: %s\r\n", proto)
	}
	sb.WriteString("\r\n")
	io.WriteString(c, sb.String())
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return 0, err.Error()
	}
	first := ""
	if res.StatusCode == 101 {
		buf := make([]byte, 200)
		n, _ := br.Read(buf)
		first = strings.ToValidUTF8(string(buf[:n]), "?")
	}
	return res.StatusCode, first
}

type variant struct {
	label, host string
	rewrite     bool
}

func TestFrameworks(t *testing.T) {
	fw, err := filepath.Abs(".fw")
	if err != nil || !exists(fw) {
		t.Skip("no .fw fixtures")
	}
	withPy := []string{"run", "--no-project", "--with", "flask", "--with", "fastapi", "--with", "uvicorn", "--with", "django"}
	cases := []struct {
		name, dir, want string
		cmd             []string
		wsPath          func(string) (string, string)
		timeout         time.Duration
	}{
		{name: "vite", dir: "vite", want: "vite-ok", cmd: []string{"pnpm", "exec", "vite"}, wsPath: viteWS},
		{name: "next", dir: "next", want: "next-ok", cmd: []string{"pnpm", "exec", "next", "dev"}, wsPath: func(string) (string, string) { return "/_next/hmr", "" }, timeout: 40 * time.Second},
		{name: "astro", dir: "astro", want: "astro-ok", cmd: []string{"pnpm", "exec", "astro", "dev", "--ignore-lock"}, wsPath: viteWS},
		{name: "flask", dir: "py", want: "flask-ok", cmd: append(append([]string{"uv"}, withPy...), "flask", "--app", "flask_app", "run", "--port", "5057")},
		{name: "django", dir: "py", want: "django-ok", cmd: append(append([]string{"uv"}, withPy...), "python", "manage.py", "runserver")},
		{name: "uvicorn", dir: "py", want: "fastapi-ok", cmd: append(append([]string{"uv"}, withPy...), "uvicorn", "asgi_app:app")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assigned := freePort(t)
			s := spawn(t, filepath.Join(fw, c.dir), []string{"PORT=" + strconv.Itoa(assigned)}, c.cmd[0], c.cmd[1:]...)
			to := 30 * time.Second
			if c.timeout > 0 {
				to = c.timeout
			}
			d, err := Discover(context.Background(), s.tr, DiscoverOpts{Assigned: assigned, Settle: 700 * time.Millisecond, Timeout: to, Exited: s.exited})
			if err != nil {
				b, _ := os.ReadFile(s.log)
				t.Fatalf("discover: %v\n%s", err, b[:min(600, len(b))])
			}
			t.Logf("FW %-8s observed %s honoredPORT=%v http=%v %dms candidates=%d", c.name, d.Addr(), d.Port == assigned, d.HTTP, d.Elapsed.Milliseconds(), len(d.Candidates))
			e := fwSetup(t)
			for _, v := range []variant{{"x.test keep-Host", "w.test", false}, {"x.test rewrite-Host", "r.test", true}, {"x.localhost keep-Host", "w.localhost", false}} {
				e.p.Set(v.host, Route{Port: d.Port, Host: d.DialHost(), RewriteHost: v.rewrite})
				code, body := e.get(v.host, "/")
				line := fmt.Sprintf("FW %-8s %-22s GET=%d ok=%v", c.name, v.label, code, strings.Contains(body, c.want))
				if code >= 400 {
					line += " body=" + strings.Join(strings.Fields(body), " ")[:min(70, len(strings.Join(strings.Fields(body), " ")))]
				}
				if c.wsPath != nil {
					path, proto := c.wsPath(body)
					if code == 200 && c.dir != "next" {
						_, cl := e.get(v.host, "/@vite/client")
						path, proto = c.wsPath(cl)
						if m := regexp.MustCompile(`(?:const|var|let) (hmrPort|socketHost|directSocketHost) = ([^;]+);`).FindAllStringSubmatch(cl, -1); len(m) > 0 && v.label == "x.test keep-Host" {
							for _, g := range m {
								t.Logf("FW %-8s client %s = %s", c.name, g[1], g[2])
							}
						}
					}
					wc, first := e.ws(v.host, path, proto)
					line += fmt.Sprintf(" WS=%d", wc)
					if wc == 101 {
						line += fmt.Sprintf(" frame=%q", first[:min(40, len(first))])
					}
				}
				t.Log(line)
			}
		})
	}
}

var viteToken = regexp.MustCompile(`wsToken = "([^"]+)"`)

func viteWS(client string) (string, string) {
	m := viteToken.FindStringSubmatch(client)
	if m == nil {
		return "/", "vite-hmr"
	}
	return "/?token=" + m[1], "vite-hmr"
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Two copies of a tool with one default port must not collide silently.
func TestTwoVites(t *testing.T) {
	fw, _ := filepath.Abs(".fw/vite")
	if !exists(fw) {
		t.Skip("no fixture")
	}
	var ports []string
	for range 2 {
		s := spawn(t, fw, nil, "pnpm", "exec", "vite")
		d, err := Discover(context.Background(), s.tr, DiscoverOpts{Assigned: 1, Settle: 700 * time.Millisecond, Timeout: 20 * time.Second, Exited: s.exited})
		if err != nil {
			t.Fatal(err)
		}
		ports = append(ports, d.Addr())
	}
	t.Logf("FW two vites -> %v", ports)
	if ports[0] == ports[1] {
		t.Fatal("same port")
	}
}

// Package managers differ in how they launch the server, so each must be
// tracked, routed, and stopped without leaving the server behind.
func TestPackageManagerWrappers(t *testing.T) {
	fw, _ := filepath.Abs(".fw/vite")
	if !exists(fw) {
		t.Skip("no fixture")
	}
	os.WriteFile(filepath.Join(fw, "package.json"), []byte(`{"name":"fw-vite","private":true,"scripts":{"dev":"vite"},"dependencies":{"vite":"^8.3.5"}}`), 0o644)
	for _, c := range [][]string{
		{"pnpm", "exec", "vite"},
		{"pnpm", "run", "dev"},
		{"npm", "run", "dev"},
		{"npx", "vite"},
		{"sh", "-c", "node_modules/.bin/vite | cat"},
	} {
		label := strings.Join(c, " ")
		t.Run(label, func(t *testing.T) {
			s := spawn(t, fw, nil, c[0], c[1:]...)
			d, err := Discover(context.Background(), s.tr, DiscoverOpts{Assigned: 1, Settle: 300 * time.Millisecond, Timeout: 20 * time.Second, Exited: s.exited})
			if err != nil {
				b, _ := os.ReadFile(s.log)
				t.Fatalf("discover: %v\n%s", err, b)
			}
			if err := s.tr.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			stopped := false
			for range 40 {
				time.Sleep(100 * time.Millisecond)
				c, err := net.DialTimeout("tcp", d.Addr(), 100*time.Millisecond)
				if err != nil {
					stopped = true
					break
				}
				c.Close()
			}
			t.Logf("FW %-34s -> %s %dms stoppedAfterSIGTERM=%v", label, d.Addr(), d.Elapsed.Milliseconds(), stopped)
			if !stopped {
				t.Fatal("server survived")
			}
		})
	}
}
