package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type env struct {
	p    *Proxy
	addr string
	pool *x509.CertPool
	c    *http.Client
}

func setup(t *testing.T, hold time.Duration) *env {
	t.Helper()
	ca, err := newCA([]string{"test"})
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy(ca, []string{"test"}, hold)
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
	return &env{p: p, addr: addr, pool: pool, c: &http.Client{Transport: tr}}
}

func backend(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.Listener.Addr().(*net.TCPAddr).Port
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestH2ForwardingHeaders(t *testing.T) {
	e := setup(t, 0)
	e.p.Set("api.test", Route{Port: backend(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"host": r.Host, "proto": r.Proto, "hdr": r.Header})
	})})
	req, _ := http.NewRequest("GET", "https://api.test/x", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	res, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Fatalf("client proto %s", res.Proto)
	}
	var got struct {
		Host, Proto string
		Hdr         http.Header
	}
	json.NewDecoder(res.Body).Decode(&got)
	if got.Host != "api.test" || got.Proto != "HTTP/1.1" {
		t.Fatalf("%+v", got)
	}
	if got.Hdr.Get("X-Forwarded-Proto") != "https" || got.Hdr.Get("X-Forwarded-Host") != "api.test" || got.Hdr.Get("X-Forwarded-Port") != "443" {
		t.Fatalf("%+v", got.Hdr)
	}
	if strings.Contains(got.Hdr.Get("X-Forwarded-For"), "6.6.6.6") {
		t.Fatalf("spoofed XFF kept: %v", got.Hdr)
	}
}

