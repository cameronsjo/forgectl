//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/herdradapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func TestParseTagTokens(t *testing.T) {
	got, err := parseTagTokens([]string{"task=PAP-12", "issue=forgectl#1239", "note=a=b"})
	if err != nil {
		t.Fatalf("parseTagTokens: %v", err)
	}
	want := map[string]string{"task": "PAP-12", "issue": "forgectl#1239", "note": "a=b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for _, bad := range [][]string{
		{"novalue"},
		{"bad.key=v"},
		{"k="},
		{"k=a\x07b"},
		{"k=v", "k=w"},
	} {
		if _, err := parseTagTokens(bad); err == nil {
			t.Errorf("parseTagTokens(%q) accepted", bad)
		}
	}
	if _, err := parseTagTokens([]string{"k=" + strings.Repeat("v", herdradapter.MaxTokenValueLen+1)}); !errors.Is(err, herdradapter.ErrBadToken) {
		t.Errorf("a long value: err = %v, want ErrBadToken", err)
	}
}

// tag refuses a call with nothing to do, or a bad token, as a usage error
// before it opens a ledger or reaches herdr.
func TestSurfaceTagRefusesBeforeHerdr(t *testing.T) {
	many := []string{"w1"}
	for i := range herdradapter.MaxTokensPerReport + 1 {
		many = append(many, "--clear-token", "k"+strings.Repeat("x", i))
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no token":                 {[]string{"w1"}, "at least one"},
		"bad token":                {[]string{"w1", "--token", "a.b=c"}, "a.b"},
		"bad name":                 {[]string{"../w1", "--token", "a=b"}, "name"},
		"dash-leading key":         {[]string{"w1", "--token=-x=1"}, "must not start with '-'"},
		"dash-leading cleared key": {[]string{"w1", "--clear-token=-x"}, "must not start with '-'"},
		"bad cleared key":          {[]string{"w1", "--clear-token", "a.b"}, "a.b"},
		"cleared twice":            {[]string{"w1", "--clear-token", "k", "--clear-token", "k"}, "named twice"},
		"set and cleared":          {[]string{"w1", "--token", "k=v", "--clear-token", "k"}, "named twice"},
		"over the cap":             {many, "at most"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			cmd := newSurfaceTagCmd(module.Deps{})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			err := cmd.ExecuteContext(t.Context())
			if ExitCode(err) != exitUsage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v (exit %d), want exit %d naming %q", err, ExitCode(err), exitUsage, tc.want)
			}
		})
	}
}

func TestPriceWorker(t *testing.T) {
	priced := &statusUsage{CostUSD: 1.25, ByModel: json.RawMessage(`[]`), Priced: true, UnpricedModels: []string{}}
	never := func(context.Context, string) *statusUsage { t.Fatal("priced a row with no transcript"); return nil }

	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := priceWorker(t.Context(), worker.Row{Name: "w1", Harness: "claude", Transcript: transcript}, "/repo",
		func(_ context.Context, tr string) *statusUsage {
			if tr != transcript {
				t.Errorf("priced %q", tr)
			}
			return priced
		})
	if got.Usage != priced || got.Reason != "" || got.Repo != "/repo" {
		t.Errorf("priced result = %+v", got)
	}

	got = priceWorker(t.Context(), worker.Row{Name: "w1", Harness: "codex"}, "/repo", never)
	if got.Usage != nil || !strings.Contains(got.Reason, "codex worker") {
		t.Errorf("codex result = %+v", got)
	}
	got = priceWorker(t.Context(), worker.Row{Name: "w1", Harness: "claude"}, "/repo", never)
	if got.Usage != nil || !strings.Contains(got.Reason, "records no transcript") {
		t.Errorf("no-transcript result = %+v", got)
	}
	got = priceWorker(t.Context(), worker.Row{Name: "w1", Harness: "claude", Transcript: filepath.Join(t.TempDir(), "gone.jsonl")}, "/repo", never)
	if got.Usage != nil || !strings.Contains(got.Reason, "transcript not found") {
		t.Errorf("missing-transcript result = %+v", got)
	}
	got = priceWorker(t.Context(), worker.Row{Name: "w1", Harness: "claude", Transcript: transcript}, "/repo",
		func(context.Context, string) *statusUsage { return nil })
	if got.Usage != nil || !strings.Contains(got.Reason, "cadence-hooks") {
		t.Errorf("unpriced result = %+v", got)
	}
}

func TestReportWorkerUsage(t *testing.T) {
	run := func(res workerUsageResult, asJSON bool) (string, error) {
		cmd := &cobra.Command{}
		var o bytes.Buffer
		cmd.SetOut(&o)
		err := reportWorkerUsage(cmd, res, asJSON)
		return o.String(), err
	}
	priced := workerUsageResult{Name: "w1", Repo: "/r", Harness: "claude", Transcript: "/t",
		Usage: &statusUsage{CostUSD: 2.5, ByModel: json.RawMessage(`[]`), Priced: true, UnpricedModels: []string{}}}
	out, err := run(priced, true)
	if err != nil {
		t.Fatalf("priced --json: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal([]byte(out), &keys); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if k := mapKeys(keys); !reflect.DeepEqual(k, []string{"harness", "name", "repo", "transcript", "usage"}) {
		t.Errorf("keys = %v", k)
	}
	if out, err := run(priced, false); err != nil || out != "w1: $2.50\n" {
		t.Errorf("text = %q, %v", out, err)
	}

	none := workerUsageResult{Name: "w1", Repo: "/r", Harness: "codex", Reason: "no transcript"}
	out, err = run(none, true)
	if ExitCode(err) != exitFailed || !strings.Contains(out, `"usage": null`) || !strings.Contains(out, `"reason": "no transcript"`) {
		t.Errorf("unpriced --json: out %q, err %v (exit %d)", out, err, ExitCode(err))
	}
	if _, err := run(none, false); ExitCode(err) != exitFailed || !strings.Contains(err.Error(), "no transcript") {
		t.Errorf("unpriced text: err %v (exit %d)", err, ExitCode(err))
	}
}

// usage with a name takes no --days, and without one takes no --repo.
func TestSurfaceUsageFlagsByMode(t *testing.T) {
	for name, args := range map[string][]string{
		"days with a name":    {"w1", "--days", "7"},
		"repo without a name": {"--repo", "."},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newSurfaceUsageCmd(module.Deps{})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(t.Context()); ExitCode(err) != exitUsage {
				t.Fatalf("err = %v (exit %d), want exit %d", err, ExitCode(err), exitUsage)
			}
		})
	}
}
