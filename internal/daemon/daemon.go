// Package daemon runs the long-lived process that serves every route.
package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Uncool-dudes/switchport/internal/ca"
	"github.com/Uncool-dudes/switchport/internal/proxy"
	"github.com/Uncool-dudes/switchport/internal/registry"
)

const (
	lockFile        = "daemon.lock"
	statusFile      = "daemon.json"
	logFile         = "daemon.log"
	fileMode        = 0o600
	refreshInterval = 250 * time.Millisecond
	holdFor         = 2 * time.Second
	shutdownWait    = 2 * time.Second
	readHeaderWait  = 10 * time.Second
	startWait       = 5 * time.Second
	stopWait        = 5 * time.Second
	pollWait        = 25 * time.Millisecond
	httpsPort       = 443
	httpPort        = 80
	fallbackPort    = 8443
)

// ErrRunning means another daemon already serves this state directory.
var ErrRunning = errors.New("daemon already running")

// Status is what a running daemon publishes about itself.
type Status struct {
	PID  int `json:"pid"`
	Port int `json:"port"`
}

// Options configure Run.
type Options struct {
	Dir      string
	Registry *registry.Registry
	CA       *ca.CA
	TLDs     []string
	Port     int
	Log      *slog.Logger
}

// Run serves until ctx ends. The lock is held for the whole run, and the kernel
// drops it if this process dies, so a crash never blocks the next daemon.
func Run(ctx context.Context, o Options) error {
	lock, err := acquire(o.Dir)
	if err != nil {
		return err
	}
	defer lock.Close()

	listeners, port, err := listen(o.Port)
	if err != nil {
		return err
	}
	table := &routeTable{tlds: o.TLDs}
	table.refresh(o.Registry, o.Log)

	srv := &http.Server{
		Handler: proxy.New(table.lookup, proxy.Options{
			TLDs:   o.TLDs,
			Port:   strconv.Itoa(port),
			Hold:   holdFor,
			Routes: table.hosts,
		}),
		TLSConfig:         &tls.Config{GetCertificate: o.CA.GetCertificate},
		ReadHeaderTimeout: readHeaderWait,
	}
	serveErr := make(chan error, len(listeners))
	for _, ln := range listeners {
		go func() { serveErr <- srv.ServeTLS(ln, "", "") }()
	}
	var redirect *http.Server
	if port == httpsPort {
		redirect = serveRedirect(o.Log)
	}

	statusPath := filepath.Join(o.Dir, statusFile)
	if err := writeStatus(statusPath, Status{PID: os.Getpid(), Port: port}); err != nil {
		srv.Close()
		return err
	}
	defer os.Remove(statusPath)
	o.Log.Info("serving", "port", port, "tlds", o.TLDs)

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
			defer cancel()
			if redirect != nil {
				redirect.Close()
			}
			if err := srv.Shutdown(shutdownCtx); err != nil {
				srv.Close()
			}
			return nil
		case err := <-serveErr:
			return err
		case <-ticker.C:
			table.refresh(o.Registry, o.Log)
		}
	}
}

func acquire(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, err
	}
	return f, nil
}

func running(dir string) bool {
	f, err := acquire(dir)
	if err != nil {
		return errors.Is(err, ErrRunning)
	}
	f.Close()
	return false
}

// listen binds port, falling back to a high port when 443 is not allowed.
func listen(port int) ([]net.Listener, int, error) {
	listeners, err := bind(port)
	if err == nil {
		return listeners, port, nil
	}
	if port == httpsPort && errors.Is(err, syscall.EACCES) {
		listeners, err = bind(fallbackPort)
		return listeners, fallbackPort, err
	}
	return nil, 0, err
}

