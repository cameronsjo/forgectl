//go:build unix

package cli

import (
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// queueTestEnv points the queue at a fresh state directory and returns a git
// repo to enqueue against.
func queueTestEnv(t *testing.T) (module.Deps, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	//nolint:gosec // G204: a fixed tool name with arguments this test constructed
	if out, err := osexec.CommandContext(t.Context(), "git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return module.Deps{Runner: exec.OSRunner{}}, repo
}

func writeBrief(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runQueueCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestSurfaceEnqueue(t *testing.T) {
	deps, repo := queueTestEnv(t)
	first := writeBrief(t, "fix the login page\n")

	out, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), "--repo", repo, "--name", "fix-login", "--brief", first, "--json")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var res enqueueResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if !res.Added || res.State != "queued" || res.BriefSHA256 != worker.BriefSHA256("fix the login page") {
		t.Fatalf("result %+v", res)
	}

	out, err = runQueueCmd(t, newSurfaceEnqueueCmd(deps), "--repo", repo, "--name", "fix-login", "--brief", first)
	if err != nil || !strings.Contains(out, "already queued with this brief: queued") {
		t.Fatalf("same brief: %q, %v", out, err)
	}

	cases := map[string]struct {
		args   []string
		exit   int
		wantIn []string
	}{
		"different brief": {
			[]string{"--name", "fix-login", "--brief", writeBrief(t, "something else")}, exitFailed,
			[]string{worker.BriefSHA256("fix the login page"), worker.BriefSHA256("something else")},
		},
		"@ on the flag":           {[]string{"--name", "w2", "--brief", "@" + first}, exitUsage, []string{"drop the leading '@'"}},
		"@ leading the brief":     {[]string{"--name", "w2", "--brief", writeBrief(t, "@notes.md do this")}, exitUsage, []string{"'@'"}},
		"control char in brief":   {[]string{"--name", "w2", "--brief", writeBrief(t, "x\x1b[2J")}, exitUsage, nil},
		"bad name":                {[]string{"--name", "W2", "--brief", first}, exitUsage, nil},
		"missing brief file":      {[]string{"--name", "w2", "--brief", filepath.Join(t.TempDir(), "nope")}, exitUsage, nil},
		"bad batch":               {[]string{"--name", "w2", "--brief", first, "--batch", "Oct 7"}, exitUsage, nil},
		"not a git checkout repo": {[]string{"--name", "w2", "--brief", first, "--repo", t.TempDir()}, exitUsage, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"--repo", repo}, c.args...)
			_, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), args...)
			if err == nil {
				t.Fatal("accepted")
			}
			if got := ExitCode(err); got != c.exit {
				t.Errorf("exit %d, want %d (%v)", got, c.exit, err)
			}
			for _, w := range c.wantIn {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestSurfaceDequeueAndQueue(t *testing.T) {
	deps, repo := queueTestEnv(t)
	secret := "brief text that must never be listed"
	brief := writeBrief(t, secret)
	for _, name := range []string{"w1", "w2"} {
		if _, err := runQueueCmd(t, newSurfaceEnqueueCmd(deps), "--repo", repo, "--name", name, "--brief", brief, "--batch", "oct-07"); err != nil {
			t.Fatal(err)
		}
	}
	q, err := worker.OpenQueue()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim("w1", "launch-1", time.Now()); err != nil {
		t.Fatal(err)
	}

	_, err = runQueueCmd(t, newSurfaceDequeueCmd(deps), "w1")
	if err == nil || ExitCode(err) != exitFailed || !strings.Contains(err.Error(), "run surface close first") {
		t.Fatalf("dequeue of a claimed row: %v (exit %d)", err, ExitCode(err))
	}
	if _, err := runQueueCmd(t, newSurfaceDequeueCmd(deps), "nope"); ExitCode(err) != exitUsage {
		t.Fatalf("dequeue of a missing row: %v", err)
	}

	out, err := runQueueCmd(t, newSurfaceQueueCmd(deps))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, secret) || !strings.Contains(out, "claimed") || !strings.Contains(out, "oct-07") {
		t.Fatalf("queue text:\n%s", out)
	}

	out, err = runQueueCmd(t, newSurfaceQueueCmd(deps), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res queueResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if strings.Contains(out, secret) || len(res.Rows) != 2 || res.Rows[0].LaunchID != "launch-1" || res.Rows[1].State != "queued" {
		t.Fatalf("queue json:\n%s", out)
	}

	out, err = runQueueCmd(t, newSurfaceDequeueCmd(deps), "w2")
	if err != nil || !strings.Contains(out, "dequeued w2 (was queued)") {
		t.Fatalf("dequeue: %q, %v", out, err)
	}
}

func TestAgeText(t *testing.T) {
	cases := map[time.Duration]string{
		0:                             "0s",
		45 * time.Second:              "45s",
		12*time.Minute + time.Second:  "12m",
		3*time.Hour + 20*time.Minute:  "3h20m",
		50 * time.Hour:                "2d",
		47*time.Hour + 59*time.Minute: "47h59m",
	}
	for d, want := range cases {
		if got := ageText(d); got != want {
			t.Errorf("ageText(%v) = %q, want %q", d, got, want)
		}
	}
}
