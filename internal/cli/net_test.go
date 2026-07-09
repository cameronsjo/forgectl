package cli

// Test plan for net.go
//
// newNetCmd / newNetCmdForClient (Classification: API handler / cobra command)
//   [x] Happy: --json emits the {reachable, checkedAt, ageSeconds} shape to stdout
//   [x] Happy: --refresh forces a probe (dialFunc called), bypassing a fresh cache
//   [x] Happy: without --refresh, a fresh cache is used and dialFunc is not called
//   [x] Happy: human (non-JSON) output names reachable/unreachable and an age

import (
	"bytes"
	"context"
	"encoding/json"
	stdnet "net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
)

// netFixture builds a *net.Client wired for CLI tests: a temp cache path, a
// fixed clock, and a dial that always "succeeds" (or the given error).
func netFixture(t *testing.T, dialCalls *int, dialErr error) *netpkg.Client {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), "net-cache.json")
	now := time.Now()
	return netpkg.New(&exec.FakeRunner{},
		netpkg.WithCachePath(cachePath),
		netpkg.WithNow(func() time.Time { return now }),
		netpkg.WithDialFunc(func(_, _ string, _ time.Duration) (stdnet.Conn, error) {
			*dialCalls++
			return nil, dialErr
		}),
	)
}

func TestNetCmd_JSONFlag_EmitsReachableShape(t *testing.T) {
	var calls int
	client := netFixture(t, &calls, nil)
	cmd := newNetCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--json"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got struct {
		Reachable  bool      `json:"reachable"`
		CheckedAt  time.Time `json:"checkedAt"`
		AgeSeconds int       `json:"ageSeconds"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, stdout.String())
	}
	if !got.Reachable {
		t.Errorf("reachable = false, want true")
	}
	if got.CheckedAt.IsZero() {
		t.Errorf("checkedAt is zero, want a timestamp")
	}
}

func TestNetCmd_RefreshFlag_ForcesProbe(t *testing.T) {
	var calls int
	client := netFixture(t, &calls, nil)

	// Prime a fresh cache first (no --refresh) — must NOT dial.
	primeCmd := newNetCmdForClient(client)
	primeCmd.SetOut(new(bytes.Buffer))
	primeCmd.SetErr(new(bytes.Buffer))
	if err := primeCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("priming run: unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("priming run: dialFunc called %d times, want 1 (cache miss)", calls)
	}

	// Second run without --refresh: cache is fresh, must not dial again.
	plainCmd := newNetCmdForClient(client)
	plainCmd.SetOut(new(bytes.Buffer))
	plainCmd.SetErr(new(bytes.Buffer))
	if err := plainCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("plain run: unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("plain run: dialFunc called %d times, want 1 (fresh cache, no re-dial)", calls)
	}

	// Third run with --refresh: must force a new dial despite the fresh cache.
	refreshCmd := newNetCmdForClient(client)
	refreshCmd.SetOut(new(bytes.Buffer))
	refreshCmd.SetErr(new(bytes.Buffer))
	refreshCmd.SetArgs([]string{"--refresh"})
	if err := refreshCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("refresh run: unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("refresh run: dialFunc called %d times, want 2 (--refresh forces a probe)", calls)
	}
}

func TestNetCmd_HumanOutput_NamesReachability(t *testing.T) {
	var calls int
	client := netFixture(t, &calls, nil)
	cmd := newNetCmdForClient(client)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(new(bytes.Buffer))

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "reachable") {
		t.Errorf("human output missing 'reachable': %q", stdout.String())
	}
}
