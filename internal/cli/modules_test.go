package cli

// Registry invariants for the module architecture (ADR-0005). Two tiers:
//
//   - Dynamic invariants (this file, effective from the first conversion):
//     hold over whatever allModules() currently contains — namespace
//     uniqueness, config claims a valid subset, command-tree smoke, and the
//     no-duplicate-root-command hybrid guard.
//   - Completeness pins (land in the final conversion commit): total count,
//     exact name set, exact core-tier set, and the full config bijection.
//     Those pins are THE growth gate: a new module edits them in its own diff.

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/workflow"
)

// moduleTokens returns a module's claimed top-level token set — Name ∪
// GroupAliases ∪ ArgvTokens — deduped WITHIN the module first: tmux
// legitimately carries "tm" as both a GroupAlias and an ArgvToken, and only
// cross-module collisions are defects (cobra's findChild resolves
// first-match-wins, silently).
func moduleTokens(m module.Manifest) map[string]bool {
	tokens := map[string]bool{m.Name: true}
	for _, a := range m.GroupAliases {
		tokens[a] = true
	}
	for _, a := range m.ArgvTokens {
		tokens[a] = true
	}
	return tokens
}

// TestModules_NamespaceUniqueness asserts no two modules claim the same
// top-level token (name, group alias, or argv spelling).
func TestModules_NamespaceUniqueness(t *testing.T) {
	owner := map[string]string{}
	for _, m := range allModules() {
		for tok := range moduleTokens(m) {
			if prev, taken := owner[tok]; taken {
				t.Errorf("token %q claimed by both %q and %q", tok, prev, m.Name)
				continue
			}
			owner[tok] = m.Name
		}
	}
}

