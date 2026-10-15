package explorer

import (
	"fmt"
	"net"
	"path"
	"strings"
)

// DefaultAllowedTargets limits the explorer to services on the local machine
// unless the operator opts in to more. Brackets are escaped because they are
// character classes in path.Match syntax.
var DefaultAllowedTargets = []string{"localhost:*", "127.0.0.1:*", `\[::1\]:*`}

// TargetPolicy decides which host:port targets the explorer may connect to.
// Patterns use path.Match syntax, so "*.internal:443" or "10.0.0.*:*" work.
// A single "*" pattern allows every target.
type TargetPolicy struct {
	patterns []string
}

// NewTargetPolicy validates patterns and returns a policy for them.
func NewTargetPolicy(patterns []string) (*TargetPolicy, error) {
	p := &TargetPolicy{}
	for _, raw := range patterns {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		if pattern == "" {
			continue
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("invalid target pattern %q: %w", raw, err)
		}
		p.patterns = append(p.patterns, pattern)
	}
	if len(p.patterns) == 0 {
		return nil, fmt.Errorf("at least one allowed target pattern is required")
	}
	return p, nil
}

// Check returns an error if target is not a valid host:port or is not allowed.
func (p *TargetPolicy) Check(target string) error {
	if _, _, err := net.SplitHostPort(target); err != nil {
		return fmt.Errorf("target %q must be in host:port form: %w", target, err)
	}
	t := strings.ToLower(target)
	for _, pattern := range p.patterns {
		if pattern == "*" {
			return nil
		}
		if ok, _ := path.Match(pattern, t); ok {
			return nil
		}
	}
	return fmt.Errorf("target %q is not allowed; allowed patterns: %s (change with --allow-targets)",
		target, strings.Join(p.patterns, ", "))
}
