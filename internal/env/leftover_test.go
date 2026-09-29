package env

// Test plan for leftover.go (cameronsjo/forgectl#520, #513)
//
//   [x] The scope tag is 12 hex digits, stable, case-folded, and distinct for
//       sibling targets
//   [x] No scoped scratch name matches IsEnvFileName, and none overflows
//       NAME_MAX even for a 255-byte base
//   [x] writeAtomic's REAL temp name is the scoped one: a run that dies with
//       it on disk is refused by the next set
//   [x] A planted scoped temp file refuses `set`; the file is untouched and
//       NOT deleted; the message names its path
//   [x] A planted scoped sops work directory refuses, naming the directory
//       and the ciphertext backup inside it
//   [x] A planted scoped sops backup refuses with the unverified-value message
//   [x] A sibling target's scoped leftover is ignored, and left alone
//   [x] A set on .env while a set on .env.local holds its lock, with a live
//       temp file on disk, is unaffected
//   [x] Concurrent sets on two targets in one directory never refuse each
//       other (-race)
//   [x] A legacy unscoped leftover warns on stderr, does not refuse, and is
//       not deleted
//   [x] A case twin sharing the scratch names is named in the refusal, and
//       no twin is claimed when there is none, or when the "twin" is the
//       target itself under another spelling
//   [x] No refusal or warning carries the value

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

const leftoverSecret = "s3ntinel-LEFTOVER-91q"

// captureWarnings points leftoverWarnings at a buffer for the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := leftoverWarnings
	leftoverWarnings = &buf
	t.Cleanup(func() { leftoverWarnings = prev })
	return &buf
}

func plant(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(leftoverSecret), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func assertStillThere(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("the leftover %s was removed or became unreadable: %v", path, err)
	}
	if string(got) != leftoverSecret {
		t.Fatalf("the leftover %s was modified", path)
	}
}

func setOn(t *testing.T, dir, name string) error {
	t.Helper()
	_, err := (&Client{}).SetValue(pinnedTarget(t, dir, name), "K", leftoverSecret)
	return err
}

func TestScopeTag(t *testing.T) {
	tag := scopeTag(".env")
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(tag) {
		t.Fatalf("scopeTag(.env) = %q, want 12 lowercase hex digits", tag)
	}
	if scopeTag(".env") != tag {
		t.Error("scopeTag is not stable")
	}
	if scopeTag(".ENV") != tag {
		t.Error("scopeTag is not case-folded; .ENV and .env share a lock on a case-insensitive volume")
	}
	if scopeTag(".env.local") == tag {
		t.Error("sibling targets share a scope tag")
	}
}

func TestScratchNamesAreNotEnvFilesAndFitNameMax(t *testing.T) {
	long := strings.Repeat("a", 251) + ".env" // 255 bytes, the NAME_MAX of APFS and ext4
	for _, base := range []string{".env", ".env.local", long} {
		tg := Target{base: base, path: "/r/" + base}
		for _, name := range []string{
			tg.envTempPrefix() + strings.Repeat("A", 16) + ".tmp",
			tg.SopsWorkDirPattern() + "4294967295",
			tg.sopsBackupName(),
		} {
			if IsEnvFileName(name) {
				t.Errorf("scratch name %q matches IsEnvFileName", name)
			}
			if len(name) > 255 {
				t.Errorf("scratch name for a %d-byte base is %d bytes, over NAME_MAX", len(base), len(name))
			}
		}
	}
}

// The name writeAtomic really uses, observed at the moment it exists, is one
// the scan attributes to this target. A run killed right there would leave
// exactly this name behind.
func TestWriteAtomicTempNameIsScopedAndRefused(t *testing.T) {
	dir := t.TempDir()
	var seen string
	prev := tempCreated
	tempCreated = func(name string) { seen = name }
	t.Cleanup(func() { tempCreated = prev })

	if err := setOn(t, dir, ".env"); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if seen == "" {
		t.Fatal("writeAtomic never reported its temp name")
	}

	// Leave that exact name behind, as SIGKILL would have.
	plant(t, filepath.Join(dir, seen))
	err := setOn(t, dir, ".env")
	if err == nil {
		t.Fatalf("a set over the leftover %s succeeded", seen)
	}
	if !strings.Contains(err.Error(), seen) {
		t.Errorf("refusal %q does not name the leftover %s", err, seen)
	}
	assertStillThere(t, filepath.Join(dir, seen))
}

