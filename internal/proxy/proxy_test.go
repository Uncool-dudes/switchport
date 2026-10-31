package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	mu      sync.Mutex
	routes  map[string]Backend
	front   *httptest.Server
	client  *http.Client
	handled chan struct{}
}

func newFixture(t *testing.T, hold time.Duration) *fixture {
	t.Helper()
	f := &fixture{routes: map[string]Backend{}, handled: make(chan struct{}, 8)}
	lookup := func(host string) (Backend, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, ok := f.routes[host]
		return b, ok
	}
	h := New(lookup, Options{
		TLDs:   []string{"localhost", "test"},
		Port:   "443",
		Hold:   hold,
		Routes: func() []string { return []string{"api.localhost"} },
	})
	f.front = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		select {
		case f.handled <- struct{}{}:
		default: // only tests waiting on one request read this
		}
	}))
	f.front.EnableHTTP2 = true
	f.front.StartTLS()
	t.Cleanup(f.front.Close)
	f.client = f.front.Client()
	f.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return f
}

func (f *fixture) route(host string, b Backend) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[host] = b
}

func (f *fixture) backend(t *testing.T, host string, h http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	f.route(host, Backend{Addr: s.Listener.Addr().String()})
}

func (f *fixture) request(method, host string, body io.Reader) *http.Request {
	req, err := http.NewRequest(method, f.front.URL+"/x?a=1", body)
	if err != nil {
		panic(err)
	}
	req.Host = host
	return req
}

func (f *fixture) do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	res, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

type echoed struct {
	Host, Proto, URI string
	Header           http.Header
}

func echo(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(echoed{r.Host, r.Proto, r.RequestURI, r.Header})
}

