package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSweepConfigLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[sweep]\nowner = \"acme\"\nsince = \"2026-10-14\"\nbriefs = \"/tmp/briefs\"\nreviewers = [\"pr-reviewer\", \"security-reviewer\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := LoadPath(path).Sweep
	if c.Owner != "acme" || c.Since != "2026-10-14" || c.Briefs != "/tmp/briefs" || len(c.Reviewers) != 2 || c.Validate() != nil {
		t.Errorf("loaded = %+v (%v)", c, c.Validate())
	}
}

func TestSweepConfigValidateNamesTheKey(t *testing.T) {
	cases := map[string]SweepConfig{
		"sweep.owner":     {Owner: "-bad"},
		"sweep.since":     {Since: "yesterday"},
		"sweep.briefs":    {Briefs: "relative/dir"},
		"sweep.reviewers": {Reviewers: []string{"Bad Name"}},
	}
	for key, c := range cases {
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v", key, err)
		}
	}
	if err := (SweepConfig{}).Validate(); err != nil {
		t.Errorf("zero value: %v", err)
	}
}
