// Package proc finds out what a command tree is doing, without knowing which
// tool started it: which processes belong to it and which ports they opened.
package proc

import (
	"net"
	"strconv"
	"sync"
	"syscall"
)

// Tracker follows every descendant of a root process. Process groups are not
// enough: pnpm starts its child in a new group, so a group lookup finds no
// listener and a group kill leaves the server running. Known pids are kept
// between refreshes so a child survives its parent exiting and being
// reparented.
type Tracker struct {
	root int

	mu    sync.Mutex
	known map[int]bool
}

// NewTracker follows root and everything it starts.
func NewTracker(root int) *Tracker {
	return &Tracker{root: root, known: map[int]bool{root: true}}
}

// process is one row of the process table.
type process struct{ pid, ppid int }

func (t *Tracker) refresh() ([]int, error) {
	procs, err := processTable()
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	alive := make(map[int]bool, len(procs))
	for _, p := range procs {
		alive[p.pid] = true
	}
	for changed := true; changed; {
		changed = false
		for _, p := range procs {
			if t.known[p.ppid] && !t.known[p.pid] {
				t.known[p.pid] = true
				changed = true
			}
		}
	}
	var pids []int
	for pid := range t.known {
		if alive[pid] {
			pids = append(pids, pid)
		} else if pid != t.root {
			delete(t.known, pid)
		}
	}
	return pids, nil
}

// Listeners returns the listening TCP sockets of the live process tree.
func (t *Tracker) Listeners() ([]Listener, error) {
	pids, err := t.refresh()
	if err != nil || len(pids) == 0 {
		return nil, err
	}
	return listenersOf(pids)
}

// Signal sends sig to every live process in the tree.
func (t *Tracker) Signal(sig syscall.Signal) error {
	pids, err := t.refresh()
	if err != nil {
		return err
	}
	for _, pid := range pids {
		// A process may exit between the snapshot and the signal.
		_ = syscall.Kill(pid, sig)
	}
	return nil
}

// Listener is a TCP socket in LISTEN state owned by a tracked process.
type Listener struct {
	PID  int
	IP   string // empty for a wildcard bind
	Port int
}

// DialHost is the address a client should connect to for this listener.
func (l Listener) DialHost() string {
	switch l.IP {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return l.IP
}

// Addr is DialHost joined with the port.
func (l Listener) Addr() string {
	return net.JoinHostPort(l.DialHost(), strconv.Itoa(l.Port))
}