func (f *fixture) echoed(t *testing.T, req *http.Request) echoed {
	t.Helper()
	res, body := f.do(t, req)
	if res.StatusCode != 200 {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	var e echoed
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestForwardsHTTP2ClientToHTTP1Backend(t *testing.T) {
	f := newFixture(t, 0)
	f.backend(t, "api.localhost", echo)
	req := f.request("GET", "api.localhost", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	res, body := f.do(t, req)
	if res.ProtoMajor != 2 {
		t.Fatalf("client proto %s", res.Proto)
	}
	var e echoed
	json.Unmarshal([]byte(body), &e)
	if e.Proto != "HTTP/1.1" || e.URI != "/x?a=1" {
		t.Fatalf("%+v", e)
	}
	for k, want := range map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "api.localhost", "X-Forwarded-Port": "443"} {
		if got := e.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if strings.Contains(e.Header.Get("X-Forwarded-For"), "6.6.6.6") {
		t.Errorf("spoofed X-Forwarded-For kept: %q", e.Header.Get("X-Forwarded-For"))
	}
}

func TestPreservesHostAndOriginOnLocalhost(t *testing.T) {
	f := newFixture(t, 0)
	f.backend(t, "api.localhost", echo)
	req := f.request("GET", "api.localhost", nil)
	req.Header.Set("Origin", "https://api.localhost")
	e := f.echoed(t, req)
	if e.Host != "api.localhost" || e.Header.Get("Origin") != "https://api.localhost" {
		t.Fatalf("%+v", e)
	}
}

func TestRewritesHostAndOwnOriginOnOtherTLDs(t *testing.T) {
	f := newFixture(t, 0)
	f.backend(t, "api.test", echo)
	_, port, _ := net.SplitHostPort(f.routes["api.test"].Addr)
	req := f.request("GET", "api.test", nil)
	req.Header.Set("Origin", "https://api.test")
	e := f.echoed(t, req)
	local := "localhost:" + port
	if e.Host != local || e.Header.Get("Origin") != "http://"+local {
		t.Fatalf("%+v", e)
	}
	if e.Header.Get("X-Forwarded-Host") != "api.test" {
		t.Fatalf("public name lost: %+v", e.Header)
	}
}

// This is what keeps dev servers' protection against malicious pages working.
func TestLeavesForeignOriginUnchanged(t *testing.T) {
	f := newFixture(t, 0)
	f.backend(t, "api.test", echo)
	for _, origin := range []string{"https://evil.example", "https://test.evil.example", "null", "https://localhost.evil"} {
		req := f.request("GET", "api.test", nil)
		req.Header.Set("Origin", origin)
		if got := f.echoed(t, req).Header.Get("Origin"); got != origin {
			t.Errorf("Origin %q became %q", origin, got)
		}
	}
}

func TestErrorResponses(t *testing.T) {
	f := newFixture(t, 0)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	f.route("dead.localhost", Backend{Addr: dead})
	for _, tc := range []struct {
		host, accept string
		loop         bool
		want         int
		ctype, body  string
	}{
		{"nope.localhost", "", false, 404, "application/json", `"routes":["api.localhost"]`},
		{"nope.localhost", "text/html", false, 404, "text/html", `https://api.localhost/`},
		{"dead.localhost", "", false, 502, "application/json", "bad_gateway"},
		{"dead.localhost", "text/html", false, 502, "text/html", "bad_gateway"},
		{"dead.localhost", "", true, 508, "application/json", "loop_detected"},
		{"example.com", "", false, 421, "application/json", "misdirected"},
		{"localhost", "", false, 421, "application/json", "misdirected"},
	} {
		req := f.request("GET", tc.host, nil)
		req.Header.Set("Accept", tc.accept)
		if tc.loop {
			req.Header.Set(loopHeader, "1")
		}
		res, body := f.do(t, req)
		if res.StatusCode != tc.want || !strings.HasPrefix(res.Header.Get("Content-Type"), tc.ctype) || !strings.Contains(body, tc.body) {
			t.Errorf("%+v: got %d %s %q", tc, res.StatusCode, res.Header.Get("Content-Type"), body)
		}
	}
}

func wsEcho(w http.ResponseWriter, r *http.Request) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprint(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	brw.Flush()
	line, _ := brw.ReadString('\n')
	fmt.Fprint(brw, "echo:"+line+"push\n")
	brw.Flush()
}

func (f *fixture) checkWebSocket(t *testing.T, host string) {
	t.Helper()
	conn, err := tls.Dial("tcp", f.front.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", host)
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 101 {
		t.Fatalf("status %d", res.StatusCode)
	}
	fmt.Fprint(conn, "hi\n")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for _, want := range []string{"echo:hi\n", "push\n"} {
		if got, _ := br.ReadString('\n'); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestRelaysWebSocketOverHTTP1(t *testing.T) {
	f := newFixture(t, 0)
	f.backend(t, "ws.localhost", wsEcho)
	f.checkWebSocket(t, "ws.localhost")
}

func TestStreamsServerSentEvents(t *testing.T) {
	f := newFixture(t, 0)
	release := make(chan struct{})
	f.backend(t, "sse.localhost", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	})
	defer close(release)
	res, err := f.client.Do(f.request("GET", "sse.localhost", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(res.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "data: first\n" {
			t.Fatalf("%q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first event was buffered")
	}
}

func TestHoldsRequestUntilBackendStarts(t *testing.T) {
	f := newFixture(t, 5*time.Second)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	f.route("late.localhost", Backend{Addr: addr})
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "%s %d", r.Method, len(b))
		})}
		t.Cleanup(func() { s.Close() })
		s.Serve(ln)
	}()
	res, body := f.do(t, f.request("POST", "late.localhost", strings.NewReader("payload")))
	if res.StatusCode != 200 || body != "POST 7" {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
}

func TestHoldEndsWhenClientLeaves(t *testing.T) {
	f := newFixture(t, 30*time.Second)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	f.route("gone.localhost", Backend{Addr: addr})
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err := f.client.Do(f.request("GET", "gone.localhost", nil).WithContext(ctx)); err == nil {
		t.Fatal("want error")
	}
	select {
	case <-f.handled:
	case <-time.After(2 * time.Second):
		t.Fatal("handler kept holding after the client left")
	}
}

func h2cBackend(t *testing.T) (string, func() string) {
	t.Helper()
	protos := new(http.Protocols)
	protos.SetHTTP1(true)
	protos.SetUnencryptedHTTP2(true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	proto := ""
	s := &http.Server{Protocols: protos, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proto = r.Proto + " te=" + r.Header.Get("TE")
		mu.Unlock()
		w.Header().Set("Trailer", "Grpc-Status")
		w.(http.Flusher).Flush()
		io.WriteString(w, "frame")
		w.Header().Set("Grpc-Status", "0")
	})}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return ln.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return proto
	}
}

func TestGRPCContentTypeSelectsH2CWithoutRouteFlag(t *testing.T) {
	f := newFixture(t, 0)
	addr, proto := h2cBackend(t)
	f.route("grpc.localhost", Backend{Addr: addr})
	req := f.request("POST", "grpc.localhost", strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("TE", "trailers")
	res, _ := f.do(t, req)
	if proto() != "HTTP/2.0 te=trailers" {
		t.Fatalf("backend saw %q", proto())
	}
	if res.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("trailers %v", res.Trailer)
	}
}

func TestRouteFlagForcesH2C(t *testing.T) {
	f := newFixture(t, 0)
	addr, proto := h2cBackend(t)
	f.route("h2.localhost", Backend{Addr: addr, H2C: true})
	f.do(t, f.request("GET", "h2.localhost", nil))
	if !strings.HasPrefix(proto(), "HTTP/2.0") {
		t.Fatalf("backend saw %q", proto())
	}
}

func TestDialsObservedIPv6Address(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	s := &http.Server{Handler: http.HandlerFunc(echo)}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	f := newFixture(t, 0)
	f.route("v6.test", Backend{Addr: ln.Addr().String()})
	e := f.echoed(t, f.request("GET", "v6.test", nil))
	if want := "localhost:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port); e.Host != want {
		t.Fatalf("host %q, want %q", e.Host, want)
	}
}

func (f *fixture) tlsBackend(t *testing.T, host string, h http.HandlerFunc) {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	s.EnableHTTP2 = true
	s.StartTLS()
	t.Cleanup(s.Close)
	f.route(host, Backend{Addr: s.Listener.Addr().String()})
}

func TestForwardsToTLSBackend(t *testing.T) {
	f := newFixture(t, 0)
	f.tlsBackend(t, "web.localhost", echo)
	got := f.echoed(t, f.request(http.MethodGet, "web.localhost", nil))
	if got.Host != "web.localhost" {
		t.Errorf("host %q", got.Host)
	}
}

func TestSendsHTTPSOriginToTLSBackendOnOtherTLDs(t *testing.T) {
	f := newFixture(t, 0)
	f.tlsBackend(t, "web.test", echo)
	req := f.request(http.MethodGet, "web.test", nil)
	req.Header.Set("Origin", "https://web.test")
	got := f.echoed(t, req)
	if origin := got.Header.Get("Origin"); !strings.HasPrefix(origin, "https://localhost:") {
		t.Errorf("origin %q", origin)
	}
}

func TestRelaysWebSocketToTLSBackend(t *testing.T) {
	f := newFixture(t, 0)
	f.tlsBackend(t, "ws.localhost", wsEcho)
	f.checkWebSocket(t, "ws.localhost")
}

func TestSendsGRPCToTLSBackendOverHTTP2(t *testing.T) {
	f := newFixture(t, 0)
	var proto atomic.Value
	f.tlsBackend(t, "grpc.localhost", func(w http.ResponseWriter, r *http.Request) { proto.Store(r.Proto) })
	req := f.request(http.MethodPost, "grpc.localhost", strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/grpc")
	if res, _ := f.do(t, req); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	if proto.Load() != "HTTP/2.0" {
		t.Errorf("backend saw %v", proto.Load())
	}
}

func serveOn(t *testing.T, addr string, secure bool) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(echo))
	s.Listener.Close()
	s.Listener = ln
	if secure {
		s.StartTLS()
	} else {
		s.Start()
	}
	return s
}

