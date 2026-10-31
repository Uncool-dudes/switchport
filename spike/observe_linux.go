package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processTable() ([]proc, error) {
	dirs, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return nil, err
	}
	var procs []proc
	for _, dir := range dirs {
		stat, err := os.ReadFile(dir + "/stat")
		if err != nil {
			continue
		}
		// comm may contain spaces, so fields are counted after the final ')'.
		i := strings.LastIndexByte(string(stat), ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(stat)[i+1:])
		if len(f) < 2 {
			continue
		}
		pid, _ := strconv.Atoi(filepath.Base(dir))
		ppid, _ := strconv.Atoi(f[1])
		procs = append(procs, proc{pid, ppid})
	}
	return procs, nil
}

// listenersOf matches socket inodes held by pids against /proc/net/tcp{,6}.
func listenersOf(pids []int) ([]Listener, error) {
	inodes := map[string]int{}
	netDir := ""
	for _, pid := range pids {
		dir := "/proc/" + strconv.Itoa(pid)
		fds, err := os.ReadDir(dir + "/fd")
		if err != nil {
			continue
		}
		netDir = dir + "/net"
		for _, fd := range fds {
			link, err := os.Readlink(dir + "/fd/" + fd.Name())
			if err == nil && strings.HasPrefix(link, "socket:[") {
				inodes[link[8:len(link)-1]] = pid
			}
		}
	}
	if netDir == "" {
		return nil, nil
	}
	var ls []Listener
	seen := map[string]bool{}
	for _, name := range []string{"tcp", "tcp6"} {
		f, err := os.Open(netDir + "/" + name)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Scan()
		for sc.Scan() {
			c := strings.Fields(sc.Text())
			if len(c) < 10 || c[3] != "0A" {
				continue
			}
			pid, ok := inodes[c[9]]
			if !ok {
				continue
			}
			ip, port, err := parseProcAddr(c[1])
			if err != nil {
				continue
			}
			key := fmt.Sprint(ip, "/", port)
			if !seen[key] {
				seen[key] = true
				ls = append(ls, Listener{PID: pid, IP: ip, Port: port})
			}
		}
		f.Close()
	}
	return ls, nil
}

func parseProcAddr(s string) (string, int, error) {
	h, p, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, fmt.Errorf("bad addr %q", s)
	}
	port, err := strconv.ParseInt(p, 16, 32)
	if err != nil {
		return "", 0, err
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return "", 0, err
	}
	// /proc prints each 32-bit word in host byte order.
	for i := 0; i+4 <= len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	ip := net.IP(raw)
	if ip.IsUnspecified() {
		return "", int(port), nil
	}
	return ip.String(), int(port), nil
}
