package gitleaks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

// canary is a fake secret planted in every field gitleaks writes the secret
// into. It must surface nowhere forgectl keeps a value.
const canary = "ghp_CANARYc4n4ryC4N4RYc4n4ryC4N4RY0000" //nolint:gosec // G101: a fake token, the canary the redaction tests look for

// argValue returns the value after flag in args.
func argValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("argv %q has no %s", args, flag)
	}
	return args[i+1]
}

// reportJSON renders findings as gitleaks would, each with the canary in
// Secret, Match and Line.
func reportJSON(t *testing.T, files ...string) string {
	t.Helper()
	var rows []map[string]any
	for i, f := range files {
		rows = append(rows, map[string]any{
			"RuleID": "github-pat", "Description": "GitHub PAT " + canary,
			"StartLine": i + 1, "EndLine": i + 1, "StartColumn": 3, "EndColumn": 40,
			"Match": "token = " + canary, "Secret": canary, "Line": "token = " + canary,
			"File": f, "SymlinkFile": "", "Commit": "", "Entropy": 4.5,
			"Author": "", "Email": "", "Date": "", "Message": canary, "Tags": []string{canary},
			"Fingerprint": f + ":github-pat:" + fmt.Sprint(i+1),
		})
	}
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writingRunner answers each scan by writing report(repo) to the argv's
// --report-path, as gitleaks does, and records each call.
func writingRunner(t *testing.T, report func(repo string) string) *fexec.FakeRunner {
	t.Helper()
	return &fexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		repo := args[len(args)-1]
		body := report(repo)
		if body == "" {
			return "", nil
		}
		return "", os.WriteFile(filepath.Clean(argValue(t, args, "--report-path")), []byte(body), 0o600)
	}}
}

// TestArgs_Golden pins the argv: `dir`, forgectl's own config, the private
// ignore path, a redacted JSON report into the temp dir, exit 0 on leaks,
// and the repo after "--". Never `git` mode, never --follow-symlinks.
//
// Mutations that turn it red: drop --config, --redact or
// --gitleaks-ignore-path; switch "dir" to "git" or "detect"; add
// --follow-symlinks.
func TestArgs_Golden(t *testing.T) {
	got := Args("/p/repo", "/tmp/w")
	want := []string{
		"dir",
		"--config", filepath.Join("/tmp/w", "cfg.toml"),
		"--gitleaks-ignore-path", "/tmp/w",
		"--report-format", "json",
		"--report-path", filepath.Join("/tmp/w", "report.json"),
		"--redact",
		"--exit-code", "0",
		"--no-banner",
		"--log-level", "error",
		"--max-target-megabytes", "5",
		"--",
		"/p/repo",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Args = %q\nwant   %q", got, want)
	}
	for _, banned := range []string{"git", "detect", "protect", "--follow-symlinks", "--log-opts"} {
		if slices.Contains(got, banned) {
			t.Errorf("argv carries %q", banned)
		}
	}
	if got[0] != "dir" {
		t.Errorf("subcommand = %q, want dir", got[0])
	}
}

// TestScan_EnvAndTempHygiene pins the environment and the work dir: every
// call removes GITLEAKS_CONFIG and GITLEAKS_CONFIG_TOML, the work dir is
// 0700 with forgectl's config 0600 in it while gitleaks runs, and it is gone
// after.
//
// Mutations that turn it red: drop either name from unsetEnv; skip the
// deferred cleanup; write the config 0644.
func TestScan_EnvAndTempHygiene(t *testing.T) {
	var tmp string
	fr := &fexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		tmp = argValue(t, args, "--gitleaks-ignore-path")
		info, err := os.Stat(tmp)
		if err != nil {
			return "", err
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("work dir mode %#o, want 0700", info.Mode().Perm())
		}
		cfg := argValue(t, args, "--config")
		b, err := os.ReadFile(filepath.Clean(cfg))
		if err != nil || string(b) != configTOML {
			t.Errorf("config = %q, %v; want %q", b, err, configTOML)
		}
		if ci, err := os.Stat(cfg); err == nil && runtime.GOOS != "windows" && ci.Mode().Perm() != 0o600 {
			t.Errorf("config mode %#o, want 0600", ci.Mode().Perm())
		}
		return "", os.WriteFile(filepath.Clean(argValue(t, args, "--report-path")), []byte("[]"), 0o600)
	}}
	res := Scan(context.Background(), fr, "/opt/bin/gitleaks", []string{"/p/a", "/p/b"}, time.Minute)
	if res.Status != StatusRan || res.ReposScanned != 2 {
		t.Errorf("result = %+v, want ran over 2 repos", res)
	}
	for _, c := range fr.Calls {
		if c.Name != "/opt/bin/gitleaks" {
			t.Errorf("ran %q, want the resolved absolute path", c.Name)
		}
		for _, v := range []string{"GITLEAKS_CONFIG", "GITLEAKS_CONFIG_TOML"} {
			if !slices.Contains(c.UnsetEnv, v) {
				t.Errorf("call unsets %v, want %s removed", c.UnsetEnv, v)
			}
		}
		if len(c.Env) != 0 {
			t.Errorf("call sets env %v; gitleaks gets no overrides", c.Env)
		}
	}
	if _, err := os.Stat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("work dir %s survived the scan: %v", tmp, err)
	}
}

