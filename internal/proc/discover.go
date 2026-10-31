package proc

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"
)

var (
	// ErrChildExited means the command stopped before opening a port.
	ErrChildExited = errors.New("command exited before listening on a port")
	// ErrNoListener means the command is running but opened no port in time.
	ErrNoListener = errors.New("command opened no port before the timeout")
)

// Options tune Discover.
type Options struct {
	Assigned int             // port offered through PORT or {port}
	Settle   time.Duration   // wait for further listeners before choosing
	Timeout  time.Duration   // give up after this long
	Exited   <-chan struct{} // closed when the command exits
}

// Result is the port Discover chose, and why.
type Result struct {
	Listener
	HTTP       bool       // the chosen port answered an HTTP request
	Candidates []Listener // every port seen, in the order they opened
	Elapsed    time.Duration
}

// Discover polls a process tree until it finds the port to route to. The
// assigned port wins at once. Otherwise it waits Settle after the first
// listener, so an app port opened just after an HMR or metrics port is seen,
// then prefers the first listener that answers HTTP.
func Discover(ctx context.Context, tr *Tracker, o Options) (Result, error) {
	start := time.Now()
	deadline := start.Add(o.Timeout)
	interval := 25 * time.Millisecond
	var firstSeen time.Time
	var order []Listener
	seen := map[int]bool{}
	for {
		listeners, err := tr.Listeners()
		if err != nil {
			return Result{}, err
		}
		slices.SortFunc(listeners, func(a, b Listener) int { return a.Port - b.Port })
		for _, l := range listeners {
			if l.Port == o.Assigned {
				return Result{Listener: l, HTTP: answersHTTP(ctx, l), Candidates: listeners, Elapsed: time.Since(start)}, nil
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
				r := choose(ctx, order)
				r.Elapsed = time.Since(start)
				return r, nil
			}
		}
		if time.Now().After(deadline) {
			return Result{}, ErrNoListener
		}
		select {
		case <-o.Exited:
			return Result{}, ErrChildExited
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(interval):
		}
		interval = min(interval*2, 200*time.Millisecond)
	}
}

func choose(ctx context.Context, order []Listener) Result {
	for _, l := range order {
		if answersHTTP(ctx, l) {
			return Result{Listener: l, HTTP: true, Candidates: order}
		}
	}
	return Result{Listener: order[0], Candidates: order}
}

func answersHTTP(ctx context.Context, l Listener) bool {
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
