package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"syscall"
	"time"
)

// Tracker follows every descendant of a root process. Process groups are not
// enough: pnpm starts its child in a new group, so a group lookup finds no
// listener and a group kill leaves the server running. Known pids are kept
// between refreshes so a child survives its parent exiting and being reparented.
type Tracker struct {
	root  int
	known map[int]bool
}

func NewTracker(root int) *Tracker {
	return &Tracker{root: root, known: map[int]bool{root: true}}
}

// proc is one row of the process table.
type proc struct{ pid, ppid int }

func (t *Tracker) refresh() ([]int, error) {
	procs, err := processTable()
	if err != nil {
		return nil, err
	}
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

// Listeners returns the listening sockets of the live process tree.
func (t *Tracker) Listeners() ([]Listener, error) {
	pids, err := t.refresh()
	if err != nil || len(pids) == 0 {
		return nil, err
	}
	return listenersOf(pids)
}

// Signal sends sig to the whole tree, children first.
func (t *Tracker) Signal(sig syscall.Signal) error {
	pids, err := t.refresh()
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.IntSlice(pids)))
	for _, pid := range pids {
		syscall.Kill(pid, sig)
	}
	return nil
}

// Listener is a TCP socket in LISTEN state owned by a tracked process.
type Listener struct {
	PID  int
	IP   string // "" for a wildcard bind
	Port int
}

// DialHost is the address a proxy should connect to for this listener.
func (l Listener) DialHost() string {
	switch l.IP {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return l.IP
}

func (l Listener) Addr() string {
	return net.JoinHostPort(l.DialHost(), strconv.Itoa(l.Port))
}

var (
	ErrChildExited = errors.New("child exited before listening")
	ErrNoListener  = errors.New("no listener before timeout")
)

type DiscoverOpts struct {
	Assigned int           // port offered through PORT or {port}
	Settle   time.Duration // wait for further listeners before choosing
	Timeout  time.Duration
	Exited   <-chan error // receives when the child exits
}

type Discovered struct {
	Listener
	HTTP       bool
	Candidates []Listener
	Elapsed    time.Duration
	Polls      int
}

// Discover polls a process group until it finds the port to route to. The
// assigned port wins at once. Otherwise it waits Settle after the first
// listener so an app port opened just after an HMR or metrics port is seen,
// then prefers the first listener that answers HTTP.
func Discover(ctx context.Context, tr *Tracker, o DiscoverOpts) (Discovered, error) {
	start := time.Now()
	deadline := start.Add(o.Timeout)
	interval := 25 * time.Millisecond
	var firstSeen time.Time
	var order []Listener
	seen := map[int]bool{}
	polls := 0
	for {
		polls++
		ls, err := tr.Listeners()
		if err != nil {
			return Discovered{}, err
		}
		sort.Slice(ls, func(i, j int) bool { return ls[i].Port < ls[j].Port })
		for _, l := range ls {
			if l.Port == o.Assigned {
				return Discovered{Listener: l, HTTP: probeHTTP(ctx, l), Candidates: ls, Elapsed: time.Since(start), Polls: polls}, nil
			}
			if !seen[l.Port] {
				seen[l.Port] = true
				order = append(order, l)
			}
		}
		if len(order) > 0 {
			if firstSeen.IsZero() {
				firstSeen = time.Now()
			}
			if time.Since(firstSeen) >= o.Settle {
				d := choose(ctx, order)
				d.Elapsed, d.Polls = time.Since(start), polls
				return d, nil
			}
		}
		if time.Now().After(deadline) {
			return Discovered{}, ErrNoListener
		}
		select {
		case err := <-o.Exited:
			return Discovered{}, fmt.Errorf("%w: %v", ErrChildExited, err)
		case <-ctx.Done():
			return Discovered{}, ctx.Err()
		case <-time.After(interval):
		}
		if interval < 200*time.Millisecond {
			interval *= 2
		}
	}
}

func choose(ctx context.Context, order []Listener) Discovered {
	for _, l := range order {
		if probeHTTP(ctx, l) {
			return Discovered{Listener: l, HTTP: true, Candidates: order}
		}
	}
	return Discovered{Listener: order[0], Candidates: order}
}

func probeHTTP(ctx context.Context, l Listener) bool {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://"+l.Addr()+"/", nil)
	if err != nil {
		return false
	}
	res, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	res.Body.Close()
	return true
}

var probeClient = &http.Client{
	Transport:     &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}
