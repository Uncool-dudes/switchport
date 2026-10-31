//go:build darwin

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func directWS(addr, host, origin, path string) string {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	key := make([]byte, 16)
	rand.Read(key)
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", path, host, origin, base64.StdEncoding.EncodeToString(key))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return "err: " + err.Error()
	}
	return strconv.Itoa(res.StatusCode)
}

func TestNextWS(t *testing.T) {
	fw, _ := filepath.Abs(".fw/next")
	if !exists(fw) {
		t.Skip("no fixture")
	}
	assigned := freePort(t)
	s := spawn(t, fw, []string{"PORT=" + strconv.Itoa(assigned)}, "pnpm", "exec", "next", "dev")
	d, err := Discover(context.Background(), s.tr, DiscoverOpts{Assigned: assigned, Settle: time.Second, Timeout: 40 * time.Second, Exited: s.exited})
	if err != nil {
		t.Fatal(err)
	}
	e := fwSetup(t)
	e.p.Set("w.test", Route{Port: d.Port, Host: d.DialHost()})
	e.p.Set("r.test", Route{Port: d.Port, Host: d.DialHost(), RewriteHost: true})
	e.p.Set("w.localhost", Route{Port: d.Port, Host: d.DialHost()})
	_, body := e.get("w.test", "/")
	t.Logf("NEXT page mentions hmr path: %v", len(body))
	path := "/_next/hmr"
	lh := "localhost:" + strconv.Itoa(d.Port)
	t.Logf("NEXT direct Host=localhost Origin=localhost -> %s", directWS(d.Addr(), lh, "http://"+lh, path))
	t.Logf("NEXT direct Host=localhost Origin=w.test    -> %s", directWS(d.Addr(), lh, "https://w.test", path))
	t.Logf("NEXT direct Host=w.test    Origin=w.test    -> %s", directWS(d.Addr(), "w.test", "https://w.test", path))
	for _, h := range []string{"w.test", "r.test", "w.localhost"} {
		code, first := e.ws(h, path, "")
		t.Logf("NEXT proxy  %-12s -> %d %q", h, code, first[:min(60, len(first))])
	}
	time.Sleep(300 * time.Millisecond)
	b, _ := os.ReadFile(s.log)
	t.Logf("NEXT server log tail:\n%s", b[max(0, len(b)-900):])
}

var _ = io.EOF
