package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// TestPrPrs_SearchIsPinnedToConfiguredHost is the #413 wiring check: the @me
// searches behind `pr prs` name no repository, so newPrCmd must route them
// through githubauth.Runner on [github] host. An ambient GH_HOST naming some
// other host must not win, and on a non-default host the ambient token
// variables are removed from the child — the same pin `review` applies.
func TestPrPrs_SearchIsPinnedToConfiguredHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GH_HOST", "ambient.example.test")

	fake := &exec.FakeRunner{RunFunc: prsRunFunc("[" + prSearchRow("platform/tools", 7) + "]")}
	var cfg config.Config
	cfg.Github.Host = "GHE.Example.test"

	cmd := newPrCmd(module.Deps{Cfg: cfg, Runner: fake})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prs", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("pr prs --json: %v (stderr %q)", err, stderr.String())
	}

	var searches int
	for _, call := range fake.Calls {
		if call.Name != "gh" || len(call.Args) < 2 || call.Args[0] != "search" {
			continue
		}
		searches++
		if got := call.Env["GH_HOST"]; got != "ghe.example.test" {
			t.Errorf("search GH_HOST = %q, want the configured host ghe.example.test", got)
		}
		unset := strings.Join(call.UnsetEnv, " ")
		for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
			if !strings.Contains(unset, k) {
				t.Errorf("search on a non-default host does not remove %s (unset %v)", k, call.UnsetEnv)
			}
		}
	}
	if searches != 3 {
		t.Fatalf("gh search calls = %d, want 3", searches)
	}
	if !strings.Contains(stdout.String(), "platform/tools#7") {
		t.Fatalf("stdout = %q, want the searched PR", stdout.String())
	}
}

// TestPrPrs_InvalidConfiguredHostFailsClosed: a [github] host that fails
// validation must not let the searches run on the ambient host instead. The
// searches degrade (they never spawn), and the rejected value is not echoed.
func TestPrPrs_InvalidConfiguredHostFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	fake := &exec.FakeRunner{RunFunc: prsRunFunc("[" + prSearchRow("platform/tools", 7) + "]")}
	var cfg config.Config
	cfg.Github.Host = "evil.test/\x1b[2J"

	cmd := newPrCmd(module.Deps{Cfg: cfg, Runner: fake})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prs", "--json"})
	_ = cmd.Execute()

	for _, call := range fake.Calls {
		if call.Name == "gh" {
			t.Fatalf("gh ran (%v) under an invalid [github] host", call.Args)
		}
	}
	if out := stdout.String() + stderr.String(); strings.Contains(out, "evil.test") || strings.Contains(out, "\x1b") {
		t.Fatalf("output echoes the rejected host: %q", out)
	}
}

