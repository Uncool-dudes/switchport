package cli

import "testing"

func TestStableExecutable(t *testing.T) {
	const store = "/nix/store/abc-switchport/bin/switchport"
	const profile = "/home/a/.nix-profile/bin/switchport"
	cases := []struct {
		name, exe, onPath, want string
	}{
		{"store path moves to the profile", store, profile, profile},
		{"not on PATH keeps the store path", store, "", store},
		{"PATH is another store path", store, "/nix/store/def/bin/switchport", store},
		{"outside the store stays put", "/usr/local/bin/switchport", profile, "/usr/local/bin/switchport"},
	}
	for _, c := range cases {
		if got := stableExecutable(c.exe, c.onPath); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
