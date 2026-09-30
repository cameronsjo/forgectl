package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// auto-tag.yml's gate job decides whether a version bump is tagged (security
// review F-I1). The logic lives inline in the workflow, so that changing it
// needs the `workflows` permission. These tests pull that exact `run:` block
// out of the workflow file and execute it against a stub `gh` that serves
// canned API responses, so every branch of the decision runs without a
// network or a token.

const (
	releaseRepo    = "cameronsjo/forgectl"
	releaseSHA     = "0123456789abcdef0123456789abcdef01234567"
	releaseVersion = "1.2.3"
	releaseBot     = "artificer-forge-bellows[bot]"
	releaseTitle   = "chore(main): release 1.2.3"
)

// stubGH answers `gh api <path>` from files in STUB_DIR: commits.json for
// the commit-to-PR list and pull.json for the PR itself. While a file named
// unlinked holds a positive count, the list call returns [] and decrements
// it, which models GitHub linking the merge commit to its PR late. A file
// named fail makes every call exit 1, the way gh does on an HTTP error.
const stubGH = `#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == api ]] || { echo "stub gh: unexpected command $*" >&2; exit 64; }
if [[ -e "$STUB_DIR/fail" ]]; then
	echo '{"message":"Server Error"}'
	echo "gh: Server Error (HTTP 500)" >&2
	exit 1
fi
case "$2" in
	repos/*/commits/*/pulls)
		n=0
		[[ -e "$STUB_DIR/unlinked" ]] && n=$(cat "$STUB_DIR/unlinked")
		if ((n > 0)); then
			echo $((n - 1)) > "$STUB_DIR/unlinked"
			echo '[]'
		else
			cat "$STUB_DIR/commits.json"
		fi ;;
	repos/*/pulls/*) cat "$STUB_DIR/pull.json" ;;
	*) echo "stub gh: unexpected path $2" >&2; exit 64 ;;
esac
`

// gateScript returns the `run:` block of the gate job's "Require a gated
// release merge" step, failing the test if the workflow no longer has one.
func gateScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(".github/workflows/auto-tag.yml")
	if err != nil {
		t.Fatalf("read auto-tag.yml: %v", err)
	}
	var wf struct {
		Jobs map[string]struct {
			Environment any `yaml:"environment"`
			Steps       []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse auto-tag.yml: %v", err)
	}
	gate, ok := wf.Jobs["gate"]
	if !ok {
		t.Fatal("auto-tag.yml has no gate job")
	}
	// The gate runs code against the commit it judges, so it must never sit
	// in the environment that holds the App key.
	if gate.Environment != nil {
		t.Fatalf("gate job declares environment %v; it must have none", gate.Environment)
	}
	for _, step := range gate.Steps {
		if step.Name == "Require a gated release merge" && strings.TrimSpace(step.Run) != "" {
			return step.Run
		}
	}
	t.Fatal("gate job has no \"Require a gated release merge\" run step")
	return ""
}

func listedPR(author, title string) string {
	return fmt.Sprintf(`[{"number":42,"title":%q,"merged_at":"2026-09-29T02:00:00Z","user":{"login":%q}}]`, title, author)
}

func pullJSON(author, mergedBy, title, mergeSHA string) string {
	return fmt.Sprintf(`{"number":42,"merged":true,"title":%q,"user":{"login":%q},"merged_by":{"login":%q},"merge_commit_sha":%q}`,
		title, author, mergedBy, mergeSHA)
}

