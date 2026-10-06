//go:build unix

package desk

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// An approval names the whole hash. Claim refuses an empty, short, or
// malformed sha before touching the item, which stays pending.
func TestClaimRefusesAnythingButAFullSHA(t *testing.T) {
	d := openDesk(t)
	a := addScript(t, d, "x.sh", "echo hi\n")
	scan(t, d)
	for _, sha := range []string{"", a.SHA256[:12], strings.ToUpper(a.SHA256), a.SHA256 + "0"} {
		if _, err := d.Claim(a.Name, sha); !errors.Is(err, ErrRefused) {
			t.Errorf("Claim(%q) = %v, want ErrRefused", sha, err)
		}
	}
	if s := scan(t, d); len(s.Pending) != 1 || len(s.Running) != 0 {
		t.Fatalf("pending %d, running %d; want the item left pending", len(s.Pending), len(s.Running))
	}
	if _, err := d.Claim(a.Name, a.SHA256); err != nil {
		t.Fatalf("Claim with the full sha: %v", err)
	}
}

// findKind prefers .sh, so a script carrying a reviewed manifest's bytes must
// not be claimed as the item the manifest was queued as.
func TestClaimRefusesAFileOfAnotherKind(t *testing.T) {
	d := openDesk(t)
	body := "# WHAT: x\n# WHY: y\na -- true\n"
	dropPending(t, d, "01-b.manifest", body)
	scan(t, d)
	sha := queuedSHA(t, d, "01-b")
	if err := os.Rename(filepath.Join(d.Path(), DirPending, "01-b.manifest"), filepath.Join(d.Path(), DirPending, "01-b.sh")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Claim("01-b", sha); !errors.Is(err, ErrRefused) {
		t.Fatalf("Claim = %v, want ErrRefused for a kind change", err)
	}
	if _, err := os.Lstat(filepath.Join(d.Path(), DirRunning, "01-b.sh")); err == nil {
		t.Error("the refused file was moved to running/")
	}
}

// A supervisor runs only the claim it was started for. A wrong sha or kind
// is refused in place: the item and its meta stay in running/ untouched.
func TestSuperviseRefusesAnotherApprovalInPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		sha  func(c *Claimed) string
		kind Kind
	}{
		{"other sha", func(*Claimed) string { return anySHA }, KindScript},
		{"short sha", func(c *Claimed) string { return c.SHA256[:12] }, KindScript},
		{"other kind", func(c *Claimed) string { return c.SHA256 }, KindBatch},
		{"no kind", func(c *Claimed) string { return c.SHA256 }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := openDesk(t)
			marker := filepath.Join(t.TempDir(), "ran")
			c := queue(t, d, "x.sh", "touch "+marker+"\n")
			metaPath := filepath.Join(d.Path(), DirRunning, c.Name+".meta.json")
			before := readFile(t, metaPath)
			if rc, err := d.supervise(c.Name, tc.sha(c), tc.kind); rc != 2 || !errors.Is(err, ErrRefused) {
				t.Fatalf("supervise = %d, %v; want 2 and ErrRefused", rc, err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the item ran")
			}
			if s := scan(t, d); len(s.Running) != 1 || len(s.Skipped) != 0 {
				t.Fatalf("running %d, skipped %d; want the claim left in running/", len(s.Running), len(s.Skipped))
			}
			if after := readFile(t, metaPath); after != before {
				t.Fatalf("meta changed: %s -> %s", before, after)
			}
		})
	}
}

