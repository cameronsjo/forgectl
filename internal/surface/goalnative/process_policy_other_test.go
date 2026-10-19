//go:build !darwin

package goalnative

import "testing"

func TestConfinedCommandFailsClosedOutsideDarwin(t *testing.T) {
	wrapped, err := confinedCommand([]string{"/bin/sh", "-c", "printf unrestricted"})
	if err == nil || wrapped != nil {
		t.Fatalf("confinedCommand = %#v, %v; want unsupported-platform refusal", wrapped, err)
	}
}