func TestStatusCodes(t *testing.T) {
	e := setup(t, 0)
	e.p.Set("dead.test", Route{Port: freePort(t)})
	cases := []struct {
		host   string
		accept string
		extra  bool
		want   int
		ctype  string
	}{
		{"nope.test", "", false, 404, "application/json"},
		{"nope.test", "text/html", false, 404, "text/html"},
		{"dead.test", "", false, 502, "application/json"},
		{"dead.test", "text/html", false, 502, "text/html"},
		{"dead.test", "", true, 508, "application/json"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest("GET", "https://"+c.host+"/", nil)
		req.Header.Set("Accept", c.accept)
		if c.extra {
			req.Header.Set(loopHeader, "1")
		}
		res, err := e.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want || !strings.HasPrefix(res.Header.Get("Content-Type"), c.ctype) {
			t.Errorf("%+v got %d %s", c, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	// A foreign SNI fails the handshake, so 421 applies only when Host differs from SNI.
	req, _ := http.NewRequest("GET", "https://nope.test/", nil)
	req.Host = "example.com"
	res, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 421 {
		t.Errorf("foreign host: %d", res.StatusCode)
	}
}

func wsEcho(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "no upgrade", 400)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprint(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	brw.Flush()
	line, err := brw.ReadString('\n')
	if err != nil {
		return
	}
	fmt.Fprint(brw, "echo:"+line)
	brw.Flush()
	time.Sleep(50 * time.Millisecond)
	fmt.Fprint(brw, "server-push\n")
	brw.Flush()
}

func TestWebSocketOverH1ALPN(t *testing.T) {
	e := setup(t, 0)
	e.p.Set("ws.test", Route{Port: backend(t, wsEcho)})
	conn, err := tls.Dial("tcp", e.addr, &tls.Config{ServerName: "ws.test", RootCAs: e.pool, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /hmr HTTP/1.1\r\nHost: ws.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 101 {
		t.Fatalf("status %d", res.StatusCode)
	}
	fmt.Fprint(conn, "hello\n")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if l, _ := br.ReadString('\n'); l != "echo:hello\n" {
		t.Fatalf("echo %q", l)
	}
	if l, _ := br.ReadString('\n'); l != "server-push\n" {
		t.Fatalf("push %q", l)
	}
}

// RFC 8441: without SETTINGS_ENABLE_CONNECT_PROTOCOL browsers must not try
// extended CONNECT, so WebSockets go over a separate HTTP/1.1 connection.
func TestH2DoesNotAdvertiseExtendedConnect(t *testing.T) {
	e := setup(t, 0)
	conn, err := tls.Dial("tcp", e.addr, &tls.Config{ServerName: "ws.test", RootCAs: e.pool, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("no h2")
	}
	io.WriteString(conn, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	conn.Write([]byte{0, 0, 0, 4, 0, 0, 0, 0, 0})
	hdr := make([]byte, 9)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[3] != 4 {
		t.Fatalf("first frame type %d", hdr[3])
	}
	n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
	body := make([]byte, n)
	io.ReadFull(conn, body)
	for i := 0; i+6 <= n; i += 6 {
		id := binary.BigEndian.Uint16(body[i:])
		t.Logf("setting %d = %d", id, binary.BigEndian.Uint32(body[i+2:]))
		if id == 8 {
			t.Fatalf("server advertises extended CONNECT")
		}
	}
}

func TestSSEStreamsUnbuffered(t *testing.T) {
	e := setup(t, 0)
	release := make(chan struct{})
	e.p.Set("sse.test", Route{Port: backend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: second\n\n")
	})})
	res, err := e.c.Get("https://sse.test/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)
	done := make(chan string, 1)
	go func() { l, _ := br.ReadString('\n'); done <- l }()
	select {
	case l := <-done:
		if l != "data: first\n" {
			t.Fatalf("%q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first event buffered")
	}
	close(release)
}

func TestHoldUntilBackendStarts(t *testing.T) {
	e := setup(t, 5*time.Second)
	port := freePort(t)
	e.p.Set("late.test", Route{Port: port})
	go func() {
		time.Sleep(700 * time.Millisecond)
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Error(err)
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "%s %d", r.Method, len(b))
		})}
		t.Cleanup(func() { srv.Close() })
		srv.Serve(ln)
	}()
	start := time.Now()
	res, err := e.c.Post("https://late.test/", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(body) != "POST 7" {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
	el := time.Since(start)
	if el < 600*time.Millisecond {
		t.Fatalf("not held: %v", el)
	}
	t.Logf("held POST %v", el)
}

func TestHoldStopsOnClientDisconnect(t *testing.T) {
	e := setup(t, 30*time.Second)
	e.p.Set("gone.test", Route{Port: freePort(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://gone.test/", nil)
	if _, err := e.c.Do(req); err == nil {
		t.Fatal("expected error")
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.p.Dialing.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("dial loop leaked: %d", e.p.Dialing.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gRPC needs HTTP/2 to the backend and trailers passed through.
func TestH2CBackendTrailers(t *testing.T) {
	e := setup(t, 0)
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Protocols: protos, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		w.Header().Set("X-Backend-Proto", r.Proto)
		w.Header().Set("X-Backend-TE", r.Header.Get("TE"))
		w.(http.Flusher).Flush()
		w.Write([]byte("frame"))
		w.Header().Set("Grpc-Status", "0")
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	e.p.Set("grpc.test", Route{Port: ln.Addr().(*net.TCPAddr).Port, H2C: true})

	req, _ := http.NewRequest("POST", "https://grpc.test/svc/Method", strings.NewReader("x"))
	req.Header.Set("TE", "trailers")
	req.Header.Set("Content-Type", "application/grpc")
	res, err := e.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	io.ReadAll(res.Body)
	if res.Header.Get("X-Backend-Proto") != "HTTP/2.0" || res.Header.Get("X-Backend-TE") != "trailers" {
		t.Fatalf("%v", res.Header)
	}
	if res.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("trailer: %v", res.Trailer)
	}
}

func TestCertMintOncePerNameConcurrently(t *testing.T) {
	e := setup(t, 0)
	var wg sync.WaitGroup
	start := time.Now()
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := tls.Dial("tcp", e.addr, &tls.Config{ServerName: "burst.test", RootCAs: e.pool})
			if err != nil {
				t.Error(err)
				return
			}
			c.Close()
		}()
	}
	wg.Wait()
	if got := e.p.Mints.Load(); got != 1 {
		t.Fatalf("mints %d", got)
	}
	t.Logf("50 concurrent handshakes %v", time.Since(start))
}

func TestForeignSNIRefused(t *testing.T) {
	e := setup(t, 0)
	if c, err := tls.Dial("tcp", e.addr, &tls.Config{ServerName: "example.com", InsecureSkipVerify: true}); err == nil {
		c.Close()
		t.Fatal("handshake should fail")
	}
}

func TestRouteChurnUnderLoad(t *testing.T) {
	e := setup(t, 0)
	port := backend(t, func(w http.ResponseWriter, r *http.Request) {})
	e.p.Set("stable.test", Route{Port: port})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				name := fmt.Sprintf("n%d.test", i%13)
				e.p.Set(name, Route{Port: port})
				e.p.Delete(name)
			}
		}
	}()
	for range 200 {
		res, err := e.c.Get("https://stable.test/")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode)
		}
	}
	close(stop)
	wg.Wait()
}

func TestDNSResponder(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go serveDNS(pc, []string{"test"})
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := r.LookupHost(ctx, "a.b.anything.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0] != "127.0.0.1" {
		t.Fatalf("%v", addrs)
	}
	if _, err := r.LookupHost(ctx, "example.com"); err == nil {
		t.Fatal("expected NXDOMAIN")
	}
}

func TestConstrainedCAInGoVerifier(t *testing.T) {
	ca, _ := newCA([]string{"test"})
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	for name, ok := range map[string]bool{"api.test": true, "a.b.test": true, "api.example.com": false} {
		leaf, err := ca.leaf(name)
		if err != nil {
			t.Fatal(err)
		}
		_, verr := leaf.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name})
		if (verr == nil) != ok {
			t.Errorf("%s: %v", name, verr)
		}
	}
}

// Asks macOS itself, without touching any keychain.
func TestMacOSVerifier(t *testing.T) {
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("no security tool")
	}
	dir := t.TempDir()
	writePEM := func(path string, der []byte) {
		os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	}
	for _, c := range []struct {
		label     string
		permitted []string
	}{{"unconstrained", nil}, {"constrained", []string{"test"}}} {
		ca, err := newCA(c.permitted)
		if err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(dir, c.label+"-root.pem")
		writePEM(root, ca.cert.Raw)
		for _, name := range []string{"api.test", "a.b.test", "api.example.com", "*.test", "*.myapp.test"} {
			leaf, err := ca.leaf(name)
			if err != nil {
				t.Fatal(err)
			}
			lf := filepath.Join(dir, "leaf.pem")
			writePEM(lf, leaf.Certificate[0])
			host := strings.Replace(name, "*", "tenant", 1)
			out, err := exec.Command("security", "verify-cert", "-c", lf, "-r", root, "-p", "ssl", "-s", host, "-L", "-n").CombinedOutput()
			t.Logf("macOS %-13s SAN %-17s host %-17s pass=%v %s", c.label, name, host, err == nil, strings.TrimSpace(string(out)))
		}
	}
}

func TestBindLow443Unprivileged(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:443")
	if err != nil {
		t.Logf("bind 127.0.0.1:443: %v", err)
		return
	}
	l.Close()
	t.Log("bind 127.0.0.1:443 ok, uid", os.Getuid())
}

func TestBindLow443Variants(t *testing.T) {
	for _, a := range []string{"127.0.0.1:443", "[::1]:443", ":443", "0.0.0.0:443"} {
		l, err := net.Listen("tcp", a)
		if err != nil {
			t.Logf("%-16s DENIED %v", a, err)
			continue
		}
		l.Close()
		t.Logf("%-16s ok (uid %d)", a, os.Getuid())
	}
}

func TestLoopbackOnlyDropsLANPeer(t *testing.T) {
	var lan net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil && n.IP.IsPrivate() {
			lan = n.IP
			break
		}
	}
	if lan == nil {
		t.Skip("no private LAN address")
	}
	inner, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	ln := loopbackOnly{inner}
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	port := inner.Addr().(*net.TCPAddr).Port
	// Dialing the machine's own LAN IP presents a non-loopback source address.
	viaLAN, err := net.Dial("tcp", net.JoinHostPort(lan.String(), strconv.Itoa(port)))
	if err == nil {
		viaLAN.Close()
	}
	select {
	case c := <-accepted:
		t.Fatalf("LAN peer accepted: %v", c.RemoteAddr())
	case <-time.After(300 * time.Millisecond):
	}
	viaLoop, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer viaLoop.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(time.Second):
		t.Fatal("loopback peer dropped")
	}
}
