package trust

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const resolverDir = "/etc/resolver"

// InstallResolver sends lookups for tld to the DNS server at addr.
func InstallResolver(tld, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	path := filepath.Join(resolverDir, tld)
	content := fmt.Sprintf("nameserver %s\nport %s\n", host, port)
	if err := sudo("mkdir", "-p", resolverDir); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "+ sudo tee %s\n", path)
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(content)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RemoveResolver undoes InstallResolver.
func RemoveResolver(tld string) error {
	return sudo("rm", "-f", filepath.Join(resolverDir, tld))
}

func sudo(args ...string) error {
	fmt.Fprintln(os.Stderr, "+ sudo", strings.Join(args, " "))
	return run("sudo", args...)
}
