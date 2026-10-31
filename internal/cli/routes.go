package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Uncool-dudes/switchport/internal/daemon"
	"github.com/Uncool-dudes/switchport/internal/proc"
	"github.com/Uncool-dudes/switchport/internal/registry"
	"github.com/Uncool-dudes/switchport/internal/route"
	"github.com/Uncool-dudes/switchport/internal/worktree"
)

const (
	stateDirEnv     = "SWITCHPORT_STATE_DIR"
	defaultStateDir = ".switchport"
	defaultTLD      = "localhost"
	httpsPort       = 443
	listJSONVersion = 1
	waitPoll        = 100 * time.Millisecond
)

func stateDir() (string, error) {
	if dir := os.Getenv(stateDirEnv); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, defaultStateDir), nil
}

func openRegistry() (*registry.Registry, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	return registry.Open(dir)
}

// routeURL includes the port only when the daemon could not take 443.
func routeURL(name route.Name) string {
	url := "https://" + name.String() + "." + defaultTLD
	dir, err := stateDir()
	if err != nil {
		return url
	}
	if s, ok := daemon.ReadStatus(dir); ok && s.Port != httpsPort {
		return url + ":" + strconv.Itoa(s.Port)
	}
	return url
}

func qualifiedName(prefix, name string) (route.Name, error) {
	if prefix == "" {
		return route.NewName(name)
	}
	return route.NewName(prefix + "." + name)
}

type listedRoute struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Addr     string `json:"addr"`
	Type     string `json:"type"`
	PID      int    `json:"pid,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

func listed(routes registry.Routes) []listedRoute {
	out := make([]listedRoute, 0, len(routes.Apps)+len(routes.Aliases))
	for _, a := range routes.Apps {
		out = append(out, listedRoute{a.Name.String(), routeURL(a.Name), a.Addr, "app", a.PID, a.Worktree})
	}
	for _, a := range routes.Aliases {
		out = append(out, listedRoute{Name: a.Name.String(), URL: routeURL(a.Name), Addr: a.Addr, Type: "alias"})
	}
	slices.SortFunc(out, func(a, b listedRoute) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func listCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	if _, err := parseMixed(fs, args); err != nil {
		return failUsage(stderr, err)
	}
	reg, err := openRegistry()
	if err != nil {
		return fail(stderr, err)
	}
	routes, err := reg.List()
	if err != nil {
		return fail(stderr, err)
	}
	rows := listed(routes)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err := enc.Encode(struct {
			Version int           `json:"version"`
			Routes  []listedRoute `json:"routes"`
		}{listJSONVersion, rows})
		if err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tURL\tBACKEND\tPID\tTYPE\tWORKTREE")
	for _, r := range rows {
		pid := "-"
		if r.PID != 0 {
			pid = strconv.Itoa(r.PID)
		}
		worktree := r.Worktree
		if worktree == "" {
			worktree = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.URL, r.Addr, pid, r.Type, worktree)
	}
	if err := tw.Flush(); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func hasRoute(routes registry.Routes, name route.Name) bool {
	return slices.ContainsFunc(routes.Apps, func(a route.App) bool { return a.Name == name }) ||
		slices.ContainsFunc(routes.Aliases, func(a route.Alias) bool { return a.Name == name })
}

func getCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	noWorktree := fs.Bool("no-worktree", false, "")
	wait := fs.Duration("wait", 0, "")
	positional, err := parseMixed(fs, args)
	if err != nil {
		return failUsage(stderr, err)
	}
	if len(positional) != 1 {
		return failUsage(stderr, errors.New("get takes one name"))
	}
	plain, err := route.NewName(positional[0])
	if err != nil {
		return fail(stderr, err)
	}
	wanted := plain
	if !*noWorktree {
		dir, err := os.Getwd()
		if err != nil {
			return fail(stderr, err)
		}
		if wanted, err = qualifiedName(worktree.Prefix(ctx, dir), positional[0]); err != nil {
			return fail(stderr, err)
		}
	}
	reg, err := openRegistry()
	if err != nil {
		return fail(stderr, err)
	}
	deadline := time.Now().Add(*wait)
	for {
		routes, err := reg.List()
		if err != nil {
			return fail(stderr, err)
		}
		switch {
		case hasRoute(routes, wanted):
			fmt.Fprintln(stdout, routeURL(wanted))
			return 0
		case hasRoute(routes, plain):
			fmt.Fprintf(stderr, "switchport: no %s in this worktree, using the main checkout\n", plain)
			fmt.Fprintln(stdout, routeURL(plain))
			return 0
		}
		if !time.Now().Before(deadline) {
			return fail(stderr, fmt.Errorf("no route named %s", wanted))
		}
		select {
		case <-ctx.Done():
			return fail(stderr, ctx.Err())
		case <-time.After(waitPoll):
		}
	}
}

func aliasCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("alias", flag.ContinueOnError)
	remove := fs.Bool("remove", false, "")
	force := fs.Bool("force", false, "")
	h2c := fs.Bool("h2c", false, "")
	positional, err := parseMixed(fs, args)
	if err != nil {
		return failUsage(stderr, err)
	}
	wantArgs := 2
	if *remove {
		wantArgs = 1
	}
	if len(positional) != wantArgs {
		return failUsage(stderr, errors.New("alias takes a name and a port, or --remove and a name"))
	}
	name, err := route.NewName(positional[0])
	if err != nil {
		return fail(stderr, err)
	}
	reg, err := openRegistry()
	if err != nil {
		return fail(stderr, err)
	}
	if *remove {
		if err := reg.RemoveAlias(name); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	port, err := strconv.Atoi(positional[1])
	if err != nil || port < 1 || port > 65535 {
		return failUsage(stderr, fmt.Errorf("invalid port %q", positional[1]))
	}
	alias := route.Alias{Name: name, Addr: "127.0.0.1:" + strconv.Itoa(port), H2C: *h2c}
	if err := reg.AddAlias(alias, *force); err != nil {
		var taken *registry.NameTakenError
		if errors.As(err, &taken) {
			return fail(stderr, fmt.Errorf("%w, use --force to replace it", err))
		}
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, routeURL(name))
	return 0
}

func pruneCommand(stdout, stderr io.Writer) int {
	reg, err := openRegistry()
	if err != nil {
		return fail(stderr, err)
	}
	stale, err := reg.Stale()
	if err != nil {
		return fail(stderr, err)
	}
	for _, app := range stale {
		stopRecorded(app.Procs)
		fmt.Fprintln(stdout, "pruned", app.Name)
	}
	if err := reg.RemoveStale(); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func stopRecorded(procs []route.Process) {
	proc.SignalRecorded(procs, syscall.SIGTERM)
	deadline := time.Now().Add(stopGrace)
	for proc.RecordedAlive(procs) {
		if time.Now().After(deadline) {
			proc.SignalRecorded(procs, syscall.SIGKILL)
			return
		}
		time.Sleep(stopPollInterval)
	}
}