func TestScanRefusesScopedLeftovers(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, dir string, tg Target) []string // returns the paths the message must name
		want  string
	}{
		{
			name: "env temp file",
			plant: func(t *testing.T, dir string, tg Target) []string {
				n := tg.envTempPrefix() + "ABCDEFGHIJKLMNOP.tmp"
				plant(t, filepath.Join(dir, n))
				return []string{n}
			},
			want: "may hold the whole new file",
		},
		{
			name: "sops work directory holding a backup",
			plant: func(t *testing.T, dir string, tg Target) []string {
				n := tg.SopsWorkDirPattern() + "123456"
				if err := os.Mkdir(filepath.Join(dir, n), 0o700); err != nil {
					t.Fatal(err)
				}
				plant(t, filepath.Join(dir, n, "value"))
				plant(t, filepath.Join(dir, n, "backup"))
				return []string{n, filepath.Join(n, "backup")}
			},
			want: "a sops process that outlived forgectl",
		},
		{
			name: "sops ciphertext backup",
			plant: func(t *testing.T, dir string, tg Target) []string {
				n := tg.sopsBackupName()
				plant(t, filepath.Join(dir, n))
				return []string{n}
			},
			want: "may now hold an unverified value",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			warn := captureWarnings(t)
			dir := t.TempDir()
			plant(t, filepath.Join(dir, ".env"))
			tg := pinnedTarget(t, dir, ".env")
			named := c.plant(t, dir, tg)

			err := setOn(t, dir, ".env")
			if err == nil {
				t.Fatal("set succeeded over a scoped leftover")
			}
			msg := err.Error()
			if !strings.Contains(msg, c.want) {
				t.Errorf("refusal %q does not say %q", msg, c.want)
			}
			for _, n := range named {
				if !strings.Contains(msg, n) {
					t.Errorf("refusal %q does not name %s", msg, n)
				}
				if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
					t.Errorf("the leftover %s was removed: %v", n, err)
				}
			}
			for _, n := range named {
				if info, err := os.Lstat(filepath.Join(dir, n)); err == nil && info.IsDir() {
					assertStillThere(t, filepath.Join(dir, n, "value"))
				}
			}
			if !strings.Contains(msg, "Nothing was removed") {
				t.Errorf("refusal %q does not say nothing was removed", msg)
			}
			if strings.Contains(msg, "letter case") {
				t.Errorf("refusal %q mentions a case twin that does not exist", msg)
			}
			// The target itself was not written.
			assertStillThere(t, filepath.Join(dir, ".env"))
			assertNoSecretInOutput(t, leftoverSecret, "", msg, warn.String())
		})
	}
}

