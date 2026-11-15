// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexbin

import (
	"context"
	"errors"
	"hash/fnv"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

const me = 501

type ent struct {
	mode fs.FileMode
	uid  int
}

// fakeFS maps absolute paths to entries; fs.ModeDir in the mode marks a
// directory.
type fakeFS map[string]ent

// Stat resolves every registered symlink in the path (any component), as
// os.Stat does.
func (f fakeFS) Stat(name string) (Info, error) {
	rest := strings.Split(strings.Trim(path.Clean(name), "/"), "/")
	cur := "/"
	for hops := 0; len(rest) > 0; {
		c := rest[0]
		rest = rest[1:]
		if c == "" || c == "." {
			continue
		}
		next := path.Join(cur, c)
		t, ok := fakeLinks[next]
		if !ok {
			cur = next
			continue
		}
		if hops++; hops > 40 {
			return Info{}, &fs.PathError{Op: "stat", Path: name, Err: errors.New("too many links")}
		}
		if path.IsAbs(t) {
			cur = "/"
		}
		rest = append(strings.Split(strings.Trim(path.Clean(t), "/"), "/"), rest...)
	}
	return f.info(cur)
}

// Lstat is Stat plus the symlink bit, which the fake keeps in the mode.
func (f fakeFS) Lstat(name string) (Info, error) {
	if err, ok := fakeLstatErr[path.Clean(name)]; ok {
		return Info{}, err
	}
	return f.info(name)
}

func (f fakeFS) info(name string) (Info, error) {
	name = path.Clean(name) // a real filesystem ignores doubled separators
	e, ok := f[name]
	if !ok {
		return Info{}, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return Info{Mode: e.mode, IsDir: e.mode&fs.ModeDir != 0, UID: e.uid, UIDKnown: !f.unknownUIDs(),
		Dev: 1, Ino: uint64(h.Sum32()) + uint64(f["\x00gen:"+name].uid)}, nil //nolint:gosec // G115: a small non-negative test counter
}

// unknownUIDs reports whether the fake should hide ownership, as a non-unix
// platform does.
func (f fakeFS) unknownUIDs() bool { _, ok := f["\x00unknown-uids"]; return ok }

// bump gives path a new inode, as a swap would.
func (f fakeFS) bump(p string) { e := f["\x00gen:"+p]; e.uid++; f["\x00gen:"+p] = e }

// fakeLinks maps a symlink's path to its text for fakeFS.Readlink and Stat. The
// tests are not parallel; each test registers its links and removes them at
// test end.
var fakeLinks = map[string]string{}

// Readlink returns a registered symlink's text.
func (f fakeFS) Readlink(name string) (string, error) {
	if t, ok := fakeLinks[path.Clean(name)]; ok {
		return t, nil
	}
	return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
}

// fakeLstatErr makes Lstat of a path fail, as a path past the kernel's length
// limit does.
var fakeLstatErr = map[string]error{}

// symlink registers path as a symlink to text (an entry the walk reads with
// Lstat and Readlink); it is removed at test end.
func symlink(t *testing.T, f fakeFS, name, text string) {
	t.Helper()
	f[name] = ent{fs.ModeSymlink | 0o777, me}
	fakeLinks[name] = text
	t.Cleanup(func() { delete(fakeLinks, name) })
}

func (f fakeFS) Glob(pattern string) ([]string, error) {
	var out []string
	// A literal pattern is returned as written when it names something, as
	// filepath.Glob does, even if it is not a clean path.
	if !strings.ContainsAny(pattern, "*?[\\") {
		if _, ok := f[path.Clean(pattern)]; ok {
			return []string{pattern}, nil
		}
		return nil, nil
	}
	for p := range f {
		if ok, err := path.Match(pattern, p); err != nil {
			return nil, err
		} else if ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// tree builds a filesystem where each file has the given mode, owned by uid,
// and every ancestor directory is 0755 owned by uid. "/Applications" itself is
// root:admin 0775, as on macOS; it is above the bundle, so never checked.
func tree(uid int, files map[string]fs.FileMode) fakeFS {
	f := fakeFS{"/": {fs.ModeDir | 0o755, 0}, "/Applications": {fs.ModeDir | 0o775, 0}}
	for p, m := range files {
		f[p] = ent{m, uid}
		for d := path.Dir(p); d != "/" && d != "/Applications" && d != "."; d = path.Dir(d) {
			if _, ok := f[d]; !ok {
				f[d] = ent{fs.ModeDir | 0o755, uid}
			}
		}
	}
	return f
}

func bin(app string) string {
	return "/Applications/" + app + "/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"
}

const (
	pathBin  = "/opt/bin/rex"
	cfgBin   = "/custom/rex"
	goodJSON = `{"rex_version":"0.1.0-preview.19","api_revision":14,"instance_id":"x"}`
	oldJSON  = `{"rex_version":"0.0.9","api_revision":13}`
)

func lookup(found string) func(string) (string, error) {
	return func(string) (string, error) {
		if found == "" {
			return "", errors.New("not found")
		}
		return found, nil
	}
}

// replies answers `<bin> whoami --json` from the map and `codesign` with
// signErr(path). Anything else is an error.
func runner(replies map[string]string, signErr func(string) error) *fexec.FakeRunner {
	return &fexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == codesignPath {
			if signErr == nil {
				return "", nil
			}
			return "", signErr(args[len(args)-1])
		}
		r, ok := replies[name]
		if !ok {
			return "", errors.New("unexpected binary " + name)
		}
		return r, nil
	}}
}

func resolver(fr *fexec.FakeRunner, f fakeFS, pathResult string, opts ...Option) *Resolver {
	base := []Option{WithFS(f), WithLookPath(lookup(pathResult)), WithUID(me), WithGOOS("linux"),
		WithHome("/Users/u", nil), WithBundlePatterns(DefaultBundlePatterns("")...)}
	return New(fr, append(base, opts...)...)
}

func ran(fr *fexec.FakeRunner, name string) bool {
	for _, c := range fr.Calls {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestResolveOrder(t *testing.T) {
	all := tree(me, map[string]fs.FileMode{cfgBin: 0o755, pathBin: 0o755, bin("Rex Beta.app"): 0o755})
	replies := map[string]string{cfgBin: goodJSON, pathBin: goodJSON, bin("Rex Beta.app"): goodJSON}
	tests := []struct {
		name       string
		fs         fakeFS
		config     string
		pathResult string
		want       string
		wantSrc    Source
	}{
		{"config wins over PATH and bundle", all, cfgBin, pathBin, cfgBin, SourceConfig},
		{"PATH wins over bundle", all, "", pathBin, pathBin, SourcePATH},
		{"bundle last", tree(me, map[string]fs.FileMode{bin("Rex Beta.app"): 0o755}), "", "", bin("Rex Beta.app"), SourceBundle},
		{"non-executable PATH hit is skipped", tree(me, map[string]fs.FileMode{pathBin: 0o644, bin("Rex Beta.app"): 0o755}), "", pathBin, bin("Rex Beta.app"), SourceBundle},
		{"directory PATH hit is skipped", tree(me, map[string]fs.FileMode{bin("Rex Beta.app"): 0o755, pathBin + "/x": 0o755}), "", pathBin, bin("Rex Beta.app"), SourceBundle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := runner(replies, nil)
			got, err := resolver(fr, tt.fs, tt.pathResult).Resolve(context.Background(), tt.config)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Path != tt.want || got.Source != tt.wantSrc {
				t.Errorf("got %s via %s, want %s via %s", got.Path, got.Source, tt.want, tt.wantSrc)
			}
			if got.Version != "0.1.0-preview.19" || got.APIRevision != 14 {
				t.Errorf("version/revision = %q/%d", got.Version, got.APIRevision)
			}
			if len(fr.Calls) != 1 || fr.Calls[0].Name != tt.want || strings.Join(fr.Calls[0].Args, " ") != "--autostart=false --color=never whoami --json" {
				t.Errorf("calls = %+v, want one `%s whoami --json`", fr.Calls, tt.want)
			}
		})
	}
}

func TestNoneFoundListsEveryPlaceTried(t *testing.T) {
	r := resolver(runner(nil, nil), tree(me, nil), "", WithBundlePatterns("/Applications/Rex*.app/x", "/home/u/Applications/Rex*.app/x"))
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	for _, want := range []string{"[surface.rex] binary_path", "PATH", "/Applications/Rex*.app/x", "/home/u/Applications/Rex*.app/x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%v", want, err)
		}
	}
}

func TestNonExecutablePATHCandidateIsNamedInRefusal(t *testing.T) {
	r := resolver(runner(nil, nil), tree(me, map[string]fs.FileMode{pathBin: 0o644}), pathBin, WithBundlePatterns())
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnusableConfigKeyIsRefusedNotReplaced(t *testing.T) {
	for name, fsys := range map[string]fakeFS{
		"missing":        tree(me, map[string]fs.FileMode{pathBin: 0o755}),
		"not executable": tree(me, map[string]fs.FileMode{cfgBin: 0o644, pathBin: 0o755}),
	} {
		t.Run(name, func(t *testing.T) {
			fr := runner(map[string]string{pathBin: goodJSON}, nil)
			_, err := resolver(fr, fsys, pathBin).Resolve(context.Background(), cfgBin)
			if err == nil || !strings.Contains(err.Error(), "binary_path") || !strings.Contains(err.Error(), cfgBin) {
				t.Fatalf("err = %v, want a refusal naming the key and path", err)
			}
			if len(fr.Calls) != 0 {
				t.Errorf("ran %+v; a bad config key must not fall through", fr.Calls)
			}
		})
	}
}

func TestConfigPathForms(t *testing.T) {
	home := "/Users/u"
	fsys := tree(me, map[string]fs.FileMode{home + "/bin/rex": 0o755, cfgBin: 0o755, "rel/rex": 0o755, "/Users/u/~/bin/rex": 0o755})
	replies := map[string]string{home + "/bin/rex": goodJSON, cfgBin: goodJSON}
	tests := []struct {
		name, value string
		homeErr     error
		want        string // resolved path; empty means a refusal
		errHas      string
	}{
		{"absolute works", cfgBin, nil, cfgBin, ""},
		{"tilde expands", "~/bin/rex", nil, home + "/bin/rex", ""},
		{"homeless tilde refuses", "~/bin/rex", errors.New("$HOME is not defined"), "", "no usable home directory"},
		{"relative refuses", "bin/rex", nil, "", "must be an absolute path"},
		{"dot-relative refuses", "./rex", nil, "", "must be an absolute path"},
		{"other-user tilde refuses", "~root/rex", nil, "", "must be an absolute path"},
		{"bare tilde is the home directory, not a binary", "~", nil, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := runner(replies, nil)
			r := resolver(fr, fsys, "", WithHome(home, tt.homeErr), WithBundlePatterns())
			got, err := r.Resolve(context.Background(), tt.value)
			if tt.want != "" {
				if err != nil || got.Path != tt.want || got.Source != SourceConfig {
					t.Fatalf("got %+v, %v; want %s", got, err, tt.want)
				}
				return
			}
			if err == nil || (!strings.Contains(err.Error(), "[surface.rex] binary_path") && tt.errHas != "") || !strings.Contains(err.Error(), tt.errHas) {
				t.Fatalf("err = %v, want it to name the key and contain %q", err, tt.errHas)
			}
			if ran(fr, tt.want) || len(fr.Calls) != 0 {
				t.Errorf("ran %+v on a refused value", fr.Calls)
			}
		})
	}
}

func TestRelativeConfigNeverTouchesCwdFile(t *testing.T) {
	// A file at rel/rex exists in the fake cwd-relative namespace; refusal must
	// come before any Stat of it.
	fsys := tree(me, map[string]fs.FileMode{"bin/rex": 0o755})
	fr := runner(map[string]string{"bin/rex": goodJSON}, nil)
	_, err := resolver(fr, fsys, "").Resolve(context.Background(), "bin/rex")
	if err == nil || len(fr.Calls) != 0 {
		t.Fatalf("err = %v, calls = %+v", err, fr.Calls)
	}
}

func TestNonAbsoluteHomeIsRefused(t *testing.T) {
	r := New(runner(nil, nil), WithFS(tree(me, nil)), WithLookPath(lookup("")), WithUID(me), WithHome("rel/home", nil))
	for _, p := range r.patterns {
		if strings.Contains(p, "rel/home") {
			t.Errorf("pattern %q built from a relative home", p)
		}
	}
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not an absolute path") {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.Resolve(context.Background(), "~/rex"); err == nil || !strings.Contains(err.Error(), "no usable home directory") {
		t.Errorf("tilde with relative home: err = %v", err)
	}
}

