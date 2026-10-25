package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseWorkflow struct {
	Jobs map[string]releaseJob `yaml:"jobs"`
}

type releaseJob struct {
	Environment yaml.Node     `yaml:"environment"`
	Steps       []releaseStep `yaml:"steps"`
}

type releaseStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

func parseReleaseWorkflow(t *testing.T) releaseWorkflow {
	t.Helper()
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release workflow: %v", err)
	}
	var wf releaseWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	return wf
}

// environmentName reads a job's environment, written either as a plain
// string or as a mapping with a name key.
func environmentName(n yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "name" {
				return n.Content[i+1].Value
			}
		}
	}
	return ""
}

// TestReleaseWorkflowAppKeyStaysAwayFromRepoCode pins #780: the job that
// holds RELEASE_APP_PRIVATE_KEY checks out nothing, so it runs no repository
// code, and is not the signing job.
func TestReleaseWorkflowAppKeyStaysAwayFromRepoCode(t *testing.T) {
	wf := parseReleaseWorkflow(t)
	holders := 0
	for name, job := range wf.Jobs {
		holds := false
		for _, s := range job.Steps {
			text := s.Run
			for _, v := range s.With {
				text += "\n" + v
			}
			for _, v := range s.Env {
				text += "\n" + v
			}
			if strings.Contains(text, "RELEASE_APP_PRIVATE_KEY") {
				holds = true
			}
		}
		if !holds {
			continue
		}
		holders++
		for _, s := range job.Steps {
			if strings.HasPrefix(s.Uses, "actions/checkout@") {
				t.Errorf("job %q holds RELEASE_APP_PRIVATE_KEY and checks out the repository", name)
			}
		}
		if env := environmentName(job.Environment); env == "signing" {
			t.Errorf("job %q holds RELEASE_APP_PRIVATE_KEY in the signing environment", name)
		}
	}
	if holders == 0 {
		t.Fatal("no job in release.yml references RELEASE_APP_PRIVATE_KEY; the guard matched nothing")
	}
}

// TestReleaseWorkflowCaskCheckAcceptsGoReleaserCask runs the tap job's cask
// check against a cask GoReleaser wrote for a real release (v0.56.0, copied
// from the tap), with gh stubbed to return that release's asset digests.
func TestReleaseWorkflowCaskCheckAcceptsGoReleaserCask(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	script := ""
	for _, s := range parseReleaseWorkflow(t).Jobs["tap"].Steps {
		if s.Name == "Check cask against the release" {
			script = s.Run
		}
	}
	if script == "" {
		t.Fatal(`tap job has no "Check cask against the release" step`)
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "release", "forgectl-0.56.0.rb"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	const release = `{"assets":[
{"name":"forgectl_0.56.0_darwin_amd64.tar.gz","digest":"sha256:98e1865c862ec16ad846dc765e6d97b844d2b081e84cb3ebc39d078d1a5c5b51"},
{"name":"forgectl_0.56.0_darwin_arm64.tar.gz","digest":"sha256:d6779755ac3b03f098071438ca48181c9153895413add7cbc07126d3342d0700"},
{"name":"forgectl_0.56.0_linux_amd64.tar.gz","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`

	run := func(t *testing.T, cask string) (string, error) {
		t.Helper()
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		if err := os.Mkdir(bin, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte(release), 0o600); err != nil {
			t.Fatal(err)
		}
		gh := "#!/usr/bin/env bash\n[[ \"$*\" == \"api repos/cameronsjo/forgectl/releases/tags/v0.56.0\" ]] || exit 9\ncat \"$RELEASE_JSON\"\n"
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o700); err != nil { //nolint:gosec // G306: test-owned temp file; the stub must be executable
			t.Fatal(err)
		}
		caskPath := filepath.Join(dir, "forgectl.rb")
		if err := os.WriteFile(caskPath, []byte(cask), 0o600); err != nil { //nolint:gosec // G703: a temp path the test itself builds
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
		cmd.Env = append(os.Environ(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"RELEASE_JSON="+filepath.Join(dir, "release.json"),
			"REPO=cameronsjo/forgectl",
			"TAG=v0.56.0",
			"CASK="+caskPath,
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run(t, string(fixture)); err != nil {
		t.Fatalf("cask check rejected the real v0.56.0 cask: %v\n%s", err, out)
	}

	// Control: the same cask with one digest changed must fail, or the
	// passing case above proves nothing.
	tampered := strings.Replace(string(fixture), "d6779755", "e6779755", 1)
	if tampered == string(fixture) {
		t.Fatal("fixture no longer carries the arm64 digest the control edits")
	}
	out, err := run(t, tampered)
	if err == nil {
		t.Fatalf("cask check accepted a cask whose arm64 sha256 does not match the release:\n%s", out)
	}
	if !strings.Contains(out, "arm64 sha256 does not match") {
		t.Fatalf("cask check rejected the tampered cask for another reason:\n%s", out)
	}
}
