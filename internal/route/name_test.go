package route

import (
	"strings"
	"testing"
)

func TestNewName(t *testing.T) {
	valid := map[string]string{
		"api":                   "api",
		"API":                   "api",
		"api.myapp":             "api.myapp",
		"fix-ui.api":            "fix-ui.api",
		"a1":                    "a1",
		strings.Repeat("a", 63): strings.Repeat("a", 63),
	}
	for in, want := range valid {
		got, err := NewName(in)
		if err != nil {
			t.Errorf("NewName(%q): %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("NewName(%q) = %q, want %q", in, got, want)
		}
	}
	invalid := []string{
		"", ".", "api.", ".api", "a..b", "-api", "api-", "a_b", "a b", "a/b",
		"api\nevil", "127.0.0.1:443", "*.api", "ä",
		strings.Repeat("a", 64),
		strings.Repeat("a.", 127) + "aa",
	}
	for _, in := range invalid {
		if got, err := NewName(in); err == nil {
			t.Errorf("NewName(%q) = %q, want error", in, got)
		}
	}
}
