package daemon

import (
	"net"
	"strconv"
	"testing"
	"time"
)

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
	port := strconv.Itoa(inner.Addr().(*net.TCPAddr).Port)
	// Dialing this machine's own LAN address presents a non-loopback source.
	if viaLAN, err := net.Dial("tcp", net.JoinHostPort(lan.String(), port)); err == nil {
		viaLAN.Close()
	}
	select {
	case c := <-accepted:
		t.Fatalf("LAN peer accepted: %v", c.RemoteAddr())
	case <-time.After(300 * time.Millisecond):
	}
	viaLoop, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
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

func TestStatusNeedsALiveDaemon(t *testing.T) {
	dir := t.TempDir()
	if err := writeStatus(dir+"/"+statusFile, Status{PID: 1, Port: 443}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadStatus(dir); ok {
		t.Fatal("a leftover status file reported a running daemon")
	}
}

func TestSecondDaemonIsRefused(t *testing.T) {
	dir := t.TempDir()
	first, err := acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := acquire(dir); err != ErrRunning {
		t.Fatalf("err = %v", err)
	}
}
