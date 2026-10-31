package cli

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestChildEnv(t *testing.T) {
	env := childEnv([]string{"A=1", "REQUESTS_CA_BUNDLE=/mine"}, 42, "https://a.localhost", certFiles{ca: "/ca", bundle: "/bundle"})
	for _, want := range []string{"A=1", "PORT=42", "SWITCHPORT_URL=https://a.localhost", "NODE_EXTRA_CA_CERTS=/ca", "SSL_CERT_FILE=/bundle", "REQUESTS_CA_BUNDLE=/mine"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s in %v", want, env)
		}
	}
	if slices.Contains(env, "REQUESTS_CA_BUNDLE=/bundle") {
		t.Error("overrode the user's value")
	}
}

func TestChildEnvBeforeCAExists(t *testing.T) {
	env := childEnv(nil, 1, "u", certFiles{})
	if strings.Contains(strings.Join(env, " "), "CERT") {
		t.Fatalf("%v", env)
	}
}

func TestDoctorReportsEachFailureWithFix(t *testing.T) {
	freshState(t)
	code, out, _ := cli(t, "doctor")
	if code != 1 {
		t.Fatalf("code %d", code)
	}
	for _, want := range []string{"FAIL  CA trust", "FAIL  daemon", "fix: switchport setup"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestDoctorReportsTakenPort(t *testing.T) {
	freshState(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	t.Setenv(httpsPortEnv, port)

	_, out, _ := cli(t, "doctor")
	if !strings.Contains(out, "FAIL  HTTPS port "+port) {
		t.Errorf("taken port not reported:\n%s", out)
	}

	ln.Close()
	_, out, _ = cli(t, "doctor")
	if !strings.Contains(out, "ok    HTTPS port "+port) {
		t.Errorf("free port reported as taken:\n%s", out)
	}
}
