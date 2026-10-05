package desk

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Ported from run-and-watch's tests/test_rw.py (ParseTest, GraphTest,
// LintTest, DigestTest, nearest_failed).

func parse(t *testing.T, text string) *Manifest {
	t.Helper()
	m, err := ParseManifest(text, "m.manifest")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestParseFirstSeparatorSplitsAndRestIsVerbatim(t *testing.T) {
	m := parse(t, "a -- echo x -- y # not a comment\n")
	if got := m.Steps[0].Command; got != "echo x -- y # not a comment" {
		t.Errorf("command = %q", got)
	}
}

func TestParseIgnoresCommentsAndBlankLines(t *testing.T) {
	m := parse(t, "# header\n# WHAT: a batch\n\n   \n  # indented\na -- true\n")
	if len(m.Steps) != 1 || m.Steps[0].ID != "a" || m.Steps[0].Line != 6 {
		t.Errorf("steps = %+v, want one step a on line 6", m.Steps)
	}
}

func TestParseOptions(t *testing.T) {
	m := parse(t, "a -- true\nb after=a timeout=600 private -- true\n")
	b := m.Steps[1]
	if !reflect.DeepEqual(b.After, []string{"a"}) || b.Timeout != 600 || !b.Private {
		t.Errorf("b = %+v", b)
	}
	if got := b.OptionsText(); got != "after=a timeout=600 private" {
		t.Errorf("options text = %q", got)
	}
	if got := m.Steps[0].OptionsText(); got != "-" {
		t.Errorf("empty options text = %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		needles    []string
	}{
		{"unknown option", "a retries=3 -- true\n", []string{`m.manifest:1: step a: unknown option "retries=3"`, "known: after=, timeout=, private"}},
		{"duplicate id", "a -- true\na -- false\n", []string{"m.manifest:2: step a: duplicate step id"}},
		{"unknown dep names known ids", "a -- true\nb -- true\nc after=zz -- true\n", []string{`m.manifest:3: step c: unknown dependency "zz" (known: a, b, c)`}},
		{"missing separator", "a echo hi\n", []string{"m.manifest:1:", "' -- '"}},
		{"empty command", "a -- \n", []string{"m.manifest:1: step a: empty command"}},
		{"tty is rejected with the alternative", "login tty -- az login\n", []string{"m.manifest:1: step login: tty steps are not supported", "TTY: yes"}},
		{"empty manifest", "# nothing\n", []string{"no steps"}},
		{"two-step cycle", "a after=b -- true\nb after=a -- true\n", []string{"cycle: a -> b -> a"}},
		{"self cycle", "a after=a -- true\n", []string{"cycle: a -> a"}},
		{"longer cycle", "a after=c -- true\nb after=a -- true\nc after=b -- true\nd -- true\n", []string{"cycle: a -> c -> b -> a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(tc.text, "m.manifest")
			if !errors.Is(err, ErrManifest) {
				t.Fatalf("err = %v, want ErrManifest", err)
			}
			for _, n := range tc.needles {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("error %q lacks %q", err, n)
				}
			}
		})
	}
}

func TestParseBadStepIDs(t *testing.T) {
	for _, bad := range []string{"A", "1a", "a_b", "a-b", strings.Repeat("x", 33)} {
		_, err := ParseManifest(bad+" -- true\n", "m.manifest")
		if err == nil || !strings.Contains(err.Error(), "bad step id") || !strings.Contains(err.Error(), "m.manifest:1:") {
			t.Errorf("%q: err = %v, want a bad step id error on line 1", bad, err)
		}
	}
}

func TestParseBadTimeouts(t *testing.T) {
	for _, bad := range []string{"0", "-1", "abc", "1.5x", "99999999999999999999"} {
		_, err := ParseManifest("a timeout="+bad+" -- true\n", "m.manifest")
		if err == nil || !strings.Contains(err.Error(), "m.manifest:1: step a: bad timeout") {
			t.Errorf("timeout=%s: err = %v", bad, err)
		}
	}
}

const graphManifest = "build -- true\nlint -- true\npush after=build -- true\ndeploy after=push,lint -- true\nx -- true\n"

func TestWavesKeepManifestOrder(t *testing.T) {
	m := parse(t, graphManifest)
	want := [][]string{{"build", "lint", "x"}, {"push"}, {"deploy"}}
	if got := m.Waves(); !reflect.DeepEqual(got, want) {
		t.Errorf("waves = %v, want %v", got, want)
	}
	if got := OrderLine(m.Waves()); got != "build,lint,x -> push -> deploy" {
		t.Errorf("order line = %q", got)
	}
}

func TestWavesResolveDependenciesDeclaredLater(t *testing.T) {
	m := parse(t, "late after=early -- true\nearly -- true\n")
	if got := OrderLine(m.Waves()); got != "early -> late" {
		t.Errorf("order line = %q", got)
	}
}

func TestTransitiveAncestors(t *testing.T) {
	m := parse(t, graphManifest)
	keys := func(s map[string]bool) []string {
		var out []string
		for k := range s {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	for id, want := range map[string][]string{"deploy": {"build", "lint", "push"}, "push": {"build"}, "x": nil} {
		if got := keys(m.Ancestors[id]); !reflect.DeepEqual(got, want) {
			t.Errorf("ancestors[%s] = %v, want %v", id, got, want)
		}
	}
}

func TestLint(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"reference to a non-ancestor", "build -- true\nlint -- true\ndeploy after=lint -- ./d \"$OUT_build_image\" $OUT_lint_ok\n",
			[]string{"deploy uses OUT_build_image but build is not an ancestor"}},
		{"reference to a missing step", "a -- echo \"$OUT_nope_k\"\n", []string{"a uses OUT_nope_k but no step nope exists"}},
		{"transitive reference is clean", "a -- true\nb after=a -- true\nc after=b -- echo \"${OUT_a_img}\"\n", nil},
		{"a repeated warning is listed once", "a -- echo $OUT_zz_k $OUT_zz_k\n", []string{"a uses OUT_zz_k but no step zz exists"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parse(t, tc.text).Lint(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("lint = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestManifestHashCoversExactBytes(t *testing.T) {
	a, err := LoadManifest([]byte("a -- echo hi\n"), "x.manifest")
	if err != nil {
		t.Fatal(err)
	}
	// `printf 'a -- echo hi\n' | shasum -a 256`
	if want := "df6e2645a639e7096f7038604a49e86723da49356a10090ab040de6f4115f17a"; a.SHA256 != want {
		t.Fatalf("sha256 = %q, want %q", a.SHA256, want)
	}
	b, err := LoadManifest([]byte("a -- echo hi \n"), "x.manifest")
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 == b.SHA256 {
		t.Error("a one-byte change kept the hash")
	}
	if _, err := LoadManifest([]byte("a -- echo \xff\n"), "x.manifest"); !errors.Is(err, ErrManifest) {
		t.Errorf("non-UTF-8 manifest: err = %v", err)
	}
}

func TestNearestFailed(t *testing.T) {
	m := parse(t, "a -- x\nb after=a -- x\nc after=b -- x\nd after=c,a -- x\n")
	if got := m.nearestFailed("d", map[string]bool{"a": true, "c": true}); got != "c" {
		t.Errorf("nearest of {a,c} = %q, want c", got)
	}
	if got := m.nearestFailed("d", map[string]bool{"a": true}); got != "a" {
		t.Errorf("nearest of {a} = %q, want a", got)
	}
}
