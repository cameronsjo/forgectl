// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package hcadapter

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestStampAgentLabelsTheTerminal(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	run := f.runner()
	a, err := New(run, "/usr/local/bin/hc", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	id := AgentIdentity{
		Kind:    "claude",
		Session: "5e55a1d0-aaaa-4bbb-8ccc-000000000001",
		Worker:  "fix-login",
		Role:    "reviewer",
	}
	if err := a.StampAgent(id); err != nil {
		t.Fatalf("StampAgent: %v", err)
	}
	startWorker(t, f, a)
	var create exec.SensitiveCommand
	for _, c := range run.Calls() {
		if c.Kind == exec.KindHcCreate {
			create = c
		}
	}
	if create.Kind != exec.KindHcCreate {
		t.Fatal("hc run was not recorded")
	}
	for _, want := range []string{
		"agent.kind=claude",
		"agent.session=5e55a1d0-aaaa-4bbb-8ccc-000000000001",
		"agent.worker=fix-login",
		"agent.role=reviewer",
	} {
		if !hasLabel(create, want) {
			t.Errorf("hc run missing --label %s", want)
		}
	}
}

func TestStampAgentOmitsEmptyFields(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	run := f.runner()
	a, err := New(run, "/usr/local/bin/hc", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StampAgent(AgentIdentity{Kind: "codex"}); err != nil {
		t.Fatalf("StampAgent: %v", err)
	}
	startWorker(t, f, a)
	var create exec.SensitiveCommand
	for _, c := range run.Calls() {
		if c.Kind == exec.KindHcCreate {
			create = c
		}
	}
	if !hasLabel(create, "agent.kind=codex") {
		t.Error("hc run missing --label agent.kind=codex")
	}
	for _, k := range []string{LabelAgentSession, LabelAgentWorker, LabelAgentRole} {
		if hasLabel(create, k+"=") {
			t.Errorf("hc run stamped empty label %s", k)
		}
	}
}

func TestStampAgentRefusesInvalidValues(t *testing.T) {
	a := newTestAdapter(t, &fakeHC{coreID: "core1", terminals: map[string]string{}}, "")
	bad := []AgentIdentity{
		{Kind: "bash"},
		{Kind: "CLAUDE"},
		{Session: "-rf"},
		{Session: "a b"},
		{Session: strings.Repeat("a", 65)},
		{Worker: "../escape"},
		{Worker: "w=x"},
		{Role: "role\nnewline"},
		{Role: "-flag"},
	}
	for _, id := range bad {
		if err := a.StampAgent(id); !errors.Is(err, ErrBadAgentIdentity) {
			t.Errorf("StampAgent(%+v) = %v, want ErrBadAgentIdentity", id, err)
		}
	}
}

func TestParseAgentLabels(t *testing.T) {
	got := ParseAgentLabels(map[string]string{
		LabelAgentKind:    "pi",
		LabelAgentSession: "sess_1-a",
		LabelAgentWorker:  "w.1-a_b",
		LabelAgentRole:    "builder",
		"herdr.space":     "sp123",
	})
	want := AgentIdentity{Kind: "pi", Session: "sess_1-a", Worker: "w.1-a_b", Role: "builder"}
	if got != want {
		t.Fatalf("ParseAgentLabels = %+v, want %+v", got, want)
	}

	// Hostile or malformed values are dropped individually without discarding
	// valid sibling labels.
	partial := ParseAgentLabels(map[string]string{
		LabelAgentKind:    "unknown",
		LabelAgentSession: "valid-sess-1",
		LabelAgentWorker:  "../evil",
		LabelAgentRole:    "ok-role",
	})
	wantPartial := AgentIdentity{Session: "valid-sess-1", Role: "ok-role"}
	if partial != wantPartial {
		t.Fatalf("ParseAgentLabels(partial) = %+v, want %+v", partial, wantPartial)
	}

	if empty := ParseAgentLabels(nil); empty != (AgentIdentity{}) {
		t.Fatalf("ParseAgentLabels(nil) = %+v, want zero", empty)
	}
}

func TestCoreSocketExists(t *testing.T) {
	dir := t.TempDir()
	sockDir := filepath.Join(dir, "herdr-core")
	sockPath := filepath.Join(sockDir, "default.sock")
	env := func(k string) string {
		if k == "XDG_RUNTIME_DIR" {
			return dir
		}
		return ""
	}
	if CoreSocketExists(env) {
		t.Fatal("CoreSocketExists = true before socket exists")
	}
	if err := listenUnixTemp(t, sockPath); err != nil {
		t.Skipf("unix socket listen unavailable in sandbox: %v", err)
	}
	if !CoreSocketExists(env) {
		t.Fatal("CoreSocketExists = false with live unix socket")
	}
	// HC_SOCKET overrides all other candidates.
	if CoreSocketExists(func(k string) string {
		if k == "HC_SOCKET" {
			return filepath.Join(dir, "missing.sock")
		}
		return env(k)
	}) {
		t.Fatal("CoreSocketExists = true when HC_SOCKET points to missing path")
	}
	if !CoreSocketExists(func(k string) string {
		if k == "HC_SOCKET" {
			return sockPath
		}
		return ""
	}) {
		t.Fatal("CoreSocketExists = false when HC_SOCKET points to live socket")
	}
}

func listenUnixTemp(t *testing.T, sockPath string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", sockPath)
	if err != nil {
		return err
	}
	t.Cleanup(func() { _ = ln.Close() })
	return nil
}