// configStructSections returns the toml tags of config.Config's struct-kind
// fields — the domain-owned sections, discriminated from host scalars
// (no_icons, log_level, log_file) by reflect.Kind() == Struct.
func configStructSections(t *testing.T) map[string]bool {
	t.Helper()
	sections := map[string]bool{}
	typ := reflect.TypeOf(config.Config{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Struct {
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if tag == "" || tag == "-" {
			t.Fatalf("config.Config field %s: struct section without a usable toml tag", f.Name)
		}
		sections[tag] = true
	}
	return sections
}

// TestModules_ConfigClaimsAreValidSubset asserts every non-empty ConfigKey
// names a real struct-kind section of config.Config and no section is
// claimed twice. (The full bijection — every section claimed — is a
// completeness pin that lands with the final conversion.)
func TestModules_ConfigClaimsAreValidSubset(t *testing.T) {
	sections := configStructSections(t)
	claimed := map[string]string{}
	for _, m := range allModules() {
		if m.ConfigKey == "" {
			continue
		}
		if !sections[m.ConfigKey] {
			t.Errorf("module %q claims config section %q, which is not a struct-kind toml field on config.Config", m.Name, m.ConfigKey)
		}
		if prev, taken := claimed[m.ConfigKey]; taken {
			t.Errorf("config section %q claimed by both %q and %q", m.ConfigKey, prev, m.Name)
			continue
		}
		claimed[m.ConfigKey] = m.Name
	}
}

// TestModules_CommandTreeSmoke builds every module's command over a
// FakeRunner and checks the constructed command matches its manifest.
func TestModules_CommandTreeSmoke(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	for _, m := range allModules() {
		cmd := m.New(deps)
		if cmd == nil {
			t.Errorf("module %q: New returned nil", m.Name)
			continue
		}
		if cmd.Name() != m.Name {
			t.Errorf("module %q: constructed command is named %q", m.Name, cmd.Name())
		}
		if cmd.Short == "" {
			t.Errorf("module %q: command has no Short (the manifest deliberately has no Summary — Short is the one description)", m.Name)
		}
	}
}

// TestModules_CompletenessPins is THE growth gate (ADR-0005 §Tier policy):
// a new module enters as TierExtension and edits these pins in the same
// diff; a tier promotion edits the core set and notes the evidence in the
// ADR. The pin is a deliberate speed bump, not a cap.
func TestModules_CompletenessPins(t *testing.T) {
	mods := allModules()

	const wantCount = 18
	if len(mods) != wantCount {
		t.Errorf("allModules() has %d modules, want %d — adding a module means editing this pin deliberately (ADR-0005)", len(mods), wantCount)
	}

	wantNames := map[string]bool{
		"tmux": true, "projects": true, "config": true, "launch": true,
		"workflow": true, "pr": true, "net": true, "bench": true,
		"quarantine": true, "pip": true, "docker": true, "branch": true,
		"clean": true, "y": true, "sessions": true, "review": true,
		"env": true,
		"docs": true,
	}
	got := map[string]bool{}
	for _, m := range mods {
		got[m.Name] = true
		if !wantNames[m.Name] {
			t.Errorf("unexpected module %q — add it to the name pin deliberately", m.Name)
		}
	}
	for name := range wantNames {
		if !got[name] {
			t.Errorf("module %q missing from allModules()", name)
		}
	}

	wantCore := map[string]bool{
		"tmux": true, "projects": true, "launch": true,
		"workflow": true, "pr": true, "config": true,
	}
	for _, m := range mods {
		if isCore := m.Tier == module.TierCore; isCore != wantCore[m.Name] {
			t.Errorf("module %q tier = %v, want core=%v — promotion/demotion edits this pin plus an ADR note", m.Name, m.Tier, wantCore[m.Name])
		}
	}
}

// TestModules_ConfigClaimsBijection completes the ownership contract at full
// conversion: every struct-kind config section is claimed by exactly one
// module (validity and single-ownership are covered by
// TestModules_ConfigClaimsAreValidSubset; this adds "none unclaimed").
func TestModules_ConfigClaimsBijection(t *testing.T) {
	sections := configStructSections(t)
	claimed := map[string]bool{}
	for _, m := range allModules() {
		if m.ConfigKey != "" {
			claimed[m.ConfigKey] = true
		}
	}
	for s := range sections {
		if !claimed[s] {
			t.Errorf("config section %q has no owning module — assign a ConfigKey", s)
		}
	}
}

// varRefRe matches ${name} references in workflow step fields.
var varRefRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// stepVarRefs collects every ${var} reference across one step's fields.
func stepVarRefs(s workflow.Step) []string {
	fields := []string{s.Repo, s.Ref, s.Skill, s.Posture, s.Mode, s.From, s.To, s.Cmd}
	fields = append(fields, s.Globs...)
	fields = append(fields, s.Args...)
	var out []string
	for _, f := range fields {
		for _, m := range varRefRe.FindAllStringSubmatch(f, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// TestBuiltinWorkflows_VocabularyCovered is the data-plane safety net
// (ADR-0005): every embedded builtin workflow must be fully served by the
// engine builtins ∪ the default modules' step contributions — each `uses`
// verb registered, and each consumed ${export} provided by a param or an
// earlier step's exports. Its failures NAME which builtin a module eviction
// would break.
func TestBuiltinWorkflows_VocabularyCovered(t *testing.T) {
	deps := module.Deps{Runner: &exec.FakeRunner{}}
	reg, err := workflow.NewRegistry(stepContributions(deps)...)
	if err != nil {
		t.Fatalf("NewRegistry over default module contributions: %v", err)
	}

	names, err := workflow.ListBuiltins()
	if err != nil {
		t.Fatalf("ListBuiltins: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded builtins — the coverage net has nothing to hold")
	}

	for _, name := range names {
		wf, err := workflow.ResolveBuiltin(name)
		if err != nil {
			t.Errorf("parse builtin %q: %v", name, err)
			continue
		}
		available := map[string]bool{}
		for p := range wf.Params {
			available[p] = true
		}
		for i, s := range wf.Steps {
			def, ok := reg[s.Uses]
			if !ok {
				t.Errorf("builtin %q step %d: verb %q is not in builtins ∪ default module contributions — evicting its contributor breaks this builtin", name, i, s.Uses)
				continue
			}
			for _, ref := range stepVarRefs(s) {
				if !available[ref] {
					t.Errorf("builtin %q step %d (%s): consumes ${%s}, which no param or earlier step's exports provide", name, i, s.Uses, ref)
				}
			}
			for _, exp := range def.Exports {
				available[exp] = true
			}
		}
	}
}

// TestRoot_NoDuplicateCommands guards the hybrid migration state: a module
// converted into allModules() but not yet removed from newRoot's hand-wired
// block would register twice, and cobra silently keeps both siblings. Every
// top-level token (name or alias) must be unique across root's children.
func TestRoot_NoDuplicateCommands(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	owner := map[string]string{}
	for _, c := range root.Commands() {
		tokens := append([]string{c.Name()}, c.Aliases...)
		for _, tok := range tokens {
			if prev, taken := owner[tok]; taken {
				t.Errorf("root token %q registered by both %q and %q", tok, prev, c.Name())
				continue
			}
			owner[tok] = c.Name()
		}
	}
}