func TestDefaultBundlePatterns(t *testing.T) {
	rel := "Contents/Helpers/*.rex/Contents/MacOS/rex"
	want := []string{
		"/Applications/Rex.app/" + rel, "/Applications/Rex Beta.app/" + rel, "/Applications/Rex*.app/" + rel,
		"/Users/u/Applications/Rex.app/" + rel, "/Users/u/Applications/Rex Beta.app/" + rel, "/Users/u/Applications/Rex*.app/" + rel,
	}
	if got := DefaultBundlePatterns("/Users/u"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("patterns =\n%s", strings.Join(got, "\n"))
	}
	if n := len(DefaultBundlePatterns("")); n != 3 {
		t.Errorf("no home: %d patterns, want 3", n)
	}
	if n := len(DefaultBundlePatterns("relative")); n != 3 {
		t.Errorf("relative home: %d patterns, want 3", n)
	}
	for _, p := range DefaultBundlePatterns("/Users/a*b[c]") {
		if strings.Contains(p, "/a*b") || strings.Contains(p, "b[c") {
			t.Errorf("unescaped glob metacharacters in %q", p)
		}
	}
}

func TestHomeGlobMetacharactersAreLiteral(t *testing.T) {
	// A home containing * must not match another user's directory.
	other := "/Users/ab/Applications/Rex.app/Contents/Helpers/x.rex/Contents/MacOS/rex"
	fsys := tree(me, map[string]fs.FileMode{other: 0o755})
	fr := runner(map[string]string{other: goodJSON}, nil)
	r := New(fr, WithFS(fsys), WithLookPath(lookup("")), WithUID(me), WithGOOS("linux"), WithHome("/Users/a*", nil))
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || len(fr.Calls) != 0 {
		t.Fatalf("err = %v, calls = %+v; * in $HOME must not widen the search", err, fr.Calls)
	}
}