// TestScan_CanaryNeverDecoded: a report holding the canary in Secret, Match,
// Line, Description, Message and Tags yields findings that carry none of
// it. Mutation that turns it red: add a Secret or Match field to
// wireFinding and copy it onto Finding.
func TestScan_CanaryNeverDecoded(t *testing.T) {
	fr := writingRunner(t, func(repo string) string { return reportJSON(t, filepath.Join(repo, "conf.txt")) })
	res := Scan(context.Background(), fr, "/bin/gitleaks", []string{"/p/a"}, time.Minute)
	if len(res.Findings) != 1 {
		t.Fatalf("findings = %+v, want 1", res.Findings)
	}
	if dump := fmt.Sprintf("%#v %+v", res, res); strings.Contains(dump, canary) {
		t.Errorf("the canary reached a decoded value: %s", dump)
	}
	f := res.Findings[0]
	if f.File != "/p/a/conf.txt" || f.RuleID != "github-pat" || f.StartLine != 1 || f.Repo != "/p/a" {
		t.Errorf("finding = %+v", f)
	}
}

// TestScan_RejectsFilesOutsideTheRepo: a finding whose File is relative,
// cleans to outside the repo, or names the repo itself is dropped and
// counted. Mutation that turns it red: accept any absolute File.
func TestScan_RejectsFilesOutsideTheRepo(t *testing.T) {
	fr := writingRunner(t, func(repo string) string {
		return reportJSON(t,
			filepath.Join(repo, "ok.txt"),
			"/etc/passwd",
			filepath.Join(repo, "..", "sibling", "x.txt"),
			repo+"-evil/x.txt",
			"relative/x.txt",
			repo,
		)
	})
	res := Scan(context.Background(), fr, "/bin/gitleaks", []string{"/p/a"}, time.Minute)
	if len(res.Findings) != 1 || res.Findings[0].File != "/p/a/ok.txt" || res.Rejected != 5 {
		t.Errorf("findings=%+v rejected=%d, want only ok.txt and 5 rejected", res.Findings, res.Rejected)
	}
}

// TestScan_NestedReposDeduped: a nested repo's finding, reported by both its
// own scan and its parent's, is listed once, under the nested repo.
func TestScan_NestedReposDeduped(t *testing.T) {
	inner := "/p/a/libs/b"
	fr := writingRunner(t, func(string) string { return reportJSON(t, inner+"/k.txt") })
	res := Scan(context.Background(), fr, "/bin/gitleaks", []string{"/p/a", inner}, time.Minute)
	if len(res.Findings) != 1 || res.Findings[0].Repo != inner {
		t.Errorf("findings = %+v, want one, attributed to %s", res.Findings, inner)
	}
}

// TestScan_Failures: a nonzero exit, a missing report (gitleaks exits 0 on a
// target it cannot find) and a malformed report each fail that repo; the
// others still run, and the pass reports failed.
func TestScan_Failures(t *testing.T) {
	fr := &fexec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		repo := args[len(args)-1]
		rp := filepath.Clean(argValue(t, args, "--report-path"))
		switch repo {
		case "/p/exit":
			return "", &fexec.CommandError{Name: "gitleaks", Stderr: canary, ExitCode: 1, Err: errors.New("exit status 1")}
		case "/p/noreport":
			return "", nil
		case "/p/garbage":
			return "", os.WriteFile(rp, []byte(`{"not":"an array"}`), 0o600)
		}
		return "", os.WriteFile(rp, []byte(reportJSON(t, repo+"/x")), 0o600)
	}}
	res := Scan(context.Background(), fr, "/bin/gitleaks", []string{"/p/exit", "/p/good", "/p/noreport", "/p/garbage"}, time.Minute)
	if res.Status != StatusFailed || res.ReposFailed != 3 || res.ReposScanned != 1 || len(res.Findings) != 1 {
		t.Errorf("result = %+v, want failed, 3 failed, 1 scanned, 1 finding", res)
	}
	if strings.Contains(fmt.Sprintf("%#v", res), canary) {
		t.Error("a CommandError's text reached the result")
	}
}

