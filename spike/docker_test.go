//go:build darwin

package main

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestDockerPublishedPortIsNotInTree(t *testing.T) {
	const name = "devroute-spike-obs"
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	s := spawn(t, t.TempDir(), nil, "docker", "run", "--rm", "--name", name, "-p", "127.0.0.1:18720:4222", "nats:2.11-alpine")
	d, err := Discover(context.Background(), s.tr, DiscoverOpts{Assigned: 1, Settle: 200 * time.Millisecond, Timeout: 6 * time.Second, Exited: s.exited})
	t.Logf("FW docker run -p -> %+v err=%v", d.Listener, err)
}