func TestBundleOrdering(t *testing.T) {
	files := map[string]fs.FileMode{bin("Rex Aaa.app"): 0o755, bin("Rex Beta.app"): 0o755, bin("Rex.app"): 0o755}
	replies := map[string]string{bin("Rex Aaa.app"): goodJSON, bin("Rex Beta.app"): goodJSON, bin("Rex.app"): goodJSON}
	t.Run("exact Rex.app beats Beta and a look-alike sorted first", func(t *testing.T) {
		got, err := resolver(runner(replies, nil), tree(me, files), "").Resolve(context.Background(), "")
		if err != nil || got.Path != bin("Rex.app") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("Beta beats an unnamed look-alike", func(t *testing.T) {
		f := map[string]fs.FileMode{bin("Rex Aaa.app"): 0o755, bin("Rex Beta.app"): 0o755}
		got, err := resolver(runner(replies, nil), tree(me, f), "").Resolve(context.Background(), "")
		if err != nil || got.Path != bin("Rex Beta.app") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("other Rex apps are a sorted fallback", func(t *testing.T) {
		f := map[string]fs.FileMode{bin("Rex Zed.app"): 0o755, bin("Rex Aaa.app"): 0o755}
		r := map[string]string{bin("Rex Zed.app"): goodJSON, bin("Rex Aaa.app"): goodJSON}
		got, err := resolver(runner(r, nil), tree(me, f), "").Resolve(context.Background(), "")
		if err != nil || got.Path != bin("Rex Aaa.app") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func TestFirstAppWrongRevisionSecondCompatible(t *testing.T) {
	f := tree(me, map[string]fs.FileMode{bin("Rex.app"): 0o755, bin("Rex Beta.app"): 0o755})
	fr := runner(map[string]string{bin("Rex.app"): oldJSON, bin("Rex Beta.app"): goodJSON}, nil)
	got, err := resolver(fr, f, "").Resolve(context.Background(), "")
	if err != nil || got.Path != bin("Rex Beta.app") {
		t.Fatalf("got %+v, %v; want the compatible second app", got, err)
	}
}

func TestNoneCompatibleListsEveryReason(t *testing.T) {
	f := tree(me, map[string]fs.FileMode{bin("Rex.app"): 0o755, bin("Rex Beta.app"): 0o755})
	fr := runner(map[string]string{bin("Rex.app"): oldJSON, bin("Rex Beta.app"): `{"rex_version":"0.2.0","api_revision":15}`}, nil)
	_, err := resolver(fr, f, "").Resolve(context.Background(), "")
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v, want ErrIncompatible", err)
	}
	for _, w := range []string{"Rex.app", "api_revision 13", "Rex Beta.app", "api_revision 15"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q:\n%v", w, err)
		}
	}
}

func TestOwnershipGate(t *testing.T) {
	genuine := bin("Rex.app")
	bundle := "/Applications/Rex.app"
	tests := []struct {
		name   string
		mutate func(fakeFS)
		reason string
	}{
		{"file owned by another user", func(f fakeFS) { f[genuine] = ent{0o755, 999} }, "owned by uid 999"},
		{"bundle dir owned by another user", func(f fakeFS) { f[bundle] = ent{fs.ModeDir | 0o755, 999} }, "owned by uid 999"},
		{"group-writable parent", func(f fakeFS) { f[path.Dir(genuine)] = ent{fs.ModeDir | 0o775, me} }, "writable by group or other"},
		{"world-writable bundle dir", func(f fakeFS) { f[bundle] = ent{fs.ModeDir | 0o757, me} }, "writable by group or other"},
		{"group-writable file", func(f fakeFS) { f[genuine] = ent{0o775, me} }, "writable by group or other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tree(me, map[string]fs.FileMode{genuine: 0o755})
			tt.mutate(f)
			fr := runner(map[string]string{genuine: goodJSON}, nil)
			_, err := resolver(fr, f, "").Resolve(context.Background(), "")
			if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("err = %v, want ErrNotFound naming %q", err, tt.reason)
			}
			if ran(fr, genuine) {
				t.Error("an untrusted candidate was executed")
			}
		})
	}
	t.Run("root-owned is accepted", func(t *testing.T) {
		f := tree(0, map[string]fs.FileMode{genuine: 0o755})
		got, err := resolver(runner(map[string]string{genuine: goodJSON}, nil), f, "").Resolve(context.Background(), "")
		if err != nil || got.Path != genuine {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("an unwritable /Applications above the bundle is not judged", func(t *testing.T) {
		f := tree(me, map[string]fs.FileMode{genuine: 0o755})
		f["/Applications"] = ent{fs.ModeDir | 0o777, 999}
		if _, err := resolver(runner(map[string]string{genuine: goodJSON}, nil), f, "").Resolve(context.Background(), ""); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPlantedLookAlikeIsSkippedAndRealAppWins(t *testing.T) {
	planted, genuine := bin("Rex Aaa.app"), bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	for k, v := range tree(999, map[string]fs.FileMode{planted: 0o755}) {
		if k != "/Applications" {
			f[k] = v
		}
	}
	fr := runner(map[string]string{genuine: goodJSON, planted: goodJSON}, nil)
	got, err := resolver(fr, f, "").Resolve(context.Background(), "")
	if err != nil || got.Path != genuine || ran(fr, planted) {
		t.Fatalf("got %+v, %v; planted ran=%v", got, err, ran(fr, planted))
	}
	// With only the planted app present it is skipped and named, not run.
	only := tree(999, map[string]fs.FileMode{planted: 0o755})
	fr = runner(map[string]string{planted: goodJSON}, nil)
	_, err = resolver(fr, only, "").Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "Rex Aaa.app") || ran(fr, planted) {
		t.Fatalf("err = %v, planted ran=%v", err, ran(fr, planted))
	}
}

func TestSignatureGate(t *testing.T) {
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	bad := func(string) error { return errors.New("code failed to satisfy specified code requirement(s)") }

	t.Run("darwin refuses a bad signature without running the binary", func(t *testing.T) {
		fr := runner(map[string]string{genuine: goodJSON}, bad)
		_, err := resolver(fr, f, "", WithGOOS("darwin")).Resolve(context.Background(), "")
		if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "Team ID "+TeamID) || ran(fr, genuine) {
			t.Fatalf("err = %v, ran=%v", err, ran(fr, genuine))
		}
	})
	t.Run("darwin accepts a good signature and passes the requirement", func(t *testing.T) {
		fr := runner(map[string]string{genuine: goodJSON}, nil)
		if _, err := resolver(fr, f, "", WithGOOS("darwin")).Resolve(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		first := fr.Calls[0]
		want := []string{"--verify", "--strict", `-R=anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] /* exists */ and certificate leaf[field.1.2.840.113635.100.6.1.13] /* exists */ and certificate leaf[subject.OU] = "AG92C3PT77"`, genuine}
		if first.Name != "/usr/bin/codesign" || strings.Join(first.Args, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("first call = %+v, want codesign before whoami", first)
		}
		if fr.Calls[len(fr.Calls)-1].Name != genuine {
			t.Errorf("whoami did not run last: %+v", fr.Calls)
		}
	})
	t.Run("other OSes skip the signature check", func(t *testing.T) {
		fr := runner(map[string]string{genuine: goodJSON}, bad)
		if _, err := resolver(fr, f, "", WithGOOS("linux")).Resolve(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		if ran(fr, codesignPath) {
			t.Error("codesign ran off darwin")
		}
	})
	t.Run("a pinned or PATH binary is not signature-checked", func(t *testing.T) {
		g := tree(me, map[string]fs.FileMode{cfgBin: 0o755})
		fr := runner(map[string]string{cfgBin: goodJSON}, bad)
		if _, err := resolver(fr, g, "", WithGOOS("darwin")).Resolve(context.Background(), cfgBin); err != nil || ran(fr, codesignPath) {
			t.Fatalf("err = %v, codesign ran = %v", err, ran(fr, codesignPath))
		}
	})
}

func TestBundleOutsideAppIsRefused(t *testing.T) {
	odd := "/Applications/Rex/Contents/rex"
	f := tree(me, map[string]fs.FileMode{odd: 0o755})
	fr := runner(map[string]string{odd: goodJSON}, nil)
	_, err := resolver(fr, f, "", WithBundlePatterns("/Applications/Rex/Contents/rex")).Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not inside an .app bundle") || ran(fr, odd) {
		t.Fatalf("err = %v", err)
	}
}

func TestVettingRefusals(t *testing.T) {
	tests := []struct {
		name    string
		reply   string
		runErr  error
		wantAll []string
	}{
		{"wrong api_revision names both", `{"rex_version":"0.2.0","api_revision":15}`, nil,
			[]string{"0.2.0", "api_revision 15", "needs api_revision 14"}},
		{"older revision", `{"rex_version":"0.0.9","api_revision":13}`, nil, []string{"api_revision 13"}},
		{"not JSON", `rex 0.1.0`, nil, []string{"did not return JSON"}},
		{"missing rex_version", `{"api_revision":14}`, nil, []string{"rex_version"}},
		{"unparseable rex_version", `{"rex_version":"latest","api_revision":14}`, nil, []string{"rex_version"}},
		{"empty rex_version", `{"rex_version":"","api_revision":14}`, nil, []string{"rex_version"}},
		{"missing api_revision", `{"rex_version":"0.1.0"}`, nil, []string{"no api_revision"}},
		{"string api_revision", `{"rex_version":"0.1.0","api_revision":"14"}`, nil, []string{"did not return JSON"}},
		{"whoami fails", "", errors.New("exit status 1"), []string{"whoami --json failed", "is the Rex app running"}},
		{"whoami times out on its own", "", context.DeadlineExceeded, []string{"whoami --json failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) { return tt.reply, tt.runErr }}
			// A later candidate that would pass must not be reached: a PATH
			// hit is an explicit choice, so a failed check refuses at once.
			f := tree(me, map[string]fs.FileMode{pathBin: 0o755, bin("Rex.app"): 0o755})
			_, err := resolver(fr, f, pathBin).Resolve(context.Background(), "")
			if !errors.Is(err, ErrIncompatible) {
				t.Fatalf("err = %v, want ErrIncompatible", err)
			}
			for _, w := range tt.wantAll {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q: %v", w, err)
				}
			}
			if len(fr.Calls) != 1 {
				t.Errorf("calls = %d, want 1 (refuse, do not fall through)", len(fr.Calls))
			}
		})
	}
}

func TestCallerCancellationIsNotIncompatible(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fr := &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		cancel()
		return "", context.Canceled
	}}
	f := tree(me, map[string]fs.FileMode{pathBin: 0o755, bin("Rex.app"): 0o755})
	for _, cfg := range []string{"", pathBin} {
		_, err := resolver(fr, f, pathBin).Resolve(ctx, cfg)
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIncompatible) {
			t.Fatalf("cfg=%q err = %v, want context.Canceled and not ErrIncompatible", cfg, err)
		}
	}
	// A cancelled call during bundle candidates stops the walk.
	fr2 := &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		cancel()
		return "", context.Canceled
	}}
	f2 := tree(me, map[string]fs.FileMode{bin("Rex.app"): 0o755, bin("Rex Beta.app"): 0o755})
	_, err := resolver(fr2, f2, "").Resolve(ctx, "")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrIncompatible) || len(fr2.Calls) != 1 {
		t.Fatalf("err = %v, calls = %d", err, len(fr2.Calls))
	}
}

func TestWhoamiGetsItsOwnDeadline(t *testing.T) {
	var got bool
	fr := &fexec.FakeRunner{}
	r := resolver(fr, tree(me, map[string]fs.FileMode{pathBin: 0o755}), pathBin)
	r.runner = ctxRunner{FakeRunner: fr, onRun: func(ctx context.Context) { _, got = ctx.Deadline() }}
	_, _ = r.Resolve(context.Background(), "")
	if !got {
		t.Error("whoami ran without a deadline")
	}
}

// ctxRunner exposes the context Run received.
type ctxRunner struct {
	*fexec.FakeRunner
	onRun func(context.Context)
}

func (c ctxRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	c.onRun(ctx)
	return goodJSON, nil
}

func TestVersionAcceptance(t *testing.T) {
	for v, ok := range map[string]bool{
		"0.1.0-preview.19": true, "1.2.3": true, "1.2.3+build.5": true,
		"": false, "1.2": false, "v1.2.3": false, "latest": false, "1.2.3 ": false,
	} {
		if got := versionRe.MatchString(v); got != ok {
			t.Errorf("versionRe(%q) = %v, want %v", v, got, ok)
		}
	}
}

func TestMissingHomeIsNamedInRefusal(t *testing.T) {
	r := resolver(runner(nil, nil), tree(me, nil), "", WithHome("", errors.New("$HOME is not defined")), WithBundlePatterns("/Applications/Rex*.app/x"))
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "no usable home directory: $HOME is not defined") {
		t.Fatalf("err = %v", err)
	}
}

