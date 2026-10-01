package config

import (
	"os"
	"strings"
	"testing"
)

func TestHerdrOrganize_Decode(t *testing.T) {
	cfg, err := DecodeStrict([]byte(`
[herdr.organize]
default = "misc"
workspace_order = ["forge", "misc"]

[[herdr.organize.rule]]
glob = "*/forge/* :: *"
workspace = "forge"

[[herdr.organize.rule]]
glob = "*/home/* :: *"
workspace = "home"
`))
	if err != nil {
		t.Fatalf("DecodeStrict: %v", err)
	}
	o := cfg.Herdr.Organize
	if o.Default != "misc" || len(o.WorkspaceOrder) != 2 || o.WorkspaceOrder[1] != "misc" {
		t.Errorf("Organize = %+v", o)
	}
	if len(o.Rules) != 2 || o.Rules[1].Glob != "*/home/* :: *" || o.Rules[1].Workspace != "home" {
		t.Errorf("Rules = %+v", o.Rules)
	}
	if o.IsZero() {
		t.Error("IsZero() = true for a populated section")
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestHasHerdrOrganizeSection(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want bool
	}{
		{"absent", "no_icons = false\n", false},
		{"empty table, as init writes it", "[herdr.organize]\n# default = \"x\"\n", true},
		{"populated", "[herdr.organize]\ndefault = \"x\"\n", true},
		{"a sibling herdr table is not it", "[herdr]\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := DecodeStrict([]byte(tt.toml))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.HasHerdrOrganizeSection(); got != tt.want {
				t.Errorf("HasHerdrOrganizeSection() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHerdrOrganize_ZeroIsValid(t *testing.T) {
	var o HerdrOrganizeConfig
	if !o.IsZero() {
		t.Error("IsZero() = false for the zero value")
	}
	if err := o.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil: an absent section is the command's concern, not a config error", err)
	}
}

func TestHerdrOrganize_Validate(t *testing.T) {
	tests := []struct {
		name string
		cfg  HerdrOrganizeConfig
		want string
	}{
		{
			name: "rules without a default",
			cfg:  HerdrOrganizeConfig{Rules: []HerdrOrganizeRule{{Glob: "x", Workspace: "w"}}},
			want: "[herdr.organize] default is empty",
		},
		{
			name: "empty workspace names the rule by position and glob",
			cfg: HerdrOrganizeConfig{Default: "d", Rules: []HerdrOrganizeRule{
				{Glob: "a", Workspace: "w"}, {Glob: "b", Workspace: "w"}, {Glob: "*/c/*", Workspace: ""},
			}},
			want: `[[herdr.organize.rule]] #3 (glob "*/c/*"): workspace is empty`,
		},
		{
			name: "empty glob",
			cfg:  HerdrOrganizeConfig{Default: "d", Rules: []HerdrOrganizeRule{{Glob: "", Workspace: "w"}}},
			want: `[[herdr.organize.rule]] #1 (glob ""): glob is empty`,
		},
		{
			name: "repeated order label",
			cfg:  HerdrOrganizeConfig{WorkspaceOrder: []string{"a", "b", "a"}},
			want: `workspace_order lists "a" more than once`,
		},
		{
			name: "rule workspace that herdr would read as a flag",
			cfg:  HerdrOrganizeConfig{Default: "d", Rules: []HerdrOrganizeRule{{Glob: "a", Workspace: "-scratch"}}},
			want: `#1 (glob "a"): workspace "-scratch": a label must not start with '-'`,
		},
		{
			name: "default with a control character",
			cfg:  HerdrOrganizeConfig{Default: "d\x1b", Rules: []HerdrOrganizeRule{{Glob: "a", Workspace: "w"}}},
			want: "a label must not contain control characters",
		},
		{
			name: "rule workspace with a bidi override",
			cfg:  HerdrOrganizeConfig{Default: "d", Rules: []HerdrOrganizeRule{{Glob: "a", Workspace: "w\u202e"}}},
			want: "a label must not contain control characters",
		},
		{
			name: "default with a zero width space",
			cfg:  HerdrOrganizeConfig{Default: "d\u200b", Rules: []HerdrOrganizeRule{{Glob: "a", Workspace: "w"}}},
			want: "a label must not contain invisible characters or invalid UTF-8",
		},
		{
			name: "order label with a line separator",
			cfg:  HerdrOrganizeConfig{WorkspaceOrder: []string{"ok", "o\u2028"}},
			want: "workspace_order entry #2",
		},
		{
			name: "order label that is not UTF-8",
			cfg:  HerdrOrganizeConfig{WorkspaceOrder: []string{"o\xff"}},
			want: "a label must not contain invisible characters or invalid UTF-8",
		},
		{
			name: "order label starting with a dash",
			cfg:  HerdrOrganizeConfig{WorkspaceOrder: []string{"ok", "-x"}},
			want: `workspace_order entry #2 "-x": a label must not start with '-'`,
		},
		{
			name: "default over the herdr operand limit",
			cfg:  HerdrOrganizeConfig{Default: strings.Repeat("d", 65), Rules: []HerdrOrganizeRule{{Glob: "a", Workspace: "w"}}},
			want: "[herdr.organize] default " + `"` + strings.Repeat("d", 65) + `"` + ": a label is over 64 bytes",
		},
		{
			name: "rule workspace over the herdr operand limit",
			cfg:  HerdrOrganizeConfig{Default: "d", Rules: []HerdrOrganizeRule{{Glob: "a", Workspace: strings.Repeat("w", 65)}}},
			want: `#1 (glob "a"): workspace "` + strings.Repeat("w", 65) + `": a label is over 64 bytes`,
		},
		{
			name: "order label over the herdr operand limit",
			cfg:  HerdrOrganizeConfig{WorkspaceOrder: []string{"ok", strings.Repeat("o", 65)}},
			want: `workspace_order entry #2 "` + strings.Repeat("o", 65) + `": a label is over 64 bytes`,
		},
		{
			name: "empty order label",
			cfg:  HerdrOrganizeConfig{WorkspaceOrder: []string{"a", " "}},
			want: "workspace_order entry #2 is empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// TestHerdrOrganize_LabelLimitMatchesTheClient: a label the herdr client
// would refuse mid-apply (wire.CheckOperand, 64 bytes) is refused by Validate,
// so the dry run cannot promise a move --apply then fails; a label at the
// limit passes on every path that carries one.
//
// Mutation: make checkLabel skip wire.CheckOperand's length rule (or raise
// wire.MaxOperandLen) and the 65-byte rows of TestHerdrOrganize_Validate go
// red; lower it and the 64-byte rows here do.
func TestHerdrOrganize_LabelLimitMatchesTheClient(t *testing.T) {
	at := func(c byte) string { return strings.Repeat(string(c), 64) }
	for name, cfg := range map[string]HerdrOrganizeConfig{
		"default":         {Default: at('d'), Rules: []HerdrOrganizeRule{{Glob: "a", Workspace: "w"}}},
		"rule workspace":  {Default: "d", Rules: []HerdrOrganizeRule{{Glob: "a", Workspace: at('w')}}},
		"workspace_order": {WorkspaceOrder: []string{"ok", at('o')}},
	} {
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: a 64-byte label was refused: %v", name, err)
		}
	}
}

func TestValidatePath_ChecksHerdrOrganize(t *testing.T) {
	path := t.TempDir() + "/config.toml"
	body := "[herdr.organize]\nworkspace_order = [\"a\", \"a\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ValidatePath(path)
	if err == nil || !strings.Contains(err.Error(), "workspace_order") {
		t.Errorf("ValidatePath = %v, want the workspace_order error (doctor must see it)", err)
	}
}

// TestHerdrOrganize_JoinedLabelsPass: labels that real herdr workspaces carry
// (VS16 emoji, a ZWJ emoji sequence, ZWJ inside Sinhala) pass Validate on
// every path, so an existing workspace with one can still be targeted.
//
// Mutation: drop U+200D or U+FE0F from wire.operandJoiners and its rows go red.
func TestHerdrOrganize_JoinedLabelsPass(t *testing.T) {
	for _, label := range []string{"\u2764\ufe0f home", "\U0001F468\u200d\U0001F4BB dev", "\u0dc1\u0dca\u200d\u0dbb\u0dd3"} {
		cfg := HerdrOrganizeConfig{
			Default:        label,
			Rules:          []HerdrOrganizeRule{{Glob: "a", Workspace: label}},
			WorkspaceOrder: []string{label},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q refused: %v", label, err)
		}
	}
}
