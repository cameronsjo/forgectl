// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexbin

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	fexec "github.com/cameronsjo/forgectl/internal/exec"
)

type fakeInfo struct {
	mode fs.FileMode
	dir  bool
}

func (fakeInfo) Name() string        { return "" }
func (fakeInfo) Size() int64         { return 0 }
func (i fakeInfo) Mode() fs.FileMode { return i.mode }
func (fakeInfo) ModTime() time.Time  { return time.Time{} }
func (i fakeInfo) IsDir() bool       { return i.dir }
func (fakeInfo) Sys() any            { return nil }

// fakeFS maps absolute paths to modes; a mode of fs.ModeDir marks a directory.
type fakeFS map[string]fs.FileMode

func (f fakeFS) Stat(name string) (fs.FileInfo, error) {
	m, ok := f[name]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fakeInfo{mode: m, dir: m&fs.ModeDir != 0}, nil
}

func (f fakeFS) Glob(pattern string) ([]string, error) {
	var out []string
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

const (
	bundleBin = "/Applications/Rex Beta.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"
	pathBin   = "/opt/bin/rex"
	cfgBin    = "/custom/rex"
	goodJSON  = `{"rex_version":"0.1.0-preview.19","api_revision":14,"instance_id":"x"}`
)

func lookup(found string) func(string) (string, error) {
	return func(string) (string, error) {
		if found == "" {
			return "", errors.New("not found")
		}
		return found, nil
	}
}

func runner(replies map[string]string) *fexec.FakeRunner {
	return &fexec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		r, ok := replies[name]
		if !ok {
			return "", errors.New("unexpected binary " + name)
		}
		return r, nil
	}}
}

func TestResolveOrder(t *testing.T) {
	all := fakeFS{cfgBin: 0o755, pathBin: 0o755, bundleBin: 0o755}
	replies := map[string]string{cfgBin: goodJSON, pathBin: goodJSON, bundleBin: goodJSON}
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
		{"bundle last", fakeFS{bundleBin: 0o755}, "", "", bundleBin, SourceBundle},
		{"non-executable PATH hit is skipped", fakeFS{pathBin: 0o644, bundleBin: 0o755}, "", pathBin, bundleBin, SourceBundle},
		{"directory PATH hit is skipped", fakeFS{pathBin: fs.ModeDir | 0o755, bundleBin: 0o755}, "", pathBin, bundleBin, SourceBundle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := runner(replies)
			r := New(fr, WithFS(tt.fs), WithLookPath(lookup(tt.pathResult)), WithBundlePatterns("/Applications/Rex*.app/"+bundleRel))
			got, err := r.Resolve(context.Background(), tt.config)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Path != tt.want || got.Source != tt.wantSrc {
				t.Errorf("got %s via %s, want %s via %s", got.Path, got.Source, tt.want, tt.wantSrc)
			}
			if got.Version != "0.1.0-preview.19" || got.APIRevision != 14 {
				t.Errorf("version/revision = %q/%d", got.Version, got.APIRevision)
			}
			if len(fr.Calls) != 1 || fr.Calls[0].Name != tt.want || strings.Join(fr.Calls[0].Args, " ") != "whoami --json" {
				t.Errorf("calls = %+v, want one `%s whoami --json`", fr.Calls, tt.want)
			}
		})
	}
}

func TestBundleGlobSkipsNonExecutableMatch(t *testing.T) {
	other := "/Applications/Rex.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"
	fsys := fakeFS{other: 0o644, bundleBin: 0o755}
	r := New(runner(map[string]string{bundleBin: goodJSON}), WithFS(fsys), WithLookPath(lookup("")),
		WithBundlePatterns("/Applications/Rex*.app/"+bundleRel))
	got, err := r.Resolve(context.Background(), "")
	if err != nil || got.Path != bundleBin {
		t.Fatalf("got %+v, %v; want %s", got, err, bundleBin)
	}
}

func TestNoneFoundListsEveryPlaceTried(t *testing.T) {
	r := New(runner(nil), WithFS(fakeFS{}), WithLookPath(lookup("")),
		WithBundlePatterns("/Applications/Rex*.app/x", "/home/u/Applications/Rex*.app/x"))
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

func TestNonExecutableCandidateIsNamedInRefusal(t *testing.T) {
	r := New(runner(nil), WithFS(fakeFS{pathBin: 0o644}), WithLookPath(lookup(pathBin)), WithBundlePatterns())
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnusableConfigKeyIsRefusedNotReplaced(t *testing.T) {
	for name, fsys := range map[string]fakeFS{
		"missing":        {pathBin: 0o755},
		"not executable": {cfgBin: 0o644, pathBin: 0o755},
	} {
		t.Run(name, func(t *testing.T) {
			fr := runner(map[string]string{pathBin: goodJSON})
			r := New(fr, WithFS(fsys), WithLookPath(lookup(pathBin)), WithBundlePatterns())
			_, err := r.Resolve(context.Background(), cfgBin)
			if err == nil || !strings.Contains(err.Error(), "binary_path") || !strings.Contains(err.Error(), cfgBin) {
				t.Fatalf("err = %v, want a refusal naming the key and path", err)
			}
			if len(fr.Calls) != 0 {
				t.Errorf("ran %+v; a bad config key must not fall through", fr.Calls)
			}
		})
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
		{"whoami fails", "", errors.New("exit status 1"), []string{"whoami --json failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fexec.FakeRunner{RunFunc: func(string, []string) (string, error) { return tt.reply, tt.runErr }}
			// A later candidate that would pass must not be reached.
			r := New(fr, WithFS(fakeFS{pathBin: 0o755, bundleBin: 0o755}), WithLookPath(lookup(pathBin)),
				WithBundlePatterns("/Applications/Rex*.app/"+bundleRel))
			_, err := r.Resolve(context.Background(), "")
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

func TestDefaultBundlePatterns(t *testing.T) {
	got := DefaultBundlePatterns("/Users/u")
	if len(got) != 2 || got[0] != "/Applications/Rex*.app/Contents/Helpers/*.rex/Contents/MacOS/rex" ||
		got[1] != "/Users/u/Applications/Rex*.app/Contents/Helpers/*.rex/Contents/MacOS/rex" {
		t.Errorf("patterns = %q", got)
	}
	if n := len(DefaultBundlePatterns("")); n != 1 {
		t.Errorf("no home: %d patterns, want 1", n)
	}
}

func TestMissingHomeIsNamedInRefusal(t *testing.T) {
	r := New(runner(nil), WithFS(fakeFS{}), WithLookPath(lookup("")))
	r.homeErr = errors.New("$HOME is not defined")
	r.patterns = []string{"/Applications/Rex*.app/x"}
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "no home directory: $HOME is not defined") {
		t.Fatalf("err = %v", err)
	}
}
