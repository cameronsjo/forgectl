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
