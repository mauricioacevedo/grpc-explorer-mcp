package explorer

import "testing"

func TestTargetPolicy(t *testing.T) {
	p, err := NewTargetPolicy(append(DefaultAllowedTargets, "*.svc.cluster.local:443", "10.0.0.*:*"))
	if err != nil {
		t.Fatal(err)
	}
	for target, allowed := range map[string]bool{
		"localhost:50051":              true,
		"LOCALHOST:50051":              true,
		"127.0.0.1:9090":               true,
		"[::1]:50051":                  true,
		"orders.svc.cluster.local:443": true,
		"orders.svc.cluster.local:80":  false,
		"10.0.0.12:50051":              true,
		"10.0.1.12:50051":              false,
		"evil.com:443":                 false,
		"localhost.evil.com:50051":     false,
		"localhost":                    false, // missing port
	} {
		if err := p.Check(target); (err == nil) != allowed {
			t.Errorf("Check(%q) err = %v, want allowed=%v", target, err, allowed)
		}
	}
}

func TestTargetPolicyWildcardAndValidation(t *testing.T) {
	p, err := NewTargetPolicy([]string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Check("anything.example:1"); err != nil {
		t.Errorf("'*' should allow any target: %v", err)
	}
	if _, err := NewTargetPolicy([]string{"[bad"}); err == nil {
		t.Error("expected invalid pattern error")
	}
	if _, err := NewTargetPolicy([]string{" ", ""}); err == nil {
		t.Error("expected error for empty pattern list")
	}
}

func TestSplitMethod(t *testing.T) {
	for in, want := range map[string][2]string{
		"pkg.Svc/Method":  {"pkg.Svc", "Method"},
		"/pkg.Svc/Method": {"pkg.Svc", "Method"},
		"pkg.Svc.Method":  {"pkg.Svc", "Method"},
		"a.b.c.Svc/M":     {"a.b.c.Svc", "M"},
	} {
		svc, m, err := splitMethod(in)
		if err != nil || svc != want[0] || m != want[1] {
			t.Errorf("splitMethod(%q) = %q, %q, %v", in, svc, m, err)
		}
	}
	for _, bad := range []string{"", "Method", "pkg.Svc/", "/"} {
		if _, _, err := splitMethod(bad); err == nil {
			t.Errorf("splitMethod(%q) should fail", bad)
		}
	}
}
