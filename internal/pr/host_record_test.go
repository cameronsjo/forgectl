package pr

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// identityPin is the pin tests use: it pins nothing, and the recorded argv
// still shows which host --repo names.
func identityPin(run exec.Runner) func(string) exec.Runner {
	return func(string) exec.Runner { return run }
}

// TestRecordHost_PersistsConcreteHostAcrossAConfigChange: a record names the
// host it was prepared against, so reloading it after [github] host changes
// still views and posts on the original host (#413).
func TestRecordHost_PersistsConcreteHostAcrossAConfigChange(t *testing.T) {
	fake := ghViewRunner()
	dir := t.TempDir()
	before := New(fake, WithSessionsDir(dir), WithGitHubHost("ghe.example.test", identityPin(fake)))
	sess, err := before.Prepare(context.Background(), Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}, PrepareOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sess.Workspace) })

	data, err := os.ReadFile(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	var bc Breadcrumb
	if err := json.Unmarshal(data, &bc); err != nil {
		t.Fatal(err)
	}
	if bc.Host != "ghe.example.test" {
		t.Fatalf("record host = %q, want the concrete host it was prepared on", bc.Host)
	}

	after := New(fake, WithSessionsDir(dir), WithGitHubHost("github.com", identityPin(fake)),
		WithApprover(func(string) (bool, error) { return true, nil }), WithTTYCheck(func() bool { return true }))
	loaded, err := after.loadSession(sess.Path)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if _, err := after.PostReview(context.Background(), loaded, "body", false); err != nil {
		t.Fatalf("PostReview: %v", err)
	}
	if got := strings.Join(fake.Last().Args, " "); !strings.Contains(got, "--repo ghe.example.test/cameronsjo/forgectl ") {
		t.Fatalf("post argv = %q, want the recorded host, not the new configured one", got)
	}
}

// TestRecordHost_LegacyRecordMeansConfiguredHost: a record with no host (one
// written before records carried a host) means the configured [github] host.
func TestRecordHost_LegacyRecordMeansConfiguredHost(t *testing.T) {
	bc := validRecord()
	bc.Host = ""
	ref, err := refFromRecord(bc)
	if err != nil {
		t.Fatalf("refFromRecord: %v", err)
	}
	fake := ghViewRunner()
	c := New(fake, WithGitHubHost("ghe.example.test", identityPin(fake)))
	host, _, err := c.prHost(ref)
	if err != nil || host != "ghe.example.test" {
		t.Fatalf("prHost(legacy) = %q, %v; want the configured host", host, err)
	}
}

// TestRecordHost_HostileRecordHostRefused: a record's host is disk input and
// must pass the same hostname predicate as any other before it can become
// argv or GH_HOST.
func TestRecordHost_HostileRecordHostRefused(t *testing.T) {
	for _, h := range []string{"-evil.test", "evil.test:22", "EVIL\x1b.test", strings.Repeat("a", MaxHostSegmentBytes+1)} {
		bc := validRecord()
		bc.Host = h
		if err := validateBreadcrumbRecord(bc); err == nil {
			t.Errorf("record host %q accepted", h)
		}
	}
}
