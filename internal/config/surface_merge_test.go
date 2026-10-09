package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testMergeHost = "sjomba.local"

// goodMergeTOML is a complete, valid [surface.merge] for testMergeHost.
func goodMergeTOML() string {
	return `[surface.merge]
mode = "manual"
machine = "` + MergeMachineDigest(testMergeHost) + `"
approvers = ["cadence-review", "coderabbit"]
marker_author_id = 4084915
required_reviewers = ["cadence-forge-security-reviewer", "polish"]
method = "squash"
repos = ["cameronsjo/forgectl"]
[surface.merge.workflow]
"cameronsjo/forgectl" = ".github/workflows/ci.yml"
[surface.merge.required_checks]
"cameronsjo/forgectl" = ["build-test", "lint", "macos-test"]
[surface.merge.paths]
"cameronsjo/forgectl" = ["internal/tasks/**", "docs/**"]
`
}

func writeMergeConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMergeMachineDigest(t *testing.T) {
	// sha256("sjomba.local" + "forgectl-merge-v1"), first 12 hex, computed
	// independently (python3 hashlib) on 2026-10-09.
	if got := MergeMachineDigest("sjomba.local"); got != "f8e7a19c22c0" {
		t.Fatalf("digest %q", got)
	}
	if MergeMachineDigest("m5") == MergeMachineDigest("cameron-m5-mbp") {
		t.Fatal("two host names gave one digest")
	}
}

