package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestDecodeLegacyLaunch covers the shared decode directly, with no build tag.
// Both loadReadOnly implementations and the unix Capture delegate their whole
// decode to this function, so this is the one place the non-unix path's decode
// behavior can be asserted from a machine that is not that platform — the
// gap #417's review surfaced.
func TestDecodeLegacyLaunch(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantModel     string
		wantProjects  int
		wantUndecoded []string
		wantErr       error
	}{
		{
			name:      "fully modelled file reports no undecoded keys",
			body:      "[defaults]\nmodel = \"sonnet\"\n\n[[project]]\nmatch = \"/tmp\"\n",
			wantModel: "sonnet", wantProjects: 1,
		},
		{
			name:          "unknown nested and top-level keys are reported, sorted",
			body:          "[gateway]\ntoken = \"y\"\n\n[defaults]\nmodel = \"sonnet\"\nunknown_field = \"x\"\n",
			wantModel:     "sonnet",
			wantUndecoded: []string{"defaults.unknown_field", "gateway", "gateway.token"},
		},
		{
			name:      "an empty file is a clean decode, not an error",
			body:      "",
			wantModel: "",
		},
		{
			name:    "malformed TOML wraps ErrLegacyMalformed",
			body:    "this is not [valid toml\n= = =\n",
			wantErr: ErrLegacyMalformed,
		},
		{
			// A TOML escape sequence in a quoted key comes back as its literal
			// source text, because Key.String() re-quotes any key that needed
			// quoting. So an escape-encoded control character never reaches a
			// render site as a raw byte at all.
			name:          "an escape-encoded control character comes back re-quoted",
			body:          "\"\\u001b[2J\" = 1\n",
			wantUndecoded: []string{`"\u001b[2J"`},
		},
		{
			// The parser itself refuses a literal control byte anywhere in the
			// file, so that route cannot deliver a raw ESC to a render site at
			// all — it fails closed as malformed, one layer before termsafe.
			name:    "a literal control byte in a key is refused by the parser",
			body:    "\"\x1b[2Jowned\" = 1\n",
			wantErr: ErrLegacyMalformed,
		},
		{
			// A bidi override is not a TOML control character, so it decodes
			// cleanly and does reach a render site. This is the case termsafe
			// exists for; pinned here so the decoder's half of that contract
			// (surface it, do not swallow it) cannot drift.
			name:          "a bidi override in a key decodes and is surfaced",
			body:          "\"own\u202eresrever\" = 1\n",
			wantUndecoded: []string{"\"own\u202eresrever\""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lc, undecoded, err := decodeLegacyLaunch([]byte(tc.body))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want it to wrap %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error = %v", err)
			}
			if lc.Defaults.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", lc.Defaults.Model, tc.wantModel)
			}
			if len(lc.Projects) != tc.wantProjects {
				t.Errorf("projects = %d, want %d", len(lc.Projects), tc.wantProjects)
			}
			if len(undecoded) == 0 && len(tc.wantUndecoded) == 0 {
				return
			}
			if !reflect.DeepEqual(undecoded, tc.wantUndecoded) {
				t.Errorf("undecoded = %q, want %q", undecoded, tc.wantUndecoded)
			}
		})
	}
}

// TestDecodeLegacyLaunch_StripsUsageOptIn pins the privacy control through the
// shared decode. usage_stats is a modelled field, so it never appears in the
// undecoded list — it is decoded and then stripped, and nothing but an
// operator editing config.toml may set it.
func TestDecodeLegacyLaunch_StripsUsageOptIn(t *testing.T) {
	lc, undecoded, err := decodeLegacyLaunch([]byte("usage_stats = true\n\n[defaults]\nmodel = \"sonnet\"\n"))
	if err != nil {
		t.Fatalf("unexpected error = %v", err)
	}
	if lc.UsageStats {
		t.Error("UsageStats survived the legacy decode; a legacy file must never enable it")
	}
	for _, k := range undecoded {
		if k == "usage_stats" {
			t.Error("usage_stats reported as undecoded; it is a modelled field that is decoded then stripped")
		}
	}
}

// TestNativeMigrationFS_LoadReadOnly_IsLenient covers the read-only loader on
// whichever platform the test runs — it is the same exported behavior on unix
// and non-unix, and the contract that matters is the lenient one: a file with
// keys forgectl cannot represent must still yield the fields it can, because
// aborting here would block a launch over a field the launch never needed.
func TestNativeMigrationFS_LoadReadOnly_IsLenient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claunch.conf")
	if err := os.WriteFile(path, []byte("[defaults]\nmodel = \"sonnet\"\nunknown_field = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var fs nativeMigrationFS
	lc, err := fs.loadReadOnly(path)
	if err != nil {
		t.Fatalf("loadReadOnly() error = %v, want nil — the read-only path is lenient by contract", err)
	}
	if lc.Defaults.Model != "sonnet" {
		t.Errorf("model = %q, want sonnet", lc.Defaults.Model)
	}
}

func TestNativeMigrationFS_LoadReadOnly_AbsentIsNotAnError(t *testing.T) {
	var fs nativeMigrationFS
	got, err := fs.loadReadOnly(filepath.Join(t.TempDir(), "claunch.conf"))
	if !errors.Is(err, ErrNoLegacyLaunch) {
		t.Fatalf("error = %v, want it to wrap ErrNoLegacyLaunch", err)
	}
	if !got.IsZero() {
		t.Errorf("config = %+v, want the zero value on an absent file", got)
	}
}

func TestNativeMigrationFS_LoadReadOnly_MalformedIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claunch.conf")
	if err := os.WriteFile(path, []byte("this is not [valid toml\n= = =\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var fs nativeMigrationFS
	_, err := fs.loadReadOnly(path)
	if !errors.Is(err, ErrLegacyMalformed) {
		t.Fatalf("error = %v, want it to wrap ErrLegacyMalformed", err)
	}
}