func TestReleaseMergeCheck(t *testing.T) {
	check := filepath.Join(t.TempDir(), "gate.sh")
	if err := os.WriteFile(check, []byte(gateScript(t)), 0o600); err != nil {
		t.Fatalf("write gate script: %v", err)
	}

	tests := []struct {
		name       string
		commits    string
		pull       string
		apiFails   bool
		unlinked   int
		wantErr    bool
		wantGated  string
		wantOutput string
	}{
		{
			name:       "App opens and merges the release PR: tag",
			commits:    listedPR(releaseBot, releaseTitle),
			pull:       pullJSON(releaseBot, releaseBot, releaseTitle, releaseSHA),
			wantGated:  "true",
			wantOutput: "tagging v1.2.3",
		},
		{
			name:       "a person merges the App's release PR: no tag",
			commits:    listedPR(releaseBot, releaseTitle),
			pull:       pullJSON(releaseBot, "cameronsjo", releaseTitle, releaseSHA),
			wantGated:  "false",
			wantOutput: "was merged by 'cameronsjo'",
		},
		{
			name:       "title is not the release title: no tag",
			commits:    listedPR(releaseBot, "chore(main): release 1.2.4"),
			pull:       pullJSON(releaseBot, releaseBot, "chore(main): release 1.2.4", releaseSHA),
			wantGated:  "false",
			wantOutput: "no merged PR titled 'chore(main): release 1.2.3'",
		},
		{
			name:       "a person opened the PR with the release title: no tag",
			commits:    listedPR("cameronsjo", releaseTitle),
			pull:       pullJSON("cameronsjo", releaseBot, releaseTitle, releaseSHA),
			wantGated:  "false",
			wantOutput: "no merged PR titled",
		},
		{
			name:       "the PR merged as a different commit: no tag",
			commits:    listedPR(releaseBot, releaseTitle),
			pull:       pullJSON(releaseBot, releaseBot, releaseTitle, "fedcba9876543210fedcba9876543210fedcba98"),
			wantGated:  "false",
			wantOutput: "merged as fedcba98",
		},
		{
			name:       "the PR has no merged_by: no tag",
			commits:    listedPR(releaseBot, releaseTitle),
			pull:       `{"number":42,"merged":true,"title":"chore(main): release 1.2.3","user":{"login":"artificer-forge-bellows[bot]"},"merged_by":null,"merge_commit_sha":"` + releaseSHA + `"}`,
			wantGated:  "false",
			wantOutput: "was merged by ''",
		},
		{
			name:       "link appears on a later poll: tag",
			commits:    listedPR(releaseBot, releaseTitle),
			pull:       pullJSON(releaseBot, releaseBot, releaseTitle, releaseSHA),
			unlinked:   1,
			wantGated:  "true",
			wantOutput: "yet (attempt 1/2)",
		},
		{
			name:       "the PR itself reads unmerged: no tag",
			commits:    listedPR(releaseBot, releaseTitle),
			pull:       strings.Replace(pullJSON(releaseBot, releaseBot, releaseTitle, releaseSHA), `"merged":true`, `"merged":false`, 1),
			wantGated:  "false",
			wantOutput: "is not merged",
		},
		{
			name:       "the PR itself carries another title: no tag",
			commits:    listedPR(releaseBot, releaseTitle),
			pull:       pullJSON(releaseBot, releaseBot, "chore(main): release 9.9.9", releaseSHA),
			wantGated:  "false",
			wantOutput: "is titled 'chore(main): release 9.9.9'",
		},
		{
			name:       "the PR itself was opened by a person: no tag",
			commits:    listedPR(releaseBot, releaseTitle),
			pull:       pullJSON("cameronsjo", releaseBot, releaseTitle, releaseSHA),
			wantGated:  "false",
			wantOutput: "was opened by 'cameronsjo'",
		},
		{
			name:       "PR number is not a number: fail",
			commits:    `[{"number":"42; rm -rf /","title":"chore(main): release 1.2.3","merged_at":"2026-09-29T02:00:00Z","user":{"login":"artificer-forge-bellows[bot]"}}]`,
			wantErr:    true,
			wantOutput: "is not a number",
		},
		{
			name:       "no PR is linked to the commit: no tag",
			commits:    `[]`,
			wantGated:  "false",
			wantOutput: "no PR is linked",
		},
		{
			name:     "API error: fail",
			apiFails: true,
			wantErr:  true,
		},
		{
			name:       "response is not a list: fail",
			commits:    `{"message":"Not Found"}`,
			wantErr:    true,
			wantOutput: "did not return an array",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, gated, err := runReleaseMergeCheck(t, check, tt.commits, tt.pull, tt.apiFails, tt.unlinked)
			if tt.wantErr && err == nil {
				t.Fatalf("check unexpectedly passed (gated=%q)\n%s", gated, output)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("check unexpectedly failed: %v\n%s", err, output)
			}
			if tt.wantErr && gated != "" {
				t.Fatalf("a failed check still wrote gated=%q", gated)
			}
			if gated != tt.wantGated {
				t.Fatalf("gated = %q, want %q\n%s", gated, tt.wantGated, output)
			}
			if tt.wantOutput != "" && !strings.Contains(output, tt.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, tt.wantOutput)
			}
		})
	}
}

func runReleaseMergeCheck(t *testing.T, check, commits, pull string, apiFails bool, unlinked int) (output, gated string, err error) {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if werr := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); werr != nil {
			t.Fatalf("write %s: %v", name, werr)
		}
	}
	bin := filepath.Join(dir, "bin")
	if merr := os.Mkdir(bin, 0o700); merr != nil {
		t.Fatalf("mkdir: %v", merr)
	}
	write("bin/gh", stubGH)
	// The stub must be executable to stand in for gh on PATH.
	if cerr := os.Chmod(filepath.Join(bin, "gh"), 0o700); cerr != nil { //nolint:gosec // test-owned temp file
		t.Fatalf("chmod stub gh: %v", cerr)
	}
	write("commits.json", commits)
	write("pull.json", pull)
	if apiFails {
		write("fail", "")
	}
	if unlinked > 0 {
		write("unlinked", fmt.Sprint(unlinked))
	}
	outFile := filepath.Join(dir, "github_output")
	write("github_output", "")

	cmd := exec.CommandContext(t.Context(), "bash", check) //nolint:gosec // check is a temp file this test wrote
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_DIR="+dir,
		"GITHUB_REPOSITORY="+releaseRepo,
		"HEAD_SHA="+releaseSHA,
		"VERSION="+releaseVersion,
		"GITHUB_OUTPUT="+outFile,
		"LINK_ATTEMPTS=2",
		"LINK_SLEEP_SECONDS=0",
	)
	out, err := cmd.CombinedOutput()

	written, rerr := os.ReadFile(outFile) //nolint:gosec // outFile is under t.TempDir()
	if rerr != nil {
		t.Fatalf("read GITHUB_OUTPUT: %v", rerr)
	}
	for _, line := range strings.Split(string(written), "\n") {
		if v, ok := strings.CutPrefix(line, "gated="); ok {
			gated = v
		}
	}
	return string(out), gated, err
}