func TestEveryChainDirectoryIsJudged(t *testing.T) {
	genuine := bin("Rex.app")
	dirs := []string{
		"/Applications/Rex.app/Contents",
		"/Applications/Rex.app/Contents/Helpers",
		"/Applications/Rex.app/Contents/Helpers/Rex Server.rex",
		"/Applications/Rex.app/Contents/Helpers/Rex Server.rex/Contents",
		"/Applications/Rex.app/Contents/Helpers/Rex Server.rex/Contents/MacOS",
	}
	for _, d := range dirs {
		for name, e := range map[string]ent{
			"group-writable": {fs.ModeDir | 0o775, me},
			"foreign-owned":  {fs.ModeDir | 0o755, 999},
		} {
			t.Run(name+" "+strings.TrimPrefix(d, "/Applications/Rex.app/"), func(t *testing.T) {
				f := tree(me, map[string]fs.FileMode{genuine: 0o755})
				if _, ok := f[d]; !ok {
					t.Fatalf("%s is not part of the fake tree", d)
				}
				f[d] = e
				fr := runner(map[string]string{genuine: goodJSON}, nil)
				_, err := resolver(fr, f, "").Resolve(context.Background(), "")
				if !errors.Is(err, ErrNotFound) || ran(fr, genuine) {
					t.Fatalf("err = %v, ran = %v; want a refusal without running it", err, ran(fr, genuine))
				}
			})
		}
	}
}

