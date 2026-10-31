package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/Uncool-dudes/switchport/internal/daemon"
	"github.com/Uncool-dudes/switchport/internal/trust"
)

const probeTimeout = 2 * time.Second

func setupCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	tld := fs.String("tld", "", "")
	if _, err := parseMixed(fs, args); err != nil {
		return failUsage(stderr, err)
	}
	if _, err := openRegistry(); err != nil {
		return fail(stderr, err)
	}
	if *tld != "" {
		if err := addTLD(*tld); err != nil {
			return fail(stderr, err)
		}
	}
	authority, err := loadCA()
	if err != nil {
		return fail(stderr, err)
	}
	if trust.Installed(authority.Path()) {
		fmt.Fprintln(stdout, "CA already trusted")
	} else {
		fmt.Fprintln(stdout, "trusting", authority.Path(), "(the system may ask for approval)")
		if err := trust.Install(authority.Path()); err != nil {
			return fail(stderr, err)
		}
	}
	if *tld != "" && *tld != defaultTLD {
		fmt.Fprintf(stdout, "sending .%s lookups to %s\n", *tld, daemon.DNSAddr)
		if err := trust.InstallResolver(*tld, daemon.DNSAddr); err != nil {
			return fail(stderr, err)
		}
	}
	if err := ensureDaemon(); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "ready")
	return 0
}

type check struct {
	name string
	err  error
	fix  string
}

func doctorCommand(ctx context.Context, stdout, stderr io.Writer) int {
	dir, err := stateDir()
	if err != nil {
		return fail(stderr, err)
	}
	checks := runChecks(ctx, dir)
	failed := 0
	for _, c := range checks {
		if c.err == nil {
			fmt.Fprintf(stdout, "ok    %s\n", c.name)
			continue
		}
		failed++
		fmt.Fprintf(stdout, "FAIL  %s: %v\n      fix: %s\n", c.name, c.err, c.fix)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// runChecks only reads, so doctor is safe to run on a broken install.
func runChecks(ctx context.Context, dir string) []check {
	checks := []check{checkRegistry(dir)}
	caPath := filepath.Join(dir, caFile)
	checks = append(checks, checkCA(caPath), checkDaemon(dir))
	if _, up := daemon.ReadStatus(dir); !up {
		checks = append(checks, checkPort(ctx))
	}
	for _, tld := range tlds() {
		checks = append(checks, checkResolution(ctx, tld))
	}
	return checks
}

func checkRegistry(dir string) check {
	c := check{name: "registry", fix: "inspect " + filepath.Join(dir, "routes.json")}
	if _, err := os.Stat(filepath.Join(dir, "routes.json")); errors.Is(err, os.ErrNotExist) {
		return c
	}
	reg, err := openRegistry()
	if err != nil {
		c.err = err
		return c
	}
	_, c.err = reg.List()
	return c
}

func checkCA(caPath string) check {
	c := check{name: "CA trust", fix: "switchport setup"}
	if _, err := os.Stat(caPath); err != nil {
		c.err = errors.New("no CA yet")
		return c
	}
	if !trust.Installed(caPath) {
		c.err = errors.New("the system does not trust the CA")
	}
	return c
}

func checkDaemon(dir string) check {
	c := check{name: "daemon", fix: "switchport setup, or see " + filepath.Join(dir, "daemon.log")}
	s, ok := daemon.ReadStatus(dir)
	if !ok {
		c.err = errors.New("not running")
		return c
	}
	c.name = "daemon, port " + strconv.Itoa(s.Port)
	return c
}

// checkPort dials instead of binding: a wildcard bind on macOS can raise a
// firewall prompt, and doctor must change nothing.
func checkPort(ctx context.Context) check {
	c := check{name: "HTTPS port"}
	port, err := configuredPort()
	if err != nil {
		c.err = err
		c.fix = "unset " + httpsPortEnv
		return c
	}
	c.name = "HTTPS port " + strconv.Itoa(port)
	c.fix = "stop what holds it, or set " + httpsPortEnv
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var d net.Dialer
	for _, host := range []string{"127.0.0.1", "::1"} {
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		conn.Close()
		c.err = fmt.Errorf("something else is listening on %s", host)
		return c
	}
	return c
}

// checkResolution goes through the system resolver, because dig ignores both
// /etc/hosts and /etc/resolver.
func checkResolution(ctx context.Context, tld string) check {
	host := "doctor-probe." + tld
	c := check{name: "resolving " + host, fix: "switchport setup --tld test, to use a TLD switchport can resolve"}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		c.err = err
		return c
	}
	loopback := func(a string) bool {
		ip := net.ParseIP(a)
		return ip != nil && ip.IsLoopback()
	}
	if !slices.ContainsFunc(addrs, loopback) {
		c.err = fmt.Errorf("resolved to %v, not loopback", addrs)
	}
	return c
}

func uninstallCommand(stdout, stderr io.Writer) int {
	dir, err := stateDir()
	if err != nil {
		return fail(stderr, err)
	}
	if err := daemon.Stop(dir); err != nil {
		return fail(stderr, err)
	}
	if err := removeService(); err != nil {
		fmt.Fprintln(stderr, "switchport: service:", err)
	}
	for _, tld := range tlds() {
		if tld != defaultTLD {
			if err := trust.RemoveResolver(tld); err != nil {
				fmt.Fprintln(stderr, "switchport: resolver:", err)
			}
		}
	}
	caPath := filepath.Join(dir, caFile)
	if _, err := os.Stat(caPath); err == nil && trust.Installed(caPath) {
		if err := trust.Remove(caPath); err != nil {
			// Keeping the state means the CA can still be identified and removed.
			return fail(stderr, fmt.Errorf("remove CA trust: %w, state kept in %s", err, dir))
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, "removed", dir)
	return 0
}
