// Package route holds the domain types for named routes.
package route

import (
	"fmt"
	"strings"
)

const (
	maxNameLen  = 253
	maxLabelLen = 63
)

// Name is a validated route name: one or more DNS labels, such as "api" or
// "api.myapp". The zero value is not a valid name.
type Name struct{ value string }

// NewName validates s and lowercases it, because DNS names are case-insensitive.
func NewName(s string) (Name, error) {
	s = strings.ToLower(s)
	if s == "" {
		return Name{}, fmt.Errorf("route name is empty")
	}
	if len(s) > maxNameLen {
		return Name{}, fmt.Errorf("route name %q is longer than %d characters", s, maxNameLen)
	}
	for label := range strings.SplitSeq(s, ".") {
		if err := checkLabel(label); err != nil {
			return Name{}, fmt.Errorf("route name %q: %w", s, err)
		}
	}
	return Name{value: s}, nil
}

func checkLabel(label string) error {
	if label == "" {
		return fmt.Errorf("empty label")
	}
	if len(label) > maxLabelLen {
		return fmt.Errorf("label %q is longer than %d characters", label, maxLabelLen)
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return fmt.Errorf("label %q starts or ends with a hyphen", label)
	}
	for _, c := range []byte(label) {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return fmt.Errorf("label %q contains %q", label, c)
		}
	}
	return nil
}

func (n Name) String() string { return n.value }

// MarshalText implements encoding.TextMarshaler.
func (n Name) MarshalText() ([]byte, error) { return []byte(n.value), nil }

// UnmarshalText implements encoding.TextUnmarshaler. It validates, so a damaged
// state file cannot introduce a name that NewName would reject.
func (n *Name) UnmarshalText(text []byte) error {
	parsed, err := NewName(string(text))
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}
