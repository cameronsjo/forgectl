//go:build unix

package sops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/env"
	fcexec "github.com/cameronsjo/forgectl/internal/exec"
)

// TestSetValueRefusesAnOversizedFileByName: SetValue refuses a file over
// MaxDocumentBytes as too large, before IsSOPSFile would call it "not a SOPS
// document", and before sops runs. A stand-in sops on PATH only has to
// exist; the refusal comes first.
//
// Mutation that turns it red: drop the CheckSize call from setLocked (the
// refusal reads "not a SOPS document").
func TestSetValueRefusesAnOversizedFileByName(t *testing.T) {
	bin := t.TempDir()
	//nolint:gosec // G306: a stand-in executable must be executable; it is never run
	if err := os.WriteFile(filepath.Join(bin, "sops"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "secrets.sops.yaml"), padTo(t, MaxDocumentBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := env.ResolveTarget("secrets.sops.yaml", repo)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	defer target.Close()

	_, err = NewClient(fcexec.NewOSSensitiveRunner()).SetValue(t.Context(), target, "key", "v")
	if err == nil || !strings.Contains(err.Error(), "larger than 4 MiB") {
		t.Fatalf("SetValue on an oversized file: %v, want the size limit named", err)
	}
}
