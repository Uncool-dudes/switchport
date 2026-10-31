package dns

import (
	"context"
	"math/rand/v2"
	"net"
	"testing"
	"time"
)

func TestResolvesThroughARealResolver(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go Serve(pc, []string{"test"})
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", pc.LocalAddr().String())
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	addrs, err := r.LookupHost(ctx, "a.b.Anything.test")
	if err != nil || len(addrs) != 1 || addrs[0] != "127.0.0.1" {
		t.Fatalf("%v, %v", addrs, err)
	}
	if addrs, err := r.LookupHost(ctx, "example.com"); err == nil {
		t.Fatalf("foreign name resolved: %v", addrs)
	}
}

func query(name ...string) []byte {
	q := []byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range name {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	return append(q, 0, 0, 1, 0, 1)
}

func TestRejectsMalformedPackets(t *testing.T) {
	valid := query("a", "test")
	for i := range len(valid) - 1 {
		if _, ok := Answer(valid[:i], []string{"test"}); ok {
			t.Errorf("truncated at %d: answered", i)
		}
	}
	for name, q := range map[string][]byte{
		"label overruns": {0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 200, 'a'},
		"compression":    {0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xC0, 0x0C, 0, 1, 0, 1},
		"two questions":  {0, 1, 1, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1},
		"no questions":   {0, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		if _, ok := Answer(q, []string{"test"}); ok {
			t.Errorf("%s: answered", name)
		}
	}
}

func TestSurvivesRandomInput(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 200_000 {
		b := make([]byte, r.IntN(64))
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		Answer(b, []string{"test"})
	}
}
