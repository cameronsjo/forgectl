// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// TestNewRexAdapter_TakesThePathItsLocatorFinds: the adapter is built on
// whatever absolute path the locator returns (the seam the Rex locator
// fills), and a locator that finds nothing, or a relative path, is
// "unavailable" with a truly nil adapter.
func TestNewRexAdapter_TakesThePathItsLocatorFinds(t *testing.T) {
	a, err := newRexAdapter(func() (string, error) {
		return "/Applications/Rex Beta.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind() != backend.KindRex {
		t.Errorf("Kind = %v", a.Kind())
	}
	if _, err := backend.RequireCapabilities(a); err != nil {
		t.Errorf("the rex adapter cannot clean up after itself: %v", err)
	}
	for name, locate := range map[string]rexLocator{
		"not found": func() (string, error) { return "", errors.New("no rex") },
		"relative":  func() (string, error) { return "rex", nil },
	} {
		a, err := newRexAdapter(locate)
		if !errors.Is(err, errBackendUnavailable) {
			t.Errorf("%s: err = %v, want errBackendUnavailable", name, err)
		}
		if a != nil {
			t.Errorf("%s: a non-nil adapter came back with the error", name)
		}
	}
}

// TestLookPathRex_NotOnPath: with no rex on PATH the default locator refuses.
func TestLookPathRex_NotOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := lookPathRex(); err == nil {
		t.Fatal("lookPathRex found rex on an empty PATH")
	}
}