// ctxRunner blocks each call until its context ends.
type ctxRunner struct{ calls int }

func (r *ctxRunner) RunWithEnvFiltered(ctx context.Context, _ map[string]string, _ []string, _ string, _ ...string) (string, error) {
	r.calls++
	<-ctx.Done()
	return "", ctx.Err()
}

// TestScan_TimeoutStopsThePass: the deadline covers the whole pass: once it
// passes, the pass is timed_out and no further repo is started. Mutation
// that turns it red: give each repo its own fresh deadline.
func TestScan_TimeoutStopsThePass(t *testing.T) {
	r := &ctxRunner{}
	start := time.Now()
	res := Scan(context.Background(), r, "/bin/gitleaks", []string{"/p/a", "/p/b", "/p/c"}, 50*time.Millisecond)
	if res.Status != StatusTimedOut || r.calls != 1 {
		t.Errorf("status=%s calls=%d, want timed_out after one call", res.Status, r.calls)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the deadline did not bound the pass")
	}
}

// bigReader yields n bytes of 'a'.
type bigReader struct{ n int64 }

func (b *bigReader) Read(p []byte) (int, error) {
	if b.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > b.n {
		p = p[:b.n]
	}
	for i := range p {
		p[i] = 'a'
	}
	b.n -= int64(len(p))
	return len(p), nil
}

// TestDecodeReport_Caps: a report past maxReportBytes fails as too large;
// past the findings budget, decoding stops and says it truncated; null is
// empty; anything but an array is malformed.
//
// Mutations that turn it red: drop the LimitedReader (the oversize case then
// decodes); ignore budget.
func TestDecodeReport_Caps(t *testing.T) {
	huge := io.MultiReader(strings.NewReader(`[{"RuleID":"r","File":"/p/x","Secret":"`), &bigReader{n: maxReportBytes + 10}, strings.NewReader(`"}]`))
	if _, _, err := decodeReport(huge, 10); !errors.Is(err, errReportTooLarge) {
		t.Errorf("oversize report: err = %v, want errReportTooLarge", err)
	}
	three := `[{"File":"/p/1"},{"File":"/p/2"},{"File":"/p/3"}]`
	got, truncated, err := decodeReport(strings.NewReader(three), 2)
	if err != nil || !truncated || len(got) != 2 {
		t.Errorf("budget 2: got %d, truncated %v, err %v; want 2, true, nil", len(got), truncated, err)
	}
	got, truncated, err = decodeReport(strings.NewReader(three), 3)
	if err != nil || truncated || len(got) != 3 {
		t.Errorf("budget 3: got %d, truncated %v, err %v; want 3, false, nil", len(got), truncated, err)
	}
	if got, _, err := decodeReport(strings.NewReader("null"), 5); err != nil || len(got) != 0 {
		t.Errorf("null: %v, %v", got, err)
	}
	for _, bad := range []string{`{}`, `"x"`, `[1,`, ``} {
		if _, _, err := decodeReport(strings.NewReader(bad), 5); !errors.Is(err, errReportMalformed) {
			t.Errorf("%q: err = %v, want errReportMalformed", bad, err)
		}
	}
}

// TestScan_FindingsCapTruncates: the pass stops at MaxFindings and says so.
func TestScan_FindingsCapTruncates(t *testing.T) {
	files := make([]string, MaxFindings+1)
	for i := range files {
		files[i] = fmt.Sprintf("/p/a/f%d", i)
	}
	body := reportJSON(t, files...)
	fr := writingRunner(t, func(string) string { return body })
	res := Scan(context.Background(), fr, "/bin/gitleaks", []string{"/p/a", "/p/b"}, time.Minute)
	if !res.Truncated || len(res.Findings) != MaxFindings || len(fr.Calls) != 1 {
		t.Errorf("truncated=%v findings=%d calls=%d, want true, %d, 1", res.Truncated, len(res.Findings), len(fr.Calls), MaxFindings)
	}
}