func TestSymlinkAnywhereInTheChainIsRefused(t *testing.T) {
	genuine := bin("Rex.app")
	for _, p := range []string{
		"/Applications/Rex.app",                  // the .app itself
		"/Applications/Rex.app/Contents/Helpers", // a middle directory
		genuine,                                  // the rex file
	} {
		t.Run(strings.TrimPrefix(p, "/Applications/"), func(t *testing.T) {
			f := tree(me, map[string]fs.FileMode{genuine: 0o755})
			e := f[p]
			e.mode |= fs.ModeSymlink
			f[p] = e
			fr := runner(map[string]string{genuine: goodJSON}, nil)
			_, err := resolver(fr, f, "", WithGOOS("darwin")).Resolve(context.Background(), "")
			if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "is a symlink") {
				t.Fatalf("err = %v, want ErrNotFound naming a symlink", err)
			}
			if ran(fr, genuine) || ran(fr, codesignPath) {
				t.Errorf("a symlinked chain reached codesign or the binary: %+v", fr.Calls)
			}
		})
	}
}

func TestBinaryChangedWhileBeingVerifiedIsRefused(t *testing.T) {
	genuine := bin("Rex.app")
	for name, swap := range map[string]func(fakeFS){
		"new inode":     func(f fakeFS) { f.bump(genuine) },
		"now a symlink": func(f fakeFS) { e := f[genuine]; e.mode |= fs.ModeSymlink; f[genuine] = e },
		"removed":       func(f fakeFS) { delete(f, genuine) },
	} {
		t.Run(name, func(t *testing.T) {
			f := tree(me, map[string]fs.FileMode{genuine: 0o755})
			fr := &fexec.FakeRunner{RunFunc: func(n string, _ []string) (string, error) {
				if n == genuine {
					swap(f)
				}
				return goodJSON, nil
			}}
			_, err := resolver(fr, f, "").Resolve(context.Background(), "")
			if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "binary changed while being verified") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestResolvedPathIsTheCleanGatedPath(t *testing.T) {
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	messy := "/Applications//Rex.app/./Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"
	fr := runner(map[string]string{genuine: goodJSON}, nil)
	got, err := resolver(fr, f, "", WithBundlePatterns(messy)).Resolve(context.Background(), "")
	if err != nil || got.Path != genuine {
		t.Fatalf("got %+v, %v; want the clean path %s", got, err, genuine)
	}
}

func TestCancellationDuringSignatureCheckIsNotNotFound(t *testing.T) {
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755, bin("Rex Beta.app"): 0o755})
	ctx, cancel := context.WithCancel(context.Background())
	fr := &fexec.FakeRunner{RunFunc: func(n string, _ []string) (string, error) {
		if n == codesignPath {
			cancel()
			return "", context.Canceled
		}
		return goodJSON, nil
	}}
	_, err := resolver(fr, f, "", WithGOOS("darwin")).Resolve(ctx, "")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrIncompatible) {
		t.Fatalf("err = %v, want context.Canceled only", err)
	}
	if len(fr.Calls) != 1 {
		t.Errorf("calls = %d, want the walk to stop at the first cancelled codesign", len(fr.Calls))
	}
}