func TestResolveMerge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the merge policy's file checks need a Unix platform")
	}
	check := LocalMergeFileCheck()
	t.Run("a valid file on its machine resolves", func(t *testing.T) {
		s := ResolveMerge(writeMergeConfig(t, goodMergeTOML(), 0o600), testMergeHost, check)
		if s.Mode != MergeManual || s.OffReason != "" || s.MarkerAuthorID != 4084915 || len(s.Repos) != 1 {
			t.Fatalf("settings %+v", s)
		}
		r, ok := s.Repo("CameronSjo/ForgeCtl")
		if !ok || r.Workflow != ".github/workflows/ci.yml" || strings.Join(r.RequiredChecks, ",") != "build-test,lint,macos-test" || len(r.Paths) != 2 {
			t.Fatalf("repo %+v, %v", r, ok)
		}
	})
	offCases := map[string]struct {
		body   string
		mode   os.FileMode
		host   string
		reason string
	}{
		"machine mismatch":    {body: goodMergeTOML(), mode: 0o600, host: "cameron-m5-mbp", reason: "another machine"},
		"mode 0644":           {body: goodMergeTOML(), mode: 0o644, host: testMergeHost, reason: "mode 0644, expected 0600"},
		"mode 0640":           {body: goodMergeTOML(), mode: 0o640, host: testMergeHost, reason: "expected 0600"},
		"mode 0400":           {body: goodMergeTOML(), mode: 0o400, host: testMergeHost, reason: "expected 0600"},
		"missing table":       {body: "log_level = \"info\"\n", mode: 0o600, host: testMergeHost, reason: "[surface.merge] is not set"},
		"mode off":            {body: strings.Replace(goodMergeTOML(), `mode = "manual"`, `mode = "off"`, 1), mode: 0o600, host: testMergeHost, reason: "mode is off"},
		"mode absent":         {body: strings.Replace(goodMergeTOML(), "mode = \"manual\"\n", "", 1), mode: 0o600, host: testMergeHost, reason: "mode is off"},
		"machine absent":      {body: strings.Replace(goodMergeTOML(), "machine = \""+MergeMachineDigest(testMergeHost)+"\"\n", "", 1), mode: 0o600, host: testMergeHost, reason: "machine is not set"},
		"unknown key":         {body: strings.Replace(goodMergeTOML(), "method = ", "methd = ", 1), mode: 0o600, host: testMergeHost, reason: "unknown key"},
		"bad mode":            {body: strings.Replace(goodMergeTOML(), `"manual"`, `"yolo"`, 1), mode: 0o600, host: testMergeHost, reason: "mode: want off, manual or auto"},
		"does not parse":      {body: goodMergeTOML() + "[[[", mode: 0o600, host: testMergeHost, reason: "not valid"},
		"another section bad": {body: goodMergeTOML() + "[surface.intake]\nlables = [\"x\"]\n", mode: 0o600, host: testMergeHost, reason: "not valid"},
	}
	for name, c := range offCases {
		t.Run("off: "+name, func(t *testing.T) {
			s := ResolveMerge(writeMergeConfig(t, c.body, c.mode), c.host, check)
			if s.Mode != MergeOff || !strings.Contains(s.OffReason, c.reason) {
				t.Fatalf("settings mode %q reason %q; want off naming %q", s.Mode, s.OffReason, c.reason)
			}
		})
	}
	t.Run("off: symlink", func(t *testing.T) {
		target := writeMergeConfig(t, goodMergeTOML(), 0o600)
		link := filepath.Join(t.TempDir(), "config.toml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if s := ResolveMerge(link, testMergeHost, check); s.Mode != MergeOff || !strings.Contains(s.OffReason, "symlink") {
			t.Fatalf("symlink: %q %q", s.Mode, s.OffReason)
		}
	})
	t.Run("off: directory", func(t *testing.T) {
		if s := ResolveMerge(t.TempDir(), testMergeHost, check); s.Mode != MergeOff || !strings.Contains(s.OffReason, "not a regular file") {
			t.Fatalf("directory: %q %q", s.Mode, s.OffReason)
		}
	})
	t.Run("off: missing file", func(t *testing.T) {
		if s := ResolveMerge(filepath.Join(t.TempDir(), "absent.toml"), testMergeHost, check); s.Mode != MergeOff || s.OffReason == "" {
			t.Fatalf("missing: %q %q", s.Mode, s.OffReason)
		}
	})
	t.Run("off: a second hard link", func(t *testing.T) {
		path := writeMergeConfig(t, goodMergeTOML(), 0o600)
		if err := os.Link(path, filepath.Join(t.TempDir(), "other.toml")); err != nil {
			t.Fatal(err)
		}
		if s := ResolveMerge(path, testMergeHost, check); s.Mode != MergeOff || !strings.Contains(s.OffReason, "has 2 hard links, expected 1") {
			t.Fatalf("hard link: %q %q", s.Mode, s.OffReason)
		}
	})
	t.Run("off: not owned by the user", func(t *testing.T) {
		other := MergeFileCheck{UID: check.UID + 1}
		if s := ResolveMerge(writeMergeConfig(t, goodMergeTOML(), 0o600), testMergeHost, other); s.Mode != MergeOff || !strings.Contains(s.OffReason, "owned by uid") {
			t.Fatalf("owner: %q %q", s.Mode, s.OffReason)
		}
	})
}