// Anything that can write the desk can rewrite the running record and its
// meta's hash together. The supervisor checks the approved hash, not only the
// meta's, so such a rewrite runs nothing.
func TestARewriteOfRecordAndMetaAfterClaimRunsNothing(t *testing.T) {
	d := openDesk(t)
	marker := filepath.Join(t.TempDir(), "ran")
	c := queue(t, d, "x.sh", "echo fine\n")
	evil := "touch " + marker + "\n"
	writeFile(t, c.RecordPath, evil, 0o600)
	m, _, err := d.readMeta(DirRunning, c.Name)
	if err != nil {
		t.Fatal(err)
	}
	m.SHA256 = SHA256Hex([]byte(evil))
	if err := d.writeMeta(DirRunning, c.Name, m); err != nil {
		t.Fatal(err)
	}
	pid, err := d.Launch(c)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for deadline := time.Now().Add(20 * time.Second); processAlive(pid, 0); {
		if time.Now().After(deadline) {
			t.Fatal("the supervisor did not exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the rewritten record ran")
	}
	if _, err := os.Stat(d.LogPath(c.Name)); err == nil {
		t.Error("a run began for the rewritten record")
	}
}

// dirtyEnv sets the bash startup hooks an item must not inherit, and
// returns the home directory it must start in, symlinks resolved.
func dirtyEnv(t *testing.T) string {
	t.Helper()
	hook := filepath.Join(t.TempDir(), "hook.sh")
	writeFile(t, hook, "echo BASH-ENV-RAN\n", 0o600)
	t.Setenv("BASH_ENV", hook)
	t.Setenv("PS4", "$(echo PS4-RAN)")
	t.Setenv("CDPATH", t.TempDir())
	home, err := HomeDir()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if wd, err := os.Getwd(); err != nil || wd == resolved {
		t.Fatalf("cwd %q (%v) must differ from home for this test to mean anything", wd, err)
	}
	return resolved
}

// A script starts in $HOME with bash's startup hooks removed, whatever the
// supervisor's own cwd and environment. supervise runs here, in the test
// process, whose cwd is the package directory and whose env holds the hooks.
func TestAScriptStartsInHomeWithACleanEnvironment(t *testing.T) {
	home := dirtyEnv(t)
	d := openDesk(t)
	c := queue(t, d, "env.sh", strings.Join([]string{
		`echo "cwd=$(pwd -P)"`,
		`echo "bash_env=${BASH_ENV-unset} ps4=${PS4-unset} cdpath=${CDPATH-unset}"`,
		`set -x; true; set +x`,
	}, "\n")+"\n")
	if rc, err := d.supervise(c.Name, c.SHA256, c.Kind); rc != 0 || err != nil {
		t.Fatalf("supervise = %d, %v", rc, err)
	}
	log := readFile(t, d.LogPath(c.Name))
	if !strings.Contains(log, "cwd="+home+"\n") {
		t.Errorf("log %q; want the item to start in %s", log, home)
	}
	if !strings.Contains(log, "bash_env=unset ps4=+  cdpath=unset") {
		t.Errorf("log %q; want BASH_ENV and CDPATH removed and PS4 at bash's own default", log)
	}
	for _, bad := range []string{"BASH-ENV-RAN", "PS4-RAN"} {
		if strings.Contains(log, bad) {
			t.Errorf("log %q holds %s", log, bad)
		}
	}
}

// The detached supervisor itself starts in $HOME with the hooks removed.
func TestTheSupervisorStartsInHomeWithACleanEnvironment(t *testing.T) {
	home := dirtyEnv(t)
	out := t.TempDir()
	saved := supervisorArgv
	t.Cleanup(func() { supervisorArgv = saved })
	supervisorArgv = func(string, string, string, Kind) ([]string, error) {
		return []string{"/bin/sh", "-c", `pwd -P > "$0/cwd.tmp" && env > "$0/env" && mv "$0/cwd.tmp" "$0/cwd"`, out}, nil
	}
	d := openDesk(t)
	c := queue(t, d, "x.sh", "true\n")
	if _, err := d.Launch(c); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	cwdPath := filepath.Join(out, "cwd")
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(cwdPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in supervisor never ran")
		}
	}
	if got := strings.TrimSpace(readFile(t, cwdPath)); got != home {
		t.Errorf("supervisor cwd %q, want %q", got, home)
	}
	for _, line := range strings.Split(readFile(t, filepath.Join(out, "env")), "\n") {
		if strings.HasPrefix(line, "BASH_ENV=") || strings.HasPrefix(line, "CDPATH=") {
			t.Errorf("supervisor env holds %q", line)
		}
	}
}

// A detached item starts with SIGHUP at its default disposition: the
// supervisor catches it rather than ignoring it, since an ignored signal
// stays ignored across exec.
func TestADetachedItemStartsWithSIGHUPAtItsDefault(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "hup.sh", "bash -c 'kill -HUP $$; exit 7' 2>/dev/null; echo \"hup_rc=$?\"\n")
	if _, err := d.Launch(c); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, state := watchUntil(t, d, c.Name, 20*time.Second); state != WatchEnded {
		t.Fatalf("state %s, want ended", state)
	}
	// 129 is death by SIGHUP; 7 means the signal was ignored.
	if log := readFile(t, d.LogPath(c.Name)); !strings.Contains(log, "hup_rc=129\n") {
		t.Errorf("log %q; want SIGHUP at its default disposition (hup_rc=129)", log)
	}
}

func TestScrubEnvDropsBashStartupHooks(t *testing.T) {
	in := []string{
		"PATH=/bin", "HOME=/h", "BASH_ENV=/x", "ENV=/y", "SHELLOPTS=xtrace", "BASHOPTS=extglob",
		"CDPATH=/c", "GLOBIGNORE=*", "PS4=$(id)", "BASH_FUNC_ls%%=() { :; }", "GH_TOKEN=keep", "PS1=keep",
	}
	want := []string{"PATH=/bin", "HOME=/h", "GH_TOKEN=keep", "PS1=keep"}
	if got := scrubEnv(in); !slices.Equal(got, want) {
		t.Fatalf("scrubEnv = %q, want %q", got, want)
	}
}

// A batch's steps start in the home directory too, with the hooks removed.
// supervise runs in the test process, as above.
func TestBatchStepsStartInHome(t *testing.T) {
	home := dirtyEnv(t)
	d := openDesk(t)
	dropPending(t, d, "01-b.manifest", "# WHAT: x\n# WHY: y\na -- echo \"cwd=$(pwd -P) bash_env=${BASH_ENV-unset}\"\n")
	scan(t, d)
	c, err := d.Claim("01-b", queuedSHA(t, d, "01-b"))
	if err != nil {
		t.Fatal(err)
	}
	if rc, err := d.supervise(c.Name, c.SHA256, c.Kind); rc != 0 || err != nil {
		t.Fatalf("supervise = %d, %v", rc, err)
	}
	if log := readFile(t, d.LogPath(c.Name)); !strings.Contains(log, "cwd="+home+" bash_env=unset") || strings.Contains(log, "BASH-ENV-RAN") {
		t.Fatalf("log %q; want the step in %s with BASH_ENV removed", log, home)
	}
}
