package trust

import (
	"fmt"
	"net"
)

// InstallResolver has no single form across Linux DNS setups, so it states the
// systemd-resolved commands instead of guessing.
func InstallResolver(tld, addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return err
	}
	return fmt.Errorf("configure .%s by hand; with systemd-resolved: resolvectl dns lo %s && resolvectl domain lo '~%s'", tld, addr, tld)
}

// RemoveResolver is a no-op, because InstallResolver changes nothing.
func RemoveResolver(string) error { return nil }