// TestPrPickViewPost_OneHostEndToEnd is the #413 regression for a two-host
// run. With [github] host = ghe.example.test, a PR listed by the pinned @me
// search must be viewed, cloned, and posted to on that same host. A two-part
// `--repo owner/repo` is resolved by gh against GH_HOST or its default host,
// never the checkout, so before the fix the view and the post went to
// github.com's same-named (and squattable) repository.
//
// Mutation that turns it red: in internal/pr/session.go viewPR, run
// `c.run.Run(... "--repo", ref.Slug() ...)` instead of the pinned runner with
// host+"/"+ref.Slug() — the view call then carries no GH_HOST and a two-part
// --repo. The same edit in PostReview (launch.go) reddens the post leg.
func TestPrPickViewPost_OneHostEndToEnd(t *testing.T) {
	t.Setenv("GH_HOST", "")
	for _, tc := range []struct {
		name       string
		configured string
		pick       func(t *testing.T, c *pr.Client) pr.Ref
		wantHost   string
	}{
		{
			name:       "picked from the pinned search",
			configured: "ghe.example.test",
			pick: func(t *testing.T, c *pr.Client) pr.Ref {
				prs, notes, err := c.PRs(context.Background())
				if err != nil || len(notes) != 0 || len(prs) != 1 {
					t.Fatalf("PRs = %v, notes %v, err %v; want one row", prs, notes, err)
				}
				return prs[0].Ref
			},
			wantHost: "ghe.example.test",
		},
		{
			name:       "typed owner/repo#N",
			configured: "ghe.example.test",
			pick: func(t *testing.T, c *pr.Client) pr.Ref {
				ref, err := c.ResolveRef(context.Background(), "platform/tools#7")
				if err != nil {
					t.Fatal(err)
				}
				return ref
			},
			wantHost: "ghe.example.test",
		},
		{
			name:       "URL on another host",
			configured: "ghe.example.test",
			pick: func(t *testing.T, c *pr.Client) pr.Ref {
				ref, err := c.ResolveRef(context.Background(), "https://github.com/platform/tools/pull/7")
				if err != nil {
					t.Fatal(err)
				}
				return ref
			},
			wantHost: "github.com",
		},
		{
			// A bare N takes the checkout remote's host, not [github] host.
			// Mutation that turns it red: resolveOrigin (internal/pr/ref.go)
			// returning an empty host, so the Ref falls back to the configured
			// ghe.example.test while the checkout is on git.other.test.
			name:       "bare N from a checkout on another host",
			configured: "ghe.example.test",
			pick: func(t *testing.T, c *pr.Client) pr.Ref {
				ref, err := c.ResolveRef(context.Background(), "7")
				if err != nil {
					t.Fatal(err)
				}
				return ref
			},
			wantHost: "git.other.test",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				switch {
				case name == "gh" && len(args) >= 2 && args[0] == "search":
					return "[" + prSearchRow("platform/tools", 7) + "]", nil
				case name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "view":
					return "https://git.other.test/platform/tools", nil
				case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view":
					return `{"headRefName":"feature","headRefOid":"abc123",` +
						`"headRepositoryOwner":{"login":"platform"},"headRepository":{"name":"tools"}}`, nil
				}
				return "", nil
			}}
			client := pr.New(fake,
				prGitHubHostOption(fake, tc.configured),
				pr.WithSessionsDir(t.TempDir()),
				pr.WithApprover(func(string) (bool, error) { return true, nil }),
				pr.WithTTYCheck(func() bool { return true }),
			)

			ref := tc.pick(t, client)
			sess, err := client.Prepare(context.Background(), ref, pr.PrepareOpts{Agent: "claude"})
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(sess.Workspace) })
			posted, err := client.PostReview(context.Background(), sess, "body", false)
			if err != nil || !posted {
				t.Fatalf("PostReview = %v, %v; want posted", posted, err)
			}

			var sawView, sawPost, sawClone bool
			for _, call := range fake.Calls {
				argv := strings.Join(call.Args, " ")
				switch call.Name {
				case "gh":
					if len(call.Args) >= 2 && call.Args[0] == "repo" && call.Args[1] == "view" {
						// Checkout-resolved: gh picks the repo from the cwd's
						// remotes, so this one call stays off the pin.
						if _, pinned := call.Env["GH_HOST"]; pinned {
							t.Errorf("gh %s: checkout-resolved call is pinned", argv)
						}
						continue
					}
					if got := call.Env["GH_HOST"]; got != tc.wantHost {
						t.Errorf("gh %s: GH_HOST = %q, want %q", argv, got, tc.wantHost)
					}
					if len(call.Args) >= 2 && call.Args[0] == "pr" {
						sawView = sawView || call.Args[1] == "view"
						sawPost = sawPost || call.Args[1] == "review"
						if !strings.Contains(argv, "--repo "+tc.wantHost+"/platform/tools ") {
							t.Errorf("gh %s: --repo does not name %s/platform/tools", argv, tc.wantHost)
						}
					}
				case "git":
					if strings.Contains(argv, "clone") {
						sawClone = true
						if !strings.Contains(argv, "https://"+tc.wantHost+"/platform/tools") {
							t.Errorf("git %s: clone is not from %s", argv, tc.wantHost)
						}
					}
				}
			}
			if !sawView || !sawPost || !sawClone {
				t.Fatalf("view=%v clone=%v post=%v; want all three legs exercised", sawView, sawClone, sawPost)
			}
		})
	}
}
