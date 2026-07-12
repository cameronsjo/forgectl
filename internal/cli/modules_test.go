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
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
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
