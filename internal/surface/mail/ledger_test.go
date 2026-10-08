//go:build unix

package mail

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestLedgerDir(t *testing.T) {
	state := "/state"
	got, err := LedgerDir(state, env(map[string]string{EnvLedger: "/state/surface/abc/", envClaudeSocket: "/tmp/cc-socks/1.sock"}))
	if err != nil || got != "/state/surface/abc" {
		t.Fatalf("worker ledger = %q, %v", got, err)
	}
	if _, err := LedgerDir(state, env(map[string]string{EnvLedger: "relative/dir"})); err == nil {
		t.Fatal("accepted a relative FORGECTL_LEDGER")
	}
	a, err := LedgerDir(state, env(map[string]string{envClaudeSocket: "/tmp/cc-socks/1.sock"}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := LedgerDir(state, env(map[string]string{envClaudeSocket: "/tmp/cc-socks/1.sock"}))
	c, _ := LedgerDir(state, env(map[string]string{envClaudeSocket: "/tmp/cc-socks/2.sock"}))
	if a != b || a == c || !strings.HasPrefix(a, filepath.Join(state, "mail")+string(filepath.Separator)) {
		t.Fatalf("socket-keyed ledgers: %q %q %q", a, b, c)
	}
	if _, err := LedgerDir(state, env(nil)); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("no env: err = %v, want ErrNoLedger", err)
	}
}

func TestSelfName(t *testing.T) {
	r := FileRoster{Dir: t.TempDir()}
	if err := r.Put(Worker{Name: "coord", Harness: HarnessClaude, Coordinator: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := SelfName(r, env(map[string]string{EnvWorker: "pi-1"})); err != nil || got != "pi-1" {
		t.Fatalf("worker = %q, %v", got, err)
	}
	if got, err := SelfName(r, env(nil)); err != nil || got != "coord" {
		t.Fatalf("coordinator = %q, %v", got, err)
	}
	if _, err := SelfName(r, env(map[string]string{EnvWorker: "--all"})); err == nil {
		t.Fatal("accepted a flag-shaped FORGECTL_WORKER")
	}
	// A worker that unsets its own name does not become the coordinator.
	if got, err := SelfName(r, env(map[string]string{EnvLedger: "/state/mail/x"})); !errors.Is(err, ErrNoSelf) {
		t.Fatalf("ledger without a worker name = %q, %v; want ErrNoSelf", got, err)
	}
	// Nor can it send as forgectl's own notices.
	if _, err := SelfName(r, env(map[string]string{EnvWorker: SystemSender})); !errors.Is(err, ErrBadName) {
		t.Fatalf("FORGECTL_WORKER=forgectl: err = %v, want ErrBadName", err)
	}
}