func TestSignatureCheckHasItsOwnTimeoutAndSkipsTheCandidate(t *testing.T) {
	genuine, beta := bin("Rex.app"), bin("Rex Beta.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755, beta: 0o755})
	fr := &fexec.FakeRunner{RunFunc: func(n string, args []string) (string, error) {
		return goodJSON, nil
	}}
	r := resolver(fr, f, "", WithGOOS("darwin"))
	r.signTimeout = 20 * time.Millisecond
	r.runner = signRunner{FakeRunner: fr, run: func(ctx context.Context, name, path string) (string, error) {
		if name != codesignPath {
			return goodJSON, nil
		}
		if path == genuine {
			select { // the tool hangs until its own timeout fires
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			return "", errors.New("signal: killed")
		}
		return "", nil
	}}
	got, err := r.Resolve(context.Background(), "")
	if err != nil || got.Path != beta {
		t.Fatalf("got %+v, %v; want the signed second app", got, err)
	}
	// With no second app the timeout is a listed skip, not a caller error.
	delete(f, beta)
	_, err = r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("err = %v", err)
	}
}

// signRunner routes Run through run so a test can see the context.
type signRunner struct {
	*fexec.FakeRunner
	run func(ctx context.Context, name, path string) (string, error)
}

func (s signRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return s.run(ctx, name, args[len(args)-1])
}

func TestTrustedChainTerminatesOnUncleanPath(t *testing.T) {
	f := tree(me, map[string]fs.FileMode{"/x/y.app/z": 0o755})
	r := resolver(runner(nil, nil), f, "")
	done := make(chan error, 1)
	go func() { _, err := r.trustedChain("/x//y.app/z"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("an unclean path was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("trustedChain did not terminate")
	}
	// Through Resolve the path is cleaned first, so it is gated and run.
	fr := runner(map[string]string{"/x/y.app/z": goodJSON}, nil)
	got, err := resolver(fr, f, "", WithBundlePatterns("/x//y.app/z")).Resolve(context.Background(), "")
	if err != nil || got.Path != "/x/y.app/z" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestBundleDirChoice(t *testing.T) {
	for in, want := range map[string]string{
		"/Applications/Rex.app/Contents/rex":                 "/Applications/Rex.app",
		"/Users/u/Tools.app/Applications/Rex.app/Contents/x": "/Users/u/Tools.app",
		"/Applications/Rex.app/Contents/Helpers/X.app/rex":   "/Applications/Rex.app",
		"/Users/u/Tools.app/Applications/Rex Beta.app/a/rex": "/Users/u/Tools.app",
		"/a/Other.app/b/Third.app/rex":                       "/a/Other.app",
		"/Applications/REX BETA.APP/Contents/rex":            "/Applications/REX BETA.APP",
	} {
		if got, ok := bundleDir(in); !ok || got != want {
			t.Errorf("bundleDir(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := bundleDir("/opt/bin/rex"); ok {
		t.Error("a path with no .app has a bundle dir")
	}
}

func TestHomeUnderAnAppDirectoryStillExaminesBelowIt(t *testing.T) {
	home := "/Users/u/Tools.app/h"
	p := home + "/Applications/Rex.app/Contents/Helpers/a.rex/Contents/MacOS/rex"
	f := tree(me, map[string]fs.FileMode{p: 0o755})
	d := home + "/Applications/Rex.app/Contents"
	f[d] = ent{fs.ModeDir | 0o775, me}
	fr := runner(map[string]string{p: goodJSON}, nil)
	_, err := resolver(fr, f, "", WithHome(home, nil)).Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || ran(fr, p) {
		t.Fatalf("err = %v, ran = %v", err, ran(fr, p))
	}
}

func TestUnknownOwnershipRefusesEveryBundle(t *testing.T) {
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	f["\x00unknown-uids"] = ent{}
	fr := runner(map[string]string{genuine: goodJSON}, nil)
	_, err := resolver(fr, f, "").Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "cannot tell who owns") || ran(fr, genuine) {
		t.Fatalf("err = %v", err)
	}
}

func TestNonRegularFilesAreRefused(t *testing.T) {
	genuine := bin("Rex.app")
	for name, mode := range map[string]fs.FileMode{
		"socket": fs.ModeSocket | 0o755, "device": fs.ModeDevice | 0o755, "fifo": fs.ModeNamedPipe | 0o755,
	} {
		t.Run(name+" in a bundle", func(t *testing.T) {
			f := tree(me, map[string]fs.FileMode{genuine: mode})
			fr := runner(map[string]string{genuine: goodJSON}, nil)
			_, err := resolver(fr, f, "").Resolve(context.Background(), "")
			if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not a regular file") || ran(fr, genuine) {
				t.Fatalf("err = %v", err)
			}
		})
		t.Run(name+" on PATH", func(t *testing.T) {
			f := tree(me, map[string]fs.FileMode{pathBin: mode})
			_, err := resolver(runner(nil, nil), f, pathBin).Resolve(context.Background(), "")
			if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestVerifyRegatesABundleBinary(t *testing.T) {
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	fr := runner(map[string]string{genuine: goodJSON}, nil)
	r := resolver(fr, f, "", WithGOOS("darwin"))
	res, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := r.Verify(context.Background(), res); err != nil {
		t.Fatalf("verify on a clean chain: %v", err)
	}
	// The chain is rewritten after Resolve: Verify must refuse the same value.
	f["/Applications/Rex.app/Contents"] = ent{fs.ModeDir | 0o775, me}
	if err := r.Verify(context.Background(), res); err == nil {
		t.Fatal("verify accepted a chain that became group-writable")
	}
}

func TestVerifyRefusesRelativePathAndNonBundleStaysUsable(t *testing.T) {
	f := tree(me, map[string]fs.FileMode{pathBin: 0o755})
	r := resolver(runner(nil, nil), f, "")
	if err := r.Verify(context.Background(), Resolved{Path: "rex", Source: SourcePATH}); err == nil {
		t.Fatal("verify accepted a relative path")
	}
	if err := r.Verify(context.Background(), Resolved{Path: pathBin, Source: SourcePATH}); err != nil {
		t.Fatalf("verify of a usable PATH binary: %v", err)
	}
	f[pathBin] = ent{0o644, me}
	if err := r.Verify(context.Background(), Resolved{Path: pathBin, Source: SourcePATH}); err == nil {
		t.Fatal("verify accepted a PATH binary that lost its exec bit")
	}
}

func TestVerifyGatesByPathNotByLabel(t *testing.T) {
	genuine := bin("Rex.app")
	for _, src := range []Source{SourceBundle, SourceConfig, SourcePATH} {
		t.Run("group-writable chain, source "+string(src), func(t *testing.T) {
			f := tree(me, map[string]fs.FileMode{genuine: 0o755})
			f["/Applications/Rex.app/Contents"] = ent{fs.ModeDir | 0o775, me}
			r := resolver(runner(nil, nil), f, "")
			if err := r.Verify(context.Background(), Resolved{Path: genuine, Source: src}); err == nil {
				t.Fatal("verify accepted a bundle path with an untrusted chain")
			}
		})
	}
}

func TestVerifyRefusesUnknownSourceAndUncleanPath(t *testing.T) {
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	r := resolver(runner(nil, nil), f, "")
	for name, res := range map[string]Resolved{
		"empty source":   {Path: genuine},
		"unknown source": {Path: genuine, Source: "bogus"},
		"dotdot":         {Path: "/Applications/Rex.app/Contents/x/../Helpers/Rex Server.rex/Contents/MacOS/rex", Source: SourceBundle},
		"doubled slash":  {Path: "/Applications//Rex.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex", Source: SourceBundle},
		"relative":       {Path: "Applications/Rex.app/rex", Source: SourceBundle},
	} {
		t.Run(name, func(t *testing.T) {
			if err := r.Verify(context.Background(), res); err == nil {
				t.Fatalf("verify accepted %+v", res)
			}
		})
	}
}

func TestVerifyRechecksSignatureAndHonorsCancellation(t *testing.T) {
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	signErr := error(nil)
	fr := runner(map[string]string{genuine: goodJSON}, func(string) error { return signErr })
	r := resolver(fr, f, "", WithGOOS("darwin"))
	res := Resolved{Path: genuine, Source: SourceBundle}
	if err := r.Verify(context.Background(), res); err != nil {
		t.Fatalf("verify with a good signature: %v", err)
	}
	signErr = errors.New("bad signature")
	if err := r.Verify(context.Background(), res); err == nil {
		t.Fatal("verify accepted a binary whose signature now fails")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Verify(ctx, res); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verify = %v, want context.Canceled", err)
	}
}

// TestCaseVariantAppIsStillGated: APFS is case-insensitive, so a path spelled
// REX BETA.APP names the real bundle and must get the bundle checks, not just
// the executable check.
func TestCaseVariantAppIsStillGated(t *testing.T) {
	p := "/Applications/REX BETA.APP/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"
	f := tree(me, map[string]fs.FileMode{p: 0o755})
	r := resolver(runner(map[string]string{p: goodJSON}, nil), f, "")
	res := Resolved{Path: p, Source: SourceConfig}
	if err := r.Verify(context.Background(), res); err != nil {
		t.Fatalf("clean case-variant bundle refused: %v", err)
	}
	f["/Applications/REX BETA.APP/Contents"] = ent{fs.ModeDir | 0o775, me}
	if err := r.Verify(context.Background(), res); err == nil {
		t.Fatal("a case-variant .APP path with a group-writable directory skipped the bundle checks")
	}
}

// TestSymlinkIntoBundleIsGated: a symlink on PATH or in the config whose
// target is inside a bundle is judged by the target's chain, at resolve time
// and at Verify, and a refused one is never run.
func TestSymlinkIntoBundleIsGated(t *testing.T) {
	const link = "/usr/local/bin/rex"
	genuine := bin("Rex.app")
	build := func(t *testing.T) (fakeFS, *fexec.FakeRunner) {
		f := tree(me, map[string]fs.FileMode{genuine: 0o755})
		for _, d := range []string{"/usr", "/usr/local", "/usr/local/bin"} {
			f[d] = ent{fs.ModeDir | 0o755, 0}
		}
		symlink(t, f, link, genuine)
		return f, runner(map[string]string{link: goodJSON, genuine: goodJSON}, nil)
	}
	t.Run("clean target resolves and verifies", func(t *testing.T) {
		f, fr := build(t)
		for name, cfg := range map[string]string{"config": link, "PATH": ""} {
			res, err := resolver(fr, f, link).Resolve(context.Background(), cfg)
			if err != nil || res.Path != link {
				t.Fatalf("%s: Resolve = %+v, %v", name, res, err)
			}
			if err := resolver(fr, f, link).Verify(context.Background(), res); err != nil {
				t.Fatalf("%s: Verify: %v", name, err)
			}
		}
	})
	t.Run("untrusted target is refused and not run", func(t *testing.T) {
		f, fr := build(t)
		f["/Applications/Rex.app/Contents"] = ent{fs.ModeDir | 0o775, me}
		if _, err := resolver(fr, f, link).Resolve(context.Background(), link); err == nil || !strings.Contains(err.Error(), "app-bundle binary") {
			t.Fatalf("config symlink into a bad bundle: err = %v", err)
		}
		if _, err := resolver(fr, f, link).Resolve(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "PATH hit is refused as an app-bundle binary") {
			t.Fatalf("PATH symlink into a bad bundle: err = %v", err)
		}
		if ran(fr, link) || ran(fr, genuine) {
			t.Error("a refused symlink target was run")
		}
		if err := resolver(fr, f, link).Verify(context.Background(), Resolved{Path: link, Source: SourcePATH}); err == nil {
			t.Error("Verify accepted a symlink into a bundle with an untrusted chain")
		}
	})
}

// TestPinnedBundlePathIsGatedAtResolve: a config pin that names a bundle
// binary gets the bundle checks before it is run, not only at Verify.
func TestPinnedBundlePathIsGatedAtResolve(t *testing.T) {
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	f["/Applications/Rex.app/Contents/Helpers"] = ent{fs.ModeDir | 0o775, me}
	fr := runner(map[string]string{genuine: goodJSON}, nil)
	if _, err := resolver(fr, f, "").Resolve(context.Background(), genuine); err == nil {
		t.Fatal("a pinned bundle path with a group-writable directory was accepted")
	}
	if ran(fr, genuine) {
		t.Error("a pinned bundle path that failed its checks was run")
	}
	f["/Applications/Rex.app/Contents/Helpers"] = ent{fs.ModeDir | 0o755, me}
	if res, err := resolver(fr, f, "").Resolve(context.Background(), genuine); err != nil || res.Source != SourceConfig {
		t.Fatalf("a clean pinned bundle path: %+v, %v", res, err)
	}
}

// TestOuterAppAbovePinnedPathIsJudged: the chain runs up to the outermost
// .app, so an untrusted Other.app above Rex.app is refused.
func TestOuterAppAbovePinnedPathIsJudged(t *testing.T) {
	p := "/Applications/Other.app/Rex.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"
	f := tree(me, map[string]fs.FileMode{p: 0o755})
	r := resolver(runner(map[string]string{p: goodJSON}, nil), f, "")
	res := Resolved{Path: p, Source: SourceConfig}
	if err := r.Verify(context.Background(), res); err != nil {
		t.Fatalf("clean nested bundle refused: %v", err)
	}
	f["/Applications/Other.app"] = ent{fs.ModeDir | 0o775, me}
	if err := r.Verify(context.Background(), res); err == nil {
		t.Fatal("an untrusted outer .app above the Rex bundle was not judged")
	}
}

// TestLinkThatNamesAnAppButResolvesOutIsRefused: the gate level must not come
// from where the link ends up. A link whose text names an .app, with that .app
// component replaced by a symlink to a plain directory, resolves outside any
// .app; it is refused rather than treated as a plain executable.
func TestLinkThatNamesAnAppButResolvesOutIsRefused(t *testing.T) {
	const link = "/usr/local/bin/rex"
	f := tree(me, map[string]fs.FileMode{"/tmp/t/plain/bin/rex": 0o755})
	for _, d := range []string{"/usr", "/usr/local", "/usr/local/bin"} {
		f[d] = ent{fs.ModeDir | 0o755, 0}
	}
	f["/tmp/t/plain"] = ent{fs.ModeDir | 0o777, me}
	symlink(t, f, "/tmp/t/Rex Beta.app", "/tmp/t/plain")
	symlink(t, f, link, "/tmp/t/Rex Beta.app/bin/rex")
	fr := runner(map[string]string{link: goodJSON, "/tmp/t/plain/bin/rex": goodJSON}, nil)
	r := resolver(fr, f, link)
	if _, err := r.Resolve(context.Background(), link); err == nil || !strings.Contains(err.Error(), "outside one") {
		t.Fatalf("config: err = %v, want a refusal naming that it resolves outside an app", err)
	}
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Fatal("PATH: a link naming an app that resolves outside one was accepted")
	}
	if err := r.Verify(context.Background(), Resolved{Path: link, Source: SourcePATH}); err == nil {
		t.Fatal("Verify accepted it")
	}
	if ran(fr, link) || ran(fr, "/tmp/t/plain/bin/rex") {
		t.Error("a refused link was run")
	}
}

// TestUnresolvableLinkThatNamesAnAppIsRefused: a symlink whose text names an
// .app and whose resolution fails (a path past the kernel's length limit) is
// refused, not treated as a plain binary; so is any link whose resolution
// fails.
func TestUnresolvableLinkThatNamesAnAppIsRefused(t *testing.T) {
	const link = "/usr/local/bin/rex"
	genuine := bin("Rex.app")
	f := tree(me, map[string]fs.FileMode{genuine: 0o755})
	for _, d := range []string{"/usr", "/usr/local", "/usr/local/bin"} {
		f[d] = ent{fs.ModeDir | 0o755, 0}
	}
	symlink(t, f, link, genuine)
	fakeLstatErr["/Applications/Rex.app/Contents"] = errors.New("file name too long")
	t.Cleanup(func() { delete(fakeLstatErr, "/Applications/Rex.app/Contents") })
	fr := runner(map[string]string{link: goodJSON}, nil)
	r := resolver(fr, f, link)
	if _, err := r.Resolve(context.Background(), link); err == nil || !strings.Contains(err.Error(), "cannot resolve") {
		t.Fatalf("err = %v, want a refusal that it cannot resolve the symlinks", err)
	}
	if ran(fr, link) {
		t.Error("an unresolvable link was run")
	}
}

// TestPinAndPathFileSwappedDuringWhoamiIsCaught: the identity re-check after
// whoami covers a config pin and a PATH hit inside a bundle too. It swaps the
// gated file, not a link: repointing a symlink between the check and the exec
// is the documented race, which this does not close.
func TestPinAndPathFileSwappedDuringWhoamiIsCaught(t *testing.T) {
	genuine := bin("Rex.app")
	for _, tc := range []struct {
		name, cfg, pathResult string
	}{
		{"config pin", genuine, ""},
		{"PATH hit", "", genuine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tree(me, map[string]fs.FileMode{genuine: 0o755})
			fr := &fexec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				f.bump(genuine) // the file is swapped while whoami runs
				return goodJSON, nil
			}}
			_, err := resolver(fr, f, tc.pathResult).Resolve(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), "binary changed while being verified") {
				t.Fatalf("err = %v, want the swap caught", err)
			}
		})
	}
}

// TestWalkLinks pins walkLinks against the kernel's resolution rules: relative
// and absolute link text, ".." applied to the already-resolved directory, a
// link to a directory followed by more components, link text recorded for
// every hop, and a loop refused by the hop limit.
func TestWalkLinks(t *testing.T) {
	f := tree(me, map[string]fs.FileMode{
		"/w/real/dir/file": 0o755,
		"/w/real/other":    0o644,
	})
	symlink(t, f, "/w/a", "real/dir")           // relative, to a directory
	symlink(t, f, "/w/up", "real/dir/../other") // ".." after a directory
	symlink(t, f, "/w/abs", "/w/real/dir")      // absolute
	symlink(t, f, "/w/via", "a/../other")       // ".." after a link to a directory
	symlink(t, f, "/w/chain1", "chain2")
	symlink(t, f, "/w/chain2", "/w/a")
	symlink(t, f, "/w/loop1", "loop2")
	symlink(t, f, "/w/loop2", "loop1")
	r := resolver(runner(nil, nil), f, "")
	for _, tc := range []struct {
		in, want string
		texts    int
	}{
		{"/w/real/dir/file", "/w/real/dir/file", 0},
		{"/w/a/file", "/w/real/dir/file", 1},
		{"/w/up", "/w/real/other", 1},
		// ".." follows the directory a link resolved to, not the link's own
		// parent: text cleanup would land on /w/other, which does not exist.
		{"/w/a/../other", "/w/real/other", 1},
		{"/w/via", "/w/real/other", 2}, // the link, then the link it passes through
		{"/w/abs/file", "/w/real/dir/file", 1},
		{"/w/chain1/file", "/w/real/dir/file", 3},
	} {
		got, texts, err := r.walkLinks(tc.in)
		if err != nil || got != tc.want || len(texts) != tc.texts {
			t.Errorf("walkLinks(%q) = %q, %d texts, %v; want %q, %d texts", tc.in, got, len(texts), err, tc.want, tc.texts)
		}
	}
	if _, _, err := r.walkLinks("/w/loop1/x"); err == nil {
		t.Error("a symlink loop was resolved")
	}
	if _, _, err := r.walkLinks("/w/missing/x"); err == nil {
		t.Error("a missing component was resolved")
	}
}

// TestPathHitThatFailsItsGateFallsThroughToACleanBundle: a PATH symlink into
// an untrusted bundle is skipped and listed, and a clean installed bundle
// still resolves.
func TestPathHitThatFailsItsGateFallsThroughToACleanBundle(t *testing.T) {
	const link = "/usr/local/bin/rex"
	bad := bin("Rex.app")
	clean := bin("Rex Beta.app")
	f := tree(me, map[string]fs.FileMode{bad: 0o755, clean: 0o755})
	for _, d := range []string{"/usr", "/usr/local", "/usr/local/bin"} {
		f[d] = ent{fs.ModeDir | 0o755, 0}
	}
	f["/Applications/Rex.app/Contents"] = ent{fs.ModeDir | 0o775, me}
	symlink(t, f, link, bad)
	fr := runner(map[string]string{link: goodJSON, bad: goodJSON, clean: goodJSON}, nil)
	res, err := resolver(fr, f, link).Resolve(context.Background(), "")
	if err != nil || res.Path != clean || res.Source != SourceBundle {
		t.Fatalf("Resolve = %+v, %v; want the clean bundle %s", res, err, clean)
	}
	if ran(fr, link) || ran(fr, bad) {
		t.Error("the untrusted PATH link was run")
	}
}
