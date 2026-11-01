package server

import (
	"testing"
	"time"
)

func TestOIDCConfig(t *testing.T) {
	base := "listen: \":8080\"\n"
	c := must(ParseConfig([]byte(base)))
	if c.OIDC.keyRotation != 30*24*time.Hour {
		t.Fatalf("default rotation %v", c.OIDC.keyRotation)
	}
	c = must(ParseConfig([]byte(base + "oidc: {key_rotation: 7d, login_window: 10m, login_per_ip: 3, login_per_email: 2}\n")))
	if c.OIDC.keyRotation != 7*24*time.Hour || c.OIDC.loginWindow != 10*time.Minute || c.OIDC.LoginPerIP != 3 || c.OIDC.LoginPerEmail != 2 {
		t.Fatalf("%+v", c.OIDC)
	}
	if c = must(ParseConfig([]byte(base + "oidc: {key_rotation: \"0\"}\n"))); c.OIDC.keyRotation != 0 {
		t.Fatalf("rotation off: %v", c.OIDC.keyRotation)
	}
	for _, bad := range []string{"oidc: {key_rotation: soon}", "oidc: {login_window: -5m}", "oidc: {key_rotation: -1d}"} {
		if _, err := ParseConfig([]byte(base + bad + "\n")); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
