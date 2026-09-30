package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeOnUpdateValidate(t *testing.T) {
	cases := map[string]struct {
		toml    string
		wantErr string // "" = valid
	}{
		"restart":       {`[[resume.on_update]]` + "\nharness = \"claude\"\naction = \"restart\"", ""},
		"command":       {`[[resume.on_update]]` + "\nharness = \"claude\"\ncommand = [\"/usr/bin/true\", \"x\"]\ntimeout_seconds = 30", ""},
		"no section":    {``, ""},
		"empty section": {`[resume]`, ""},
		"both":          {`[[resume.on_update]]` + "\nharness = \"claude\"\naction = \"restart\"\ncommand = [\"x\"]", "exactly one of action and command"},
		"neither":       {`[[resume.on_update]]` + "\nharness = \"claude\"", "set exactly one of"},
		"unknown action": {`[[resume.on_update]]` + "\nharness = \"claude\"\naction = \"reboot\"",
			`action "reboot" is unknown`},
		"codex":           {`[[resume.on_update]]` + "\nharness = \"codex\"\naction = \"restart\"", `harness "codex" is not supported yet`},
		"pi":              {`[[resume.on_update]]` + "\nharness = \"pi\"\naction = \"restart\"", `harness "pi" is not supported yet`},
		"unknown harness": {`[[resume.on_update]]` + "\nharness = \"vim\"\naction = \"restart\"", `harness "vim" is unknown`},
		"no harness":      {`[[resume.on_update]]` + "\naction = \"restart\"", "harness is missing"},
		"empty command":   {`[[resume.on_update]]` + "\nharness = \"claude\"\ncommand = []", "command is empty"},
		"blank program":   {`[[resume.on_update]]` + "\nharness = \"claude\"\ncommand = [\" \"]", "command is empty"},
		"control char":    {`[[resume.on_update]]` + "\nharness = \"claude\"\ncommand = [\"a\", \"b\\u001bc\"]", "element 2 holds a control character"},
		"negative timeout": {`[[resume.on_update]]` + "\nharness = \"claude\"\naction = \"restart\"\ntimeout_seconds = -1",
			"timeout_seconds -1 is out of range"},
		"huge timeout": {`[[resume.on_update]]` + "\nharness = \"claude\"\naction = \"restart\"\ntimeout_seconds = 90000",
			"timeout_seconds 90000 is out of range"},
		"misspelled key": {`[[resume.on_update]]` + "\nharness = \"claude\"\naction = \"restart\"\ntimeout = 5",
			`unknown key "resume.on_update.timeout"`},
		"unknown resume key":    {"[resume]\nwatch = true", `unknown key "resume.watch"`},
		"top-level array table": {"[[on_update]]\nharness = \"claude\"\naction = \"restart\"", "update hooks live under [[resume.on_update]]"},
		"top-level table":       {"[on_update]\nharness = \"claude\"", "update hooks live under [[resume.on_update]]"},
		"top-level empty table": {"[on_update]", "update hooks live under [[resume.on_update]]"},
		"second entry bad": {"[[resume.on_update]]\nharness = \"claude\"\naction = \"restart\"\n[[resume.on_update]]\nharness = \"pi\"\naction = \"restart\"",
			"#2: harness \"pi\""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := DecodeStrict([]byte(tc.toml))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			hooks, err := cfg.ResumeHooks()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ResumeHooks() = %v, want valid", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ResumeHooks() error = %v, want it to contain %q", err, tc.wantErr)
			}
			if hooks != nil {
				t.Fatalf("an invalid section returned hooks %v; a caller must never act on a half-valid list", hooks)
			}
		})
	}
}

// TestValidatePathChecksResume pins that `launch doctor`'s config check
// (ValidatePath) reports a bad hook, not only `resume hooks run`.
func TestValidatePathChecksResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[[resume.on_update]]\nharness = \"claude\"\ncomand = [\"x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ValidatePath(path)
	if err == nil || !strings.Contains(err.Error(), "resume.on_update.comand") {
		t.Fatalf("ValidatePath = %v, want the unknown key named", err)
	}
}
