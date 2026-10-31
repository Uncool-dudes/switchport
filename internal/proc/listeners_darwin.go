package proc

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// listenersOf uses lsof because libproc needs cgo. -a ANDs the pid and socket
// filters; without it lsof ORs them and returns every listener on the machine.
func listenersOf(pids []int) ([]Listener, error) {
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	out, err := exec.Command("lsof", "-nP", "-a", "-p", strings.Join(list, ","), "-iTCP", "-sTCP:LISTEN", "-Fpn").Output()
	var ee *exec.ExitError
	noMatches := errors.As(err, &ee) && ee.ExitCode() == 1
	if err != nil && !noMatches {
		return nil, fmt.Errorf("lsof: %w", err)
	}
	return parseLsof(out), nil
}

func processTable() ([]process, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	var procs []process
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		procs = append(procs, process{pid, ppid})
	}
	return procs, nil
}

func startTime(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("ps: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func parseLsof(out []byte) []Listener {
	var ls []Listener
	pid := 0
	dedupe := map[string]bool{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(line) < 2 {
			continue
		}
		v := string(line[1:])
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(v)
		case 'n':
			host, port, err := net.SplitHostPort(v)
			if err != nil {
				continue
			}
			p, err := strconv.Atoi(port)
			if err != nil {
				continue
			}
			if host == "*" {
				host = ""
			}
			host = strings.Trim(host, "[]")
			key := host + "/" + port
			if !dedupe[key] {
				dedupe[key] = true
				ls = append(ls, Listener{PID: pid, IP: host, Port: p})
			}
		}
	}
	return ls
}