func TestSurfaceMergeConfig_Resolve(t *testing.T) {
	id := func(v int64) *int64 { return &v }
	good := func() SurfaceMergeConfig {
		return SurfaceMergeConfig{
			Mode: "auto", Machine: "0123456789ab", Approvers: []string{"cadence-review"}, MarkerAuthorID: id(4084915),
			RequiredReviewers: []string{"polish"}, Repos: []string{"o/r"},
			Workflow:       map[string]string{"o/r": ".github/workflows/ci.yml"},
			RequiredChecks: map[string][]string{"o/r": {"build-test"}},
			Paths:          map[string][]string{"o/r": {"docs/**"}},
		}
	}
	if s, err := good().Resolve(); err != nil || s.Mode != MergeAuto || s.Method != MergeMethodSquash {
		t.Fatalf("good: %+v, %v", s, err)
	}
	if s, err := (SurfaceMergeConfig{}).Resolve(); err != nil || s.Mode != MergeOff {
		t.Fatalf("empty: %+v, %v", s, err)
	}
	rabbit := good()
	rabbit.Approvers, rabbit.RequiredReviewers = []string{"coderabbit"}, nil
	if s, err := rabbit.Resolve(); err != nil || s.MarkerAuthorID != 4084915 {
		t.Fatalf("coderabbit only, with marker_author_id: %+v, %v", s, err)
	}
	cases := map[string]struct {
		mutate func(*SurfaceMergeConfig)
		want   string
	}{
		"unknown key":           {func(c *SurfaceMergeConfig) { c.unknown = []string{"surface.merge.methd"} }, "unknown key"},
		"bad mode":              {func(c *SurfaceMergeConfig) { c.Mode = "Auto" }, "mode: want off, manual or auto"},
		"bad machine":           {func(c *SurfaceMergeConfig) { c.Machine = "0123456789AB" }, "machine: want the 12 lowercase hex"},
		"short machine":         {func(c *SurfaceMergeConfig) { c.Machine = "0123" }, "machine"},
		"method merge":          {func(c *SurfaceMergeConfig) { c.Method = "merge" }, `method: want "squash"`},
		"unknown approver":      {func(c *SurfaceMergeConfig) { c.Approvers = []string{"chief-of-staff"} }, "approvers: want"},
		"duplicate approver":    {func(c *SurfaceMergeConfig) { c.Approvers = []string{"coderabbit", "coderabbit"} }, "listed twice"},
		"marker author missing": {func(c *SurfaceMergeConfig) { c.MarkerAuthorID = nil }, "marker_author_id: required"},
		"marker author missing, coderabbit only": {func(c *SurfaceMergeConfig) {
			c.Approvers, c.RequiredReviewers, c.MarkerAuthorID = []string{"coderabbit"}, nil, nil
		}, "marker_author_id: required whenever mode is manual or auto"},
		"marker author zero":         {func(c *SurfaceMergeConfig) { c.MarkerAuthorID = id(0) }, "marker_author_id: want"},
		"required reviewers empty":   {func(c *SurfaceMergeConfig) { c.RequiredReviewers = nil }, "required_reviewers: at least one"},
		"bad reviewer name":          {func(c *SurfaceMergeConfig) { c.RequiredReviewers = []string{"Polish"} }, "required_reviewers: want"},
		"repo without workflow":      {func(c *SurfaceMergeConfig) { c.Workflow = nil }, "[surface.merge.workflow]: no entry"},
		"workflow outside workflows": {func(c *SurfaceMergeConfig) { c.Workflow["o/r"] = "ci.yml" }, "directly under .github/workflows"},
		"workflow nested":            {func(c *SurfaceMergeConfig) { c.Workflow["o/r"] = ".github/workflows/x/ci.yml" }, "directly under"},
		"repo without checks":        {func(c *SurfaceMergeConfig) { c.RequiredChecks["o/r"] = nil }, "at least one required check"},
		"bad check name":             {func(c *SurfaceMergeConfig) { c.RequiredChecks["o/r"] = []string{" lint"} }, "check name"},
		"repo without paths":         {func(c *SurfaceMergeConfig) { delete(c.Paths, "o/r") }, "at least one path glob"},
		"table key not on repos":     {func(c *SurfaceMergeConfig) { c.Paths["o/other"] = []string{"docs/**"} }, "is not on repos"},
		"bad repo name":              {func(c *SurfaceMergeConfig) { c.Repos = []string{"r"} }, "want owner/name"},
		"duplicate repo, other case": {func(c *SurfaceMergeConfig) { c.Repos = []string{"o/r", "O/R"} }, "listed twice"},
		"workflow key twice, other case": {func(c *SurfaceMergeConfig) { c.Workflow["O/R"] = ".github/workflows/other.yml" },
			"[surface.merge.workflow]: \"O/R\" and \"o/r\" name the same repository"},
		"checks key twice, other case": {func(c *SurfaceMergeConfig) { c.RequiredChecks["o/R"] = []string{"x"} }, "name the same repository"},
		"paths key twice, other case":  {func(c *SurfaceMergeConfig) { c.Paths["O/r"] = []string{"internal/**"} }, "name the same repository"},
		"bare star glob":               {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"*"} }, "bare *"},
		"bare double-star glob":        {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"**"} }, "bare **"},
		"double star first":            {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"**/x.go"} }, "may only follow"},
		"double star after wildcard":   {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"*/**"} }, "may only follow"},
		"double star inside segment":   {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"docs/**.md"} }, "whole segment"},
		"question mark glob":           {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"docs/?.md"} }, "only wildcards"},
		"dot-dot glob":                 {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"docs/../x"} }, `".." segment`},
		"leading slash glob":           {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"/docs/**"} }, "start with '/'"},
		"empty segment glob":           {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"docs//x"} }, "empty segment"},
		"backslash glob":               {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{`docs\x`} }, "backslash"},
		"control byte glob":            {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"docs/\x1b"} }, "control byte"},
		"non-ASCII glob":               {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"internal/ſurface/**"} }, "only ASCII"},
		"invalid UTF-8 glob":           {func(c *SurfaceMergeConfig) { c.Paths["o/r"] = []string{"docs/\xff/**"} }, "valid UTF-8"},
		"approvers past two entries":   {func(c *SurfaceMergeConfig) { c.Approvers = []string{"a", "b", "c"} }, "at most"},
		"negative marker author id":    {func(c *SurfaceMergeConfig) { c.MarkerAuthorID = id(-4) }, "marker_author_id: want"},
		"duplicate required reviewer":  {func(c *SurfaceMergeConfig) { c.RequiredReviewers = []string{"polish", "polish"} }, "listed twice"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := good()
			c.mutate(&in)
			s, err := in.Resolve()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v; want one naming %q", err, c.want)
			}
			if s.Mode != MergeOff {
				t.Fatalf("an invalid value resolved mode %q; it must be off", s.Mode)
			}
		})
	}
}