// TestAtLeast pins the version gate.
func TestAtLeast(t *testing.T) {
	cases := map[string]bool{
		"8.19.0": true, "8.19.1": true, "8.30.1": true, "9.0": true, "v8.19.0": true, "8.19.0-rc1": true,
		"8.18.4": false, "7.99.99": false, "8": false, "x.y.z": false, "": false, "8.19.0.1": false,
	}
	for v, want := range cases {
		if got := AtLeast(v, MinVersion); got != want {
			t.Errorf("AtLeast(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestResolve_States pins every way Resolve can come out.
//
// Mutations that turn it red: drop the ErrDot check (the relative row then
// runs a version); drop the under-root check; accept a version below
// MinVersion; drop UnsetEnv from the version call.
func TestResolve_States(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outBin := filepath.Join(outside, "gitleaks")
	inBin := filepath.Join(root, "tools", "gitleaks")
	linkDir := t.TempDir()
	linkBin := filepath.Join(linkDir, "gitleaks")
	for _, p := range []string{outBin, inBin} {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	symlinks := runtime.GOOS != "windows"
	if symlinks {
		if err := os.Symlink(inBin, linkBin); err != nil {
			t.Fatal(err)
		}
	}
	version := func(out string, err error) *fexec.FakeRunner {
		return &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) { return out, err }}
	}
	look := func(p string, err error) func(string) (string, error) {
		return func(name string) (string, error) {
			if name != Name {
				t.Errorf("looked up %q", name)
			}
			return p, err
		}
	}
	type tc struct {
		name   string
		look   func(string) (string, error)
		run    *fexec.FakeRunner
		state  string
		reason string
		ver    string
		calls  int
	}
	cases := []tc{
		{"absent", look("", exec.ErrNotFound), version("", nil), StateAbsent, "", "", 0},
		{"relative PATH", look("bin/gitleaks", exec.ErrDot), version("8.30.1", nil), StateRefused, ReasonRelativePath, "", 0},
		{"under root", look(inBin, nil), version("8.30.1", nil), StateRefused, ReasonUnderScanRoot, "", 0},
		{"ok", look(outBin, nil), version("v8.30.1\n", nil), StateAvailable, "", "8.30.1", 1},
		{"too old", look(outBin, nil), version("8.18.4", nil), StateTooOld, "", "8.18.4", 1},
		{"unstamped", look(outBin, nil), version("version is set by build process", nil), StateTooOld, "", "", 1},
		{"version fails", look(outBin, nil), version("", errors.New("boom")), StateVersionFailed, "", "", 1},
	}
	if symlinks {
		cases = append(cases, tc{"linked into root", look(linkBin, nil), version("8.30.1", nil), StateRefused, ReasonUnderScanRoot, "", 0})
	}
	for _, c := range cases {
		b := Resolve(context.Background(), c.look, c.run, root)
		if b.State != c.state || b.Reason != c.reason || b.Version != c.ver || len(c.run.Calls) != c.calls {
			t.Errorf("%s: %+v with %d version calls; want state %s reason %q version %q, %d calls", c.name, b, len(c.run.Calls), c.state, c.reason, c.ver, c.calls)
		}
		for _, call := range c.run.Calls {
			if !slices.Equal(call.Args, []string{"version"}) || !slices.Contains(call.UnsetEnv, "GITLEAKS_CONFIG") || !slices.Contains(call.UnsetEnv, "GITLEAKS_CONFIG_TOML") {
				t.Errorf("%s: version call %+v", c.name, call)
			}
		}
	}
}

// TestScan_LiveGitleaks runs a real gitleaks, when one at MinVersion or later
// is on PATH, against a repo holding a token and a catch-all .gitleaks.toml
// allowlist: forgectl's --config must keep the finding visible.
func TestScan_LiveGitleaks(t *testing.T) {
	b := Resolve(context.Background(), exec.LookPath, fexec.OSRunner{}, "")
	if b.State != StateAvailable {
		t.Skipf("no usable gitleaks on PATH (%s)", b.State)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".gitleaks.toml"), []byte("[allowlist]\npaths = ['''.*''']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "conf.txt"), []byte("token = "+canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Scan(context.Background(), fexec.OSRunner{}, b.Path, []string{repo}, time.Minute)
	if res.Status != StatusRan || len(res.Findings) == 0 {
		t.Fatalf("result = %+v, want the planted token found despite the repo's allowlist", res)
	}
	if strings.Contains(fmt.Sprintf("%#v", res), canary) {
		t.Error("the canary reached a decoded value")
	}
}