// bind opens loopback listeners. macOS allows an unprivileged low port only on
// a wildcard address, so there the listener is wildcard and loopbackOnly is the
// security boundary.
func bind(port int) ([]net.Listener, error) {
	portStr := strconv.Itoa(port)
	if runtime.GOOS == "darwin" && port < 1024 {
		ln, err := net.Listen("tcp", ":"+portStr)
		if err != nil {
			return nil, err
		}
		return []net.Listener{loopbackOnly{ln}}, nil
	}
	v4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", portStr))
	if err != nil {
		return nil, err
	}
	listeners := []net.Listener{loopbackOnly{v4}}
	// A machine without IPv6 loopback still serves IPv4.
	if v6, err := net.Listen("tcp6", net.JoinHostPort("::1", portStr)); err == nil {
		listeners = append(listeners, loopbackOnly{v6})
	}
	return listeners, nil
}

// loopbackOnly drops non-loopback peers before the TLS handshake.
type loopbackOnly struct{ net.Listener }

// Accept implements net.Listener.
func (l loopbackOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok && a.IP.IsLoopback() {
			return c, nil
		}
		c.Close()
	}
}

// serveRedirect sends http:// to https://. Failing to bind only loses the
// convenience, so it is logged and not fatal.
func serveRedirect(log *slog.Logger) *http.Server {
	listeners, err := bind(httpPort)
	if err != nil {
		log.Warn("no http redirect", "error", err)
		return nil
	}
	srv := &http.Server{
		ReadHeaderTimeout: readHeaderWait,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
		}),
	}
	for _, ln := range listeners {
		go func() { _ = srv.Serve(ln) }()
	}
	return srv
}

// routeTable is the registry as the proxy sees it, swapped whole on refresh so
// a request never waits on the disk.
type routeTable struct {
	tlds    []string
	current atomic.Pointer[map[string]proxy.Backend]
}

func (t *routeTable) refresh(reg *registry.Registry, log *slog.Logger) {
	routes, err := reg.List()
	if err != nil {
		log.Error("read registry", "error", err)
		return
	}
	next := map[string]proxy.Backend{}
	for _, tld := range t.tlds {
		for _, a := range routes.Apps {
			next[a.Name.String()+"."+tld] = proxy.Backend{Addr: a.Addr, H2C: a.H2C}
		}
		for _, a := range routes.Aliases {
			next[a.Name.String()+"."+tld] = proxy.Backend{Addr: a.Addr, H2C: a.H2C}
		}
	}
	t.current.Store(&next)
}

func (t *routeTable) lookup(host string) (proxy.Backend, bool) {
	b, ok := (*t.current.Load())[host]
	return b, ok
}

func (t *routeTable) hosts() []string {
	table := *t.current.Load()
	hosts := make([]string, 0, len(table))
	for h := range table {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	return hosts
}

func writeStatus(path string, s Status) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, fileMode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadStatus returns the running daemon's status, and false if none is running.
func ReadStatus(dir string) (Status, bool) {
	if !running(dir) {
		return Status{}, false
	}
	data, err := os.ReadFile(filepath.Join(dir, statusFile))
	if err != nil {
		return Status{}, false
	}
	var s Status
	if json.Unmarshal(data, &s) != nil {
		return Status{}, false
	}
	return s, true
}

// Ensure starts a detached daemon with argv if none is running, and waits for
// it to serve. Two callers may race here, and the lock keeps one daemon.
func Ensure(dir string, argv []string) (Status, error) {
	if s, ok := ReadStatus(dir); ok {
		return s, nil
	}
	logPath := filepath.Join(dir, logFile)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return Status{}, err
	}
	defer log.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Status{}, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		if s, ok := ReadStatus(dir); ok {
			return s, nil
		}
		select {
		case <-exited:
			// The loser of a start race exits at once, so check for the winner.
			exited = nil
		case <-time.After(pollWait):
		}
	}
	return Status{}, fmt.Errorf("daemon did not start, see %s", logPath)
}

// Stop ends the running daemon and waits for it to release its lock.
func Stop(dir string) error {
	s, ok := ReadStatus(dir)
	if !ok {
		return nil
	}
	if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(stopWait)
	for running(dir) {
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon pid %d did not stop", s.PID)
		}
		time.Sleep(pollWait)
	}
	return nil
}