// TestMergeConfigRefusedAtLoad pins the intake pattern: an invalid
// [surface.merge] makes the file invalid at load and in ValidatePath.
func TestMergeConfigRefusedAtLoad(t *testing.T) {
	bad := strings.Replace(goodMergeTOML(), "method = ", "methd = ", 1)
	if _, err := DecodeStrict([]byte(bad)); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("DecodeStrict: %v", err)
	}
	if err := ValidatePath(writeMergeConfig(t, bad, 0o600)); err == nil || !strings.Contains(err.Error(), "[surface.merge]") {
		t.Fatalf("ValidatePath: %v", err)
	}
	if _, err := DecodeStrict([]byte(goodMergeTOML())); err != nil {
		t.Fatalf("good: %v", err)
	}
}

func TestMergeGlobs(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/plans/a.md", true},
		{"docs/**", "Docs/a.md", false},
		{"docs/**", "docsx/a.md", false},
		{"internal/tasks/**", "internal/tasks/x.go", true},
		{"internal/tasks/**", "internal/taskss/x.go", false},
		{"internal/cli/tasks*.go", "internal/cli/tasks_list.go", true},
		{"internal/cli/tasks*.go", "internal/cli/tasks/x.go", false},
		{"internal/cli/tasks*.go", "internal/cli/surface_tasks.go", false},
		{"a/**/b.go", "a/b.go", true},
		{"a/**/b.go", "a/x/y/b.go", true},
		{"a/**/b.go", "a/x/y/c.go", false},
		{"README.md", "README.md", true},
		{"README.md", "readme.md", false},
		{"a/*x*y", "a/xxy", true},
		{"a/*x*y", "a/yx", false},
	}
	for _, c := range cases {
		if got := MatchMergeGlob(c.glob, c.path); got != c.want {
			t.Errorf("MatchMergeGlob(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
	if MatchMergeGlob("**", "anything") || MatchMergeGlob("*", "x") {
		t.Error("a refused glob matched")
	}
	for _, p := range []string{"", "/a", "a//b", "a/./b", "a/../b", `a\b`, "a/\x00", "a/\x7f", ".", "internal/ſurface/x.go", "docs/café.md", "docs/\xff.md"} {
		if CheckChangedPath(p) == nil {
			t.Errorf("CheckChangedPath(%q) passed", p)
		}
	}
	if err := CheckChangedPath("internal/tasks/x.go"); err != nil {
		t.Errorf("a clean path: %v", err)
	}
}
