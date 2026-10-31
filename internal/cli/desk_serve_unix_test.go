//go:build unix

package cli

import (
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/module"
)

func TestDeskServe_RefusesNonLoopbackBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8765", ":8765", "192.168.1.5:80", "example.com:80"} {
		dir := t.TempDir()
		cmd := newDeskServeCmd(&dir, module.Deps{})
		cmd.SetArgs([]string{"--addr", addr})
		err := cmd.ExecuteContext(t.Context())
		var ec interface{ ExitCode() int }
		if err == nil || !errors.As(err, &ec) || ec.ExitCode() != deskExitUsage {
			t.Errorf("--addr %s: err %v, want a usage error", addr, err)
		}
	}
}

func TestValidHostName(t *testing.T) {
	cases := map[string]bool{
		"mymac.tail1234.ts.net": true,
		"localhost":             true,
		"":                      false,
		"a..b":                  false,
		"evil.com:80":           false,
		"x/y":                   false,
		".lead":                 false,
	}
	for h, want := range cases {
		if got := validHostName(h); got != want {
			t.Errorf("validHostName(%q) = %v, want %v", h, got, want)
		}
	}
}