func TestRelearnsSchemeWhenTLSServerTakesThePort(t *testing.T) {
	plain := serveOn(t, "127.0.0.1:0", false)
	addr := plain.Listener.Addr().String()
	f := newFixture(t, 0)
	f.route("web.localhost", Backend{Addr: addr})
	if res, _ := f.do(t, f.request(http.MethodGet, "web.localhost", nil)); res.StatusCode != http.StatusOK {
		t.Fatalf("plain status %d", res.StatusCode)
	}
	plain.Close()

	secure := serveOn(t, addr, true)
	defer secure.Close()
	f.do(t, f.request(http.MethodGet, "web.localhost", nil)) // learns from this answer
	if res, _ := f.do(t, f.request(http.MethodGet, "web.localhost", nil)); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestRelearnsSchemeWhenPlainServerTakesThePort(t *testing.T) {
	secure := serveOn(t, "127.0.0.1:0", true)
	addr := secure.Listener.Addr().String()
	f := newFixture(t, 0)
	f.route("web.localhost", Backend{Addr: addr})
	if res, _ := f.do(t, f.request(http.MethodGet, "web.localhost", nil)); res.StatusCode != http.StatusOK {
		t.Fatalf("secure status %d", res.StatusCode)
	}
	secure.Close()

	plain := serveOn(t, addr, false)
	defer plain.Close()
	f.do(t, f.request(http.MethodGet, "web.localhost", nil)) // learns from this failure
	if res, _ := f.do(t, f.request(http.MethodGet, "web.localhost", nil)); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
}

type slowListener struct {
	net.Listener
	delay time.Duration
}

func (l slowListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	time.Sleep(l.delay)
	return c, err
}

func TestWaitsForTLSBackendThatIsBusy(t *testing.T) {
	s := httptest.NewUnstartedServer(http.HandlerFunc(echo))
	s.Listener = slowListener{s.Listener, 700 * time.Millisecond}
	s.StartTLS()
	defer s.Close()
	f := newFixture(t, 0)
	f.route("web.localhost", Backend{Addr: s.Listener.Addr().String()})
	if res, body := f.do(t, f.request(http.MethodGet, "web.localhost", nil)); res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
}

func TestProbesHealthyBackendOnce(t *testing.T) {
	var conns atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(echo))
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	s.Start()
	defer s.Close()
	f := newFixture(t, 0)
	f.route("web.localhost", Backend{Addr: s.Listener.Addr().String()})
	for range 5 {
		f.do(t, f.request(http.MethodGet, "web.localhost", nil))
	}
	// One for the probe and one kept alive. A zero hold skips the readiness dial.
	if got, want := conns.Load(), int32(2); got != want {
		t.Errorf("%d connections, want %d", got, want)
	}
}