// The scope tag lowercases the base, so on a case-sensitive volume .ENV and
// .env share scratch names. The refusal says a leftover may be the twin's
// (cameronsjo/forgectl#652).
func TestScanNamesACaseTwin(t *testing.T) {
	dir := t.TempDir()
	plant(t, filepath.Join(dir, ".env"))
	f, err := os.OpenFile(filepath.Join(dir, ".ENV"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: a fixed name under t.TempDir
	if err != nil {
		t.Skipf("this volume folds case, so .ENV and .env are one file: %v", err)
	}
	_ = f.Close()
	tg := pinnedTarget(t, dir, ".env")
	plant(t, filepath.Join(dir, tg.sopsBackupName()))

	err = setOn(t, dir, ".env")
	if err == nil {
		t.Fatal("set succeeded over a scoped leftover")
	}
	for _, want := range []string{".ENV", "differs only in letter case"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

// On a case-insensitive volume the directory listing can spell the target
// itself in another case. A hard link stands in for that here: .ENV and .env
// are two names for one file, so the refusal must not call either a twin.
func TestScanDoesNotNameTheTargetAsItsOwnTwin(t *testing.T) {
	dir := t.TempDir()
	plant(t, filepath.Join(dir, ".env"))
	if err := os.Link(filepath.Join(dir, ".env"), filepath.Join(dir, ".ENV")); err != nil {
		t.Skipf("cannot give .env a second spelling here: %v", err)
	}
	tg := pinnedTarget(t, dir, ".env")
	plant(t, filepath.Join(dir, tg.sopsBackupName()))

	err := setOn(t, dir, ".env")
	if err == nil {
		t.Fatal("set succeeded over a scoped leftover")
	}
	if strings.Contains(err.Error(), "letter case") {
		t.Errorf("refusal %q names the target's own other spelling as a case twin", err)
	}
}

func TestScanIgnoresSiblingTargetsLeftover(t *testing.T) {
	warn := captureWarnings(t)
	dir := t.TempDir()
	sibling := pinnedTarget(t, dir, ".env.local")
	leftovers := []string{
		sibling.envTempPrefix() + "ABCDEFGHIJKLMNOP.tmp",
		sibling.sopsBackupName(),
	}
	for _, n := range leftovers {
		plant(t, filepath.Join(dir, n))
	}
	if err := os.Mkdir(filepath.Join(dir, sibling.SopsWorkDirPattern()+"42"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := setOn(t, dir, ".env"); err != nil {
		t.Fatalf("a sibling target's leftover blocked .env: %v", err)
	}
	for _, n := range leftovers {
		assertStillThere(t, filepath.Join(dir, n))
	}
	if warn.Len() != 0 {
		t.Errorf("a sibling's scoped leftover produced a warning: %q", warn.String())
	}
}

// A set on .env.local is mid-write: it holds its lock and its temp file is on
// disk. A set on .env must neither refuse on that file nor touch it.
func TestScanUnaffectedByConcurrentSiblingWrite(t *testing.T) {
	dir := t.TempDir()
	local := pinnedTarget(t, dir, ".env.local")
	envT := pinnedTarget(t, dir, ".env")

	var live string
	err := withFileLock(local, func() error {
		f, name, err := local.dir.createTemp(local.envTempPrefix())
		if err != nil {
			return err
		}
		_ = f.Close()
		live = name

		done := make(chan error, 1)
		go func() { _, err := (&Client{}).SetValue(envT, "K", "v"); done <- err }()
		return <-done
	})
	if err != nil {
		t.Fatalf("set on .env during a live .env.local write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, live)); err != nil {
		t.Errorf("the live temp file of the concurrent write was disturbed: %v", err)
	}
}

// Many writers on two targets in one directory. Each set's own temp file is
// live only under its own lock, and a sibling's is out of scope, so no set may
// ever be refused.
func TestConcurrentSetsNeverRefuseEachOther(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 8 {
		name := ".env"
		if i%2 == 1 {
			name = ".env.local"
		}
		tg := pinnedTarget(t, dir, name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 25 {
				key := fmt.Sprintf("K%d_%d", i, j)
				if _, err := (&Client{}).SetValue(tg, key, "v"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent set was refused: %v", err)
	}
}

func TestScanWarnsOnLegacyLeftovers(t *testing.T) {
	warn := captureWarnings(t)
	dir := t.TempDir()
	legacyTemp := ".env-ABCDEFGHIJKLMNOP.tmp"
	legacyWork := ".forgectl-sops-1234567890"
	plant(t, filepath.Join(dir, legacyTemp))
	if err := os.Mkdir(filepath.Join(dir, legacyWork), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := setOn(t, dir, ".env"); err != nil {
		t.Fatalf("a legacy leftover refused the set: %v", err)
	}
	out := warn.String()
	for _, n := range []string{legacyTemp, legacyWork} {
		if !strings.Contains(out, n) {
			t.Errorf("warnings %q do not name the legacy leftover %s", out, n)
		}
	}
	assertStillThere(t, filepath.Join(dir, legacyTemp))
	if _, err := os.Stat(filepath.Join(dir, legacyWork)); err != nil {
		t.Errorf("the legacy work directory was removed: %v", err)
	}
	assertNoSecretInOutput(t, leftoverSecret, "", out)
}
