// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/skill"
)

func skillTestRoot() *cobra.Command {
	return newRoot(module.Deps{Runner: &exec.FakeRunner{}})
}

func runSkillRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := skillTestRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestRootSkill_PrintsSkillAndExitsZero(t *testing.T) {
	out, err := runSkillRoot(t, "--skill")
	if err != nil {
		t.Fatalf("--skill: %v", err)
	}
	if out != skill.Text() {
		t.Error("--skill output differs from the embedded SKILL.md")
	}
}

func TestRootSkill_InstallWritesTheTree(t *testing.T) {
	dir := t.TempDir()
	out, err := runSkillRoot(t, "--skill", "--install", dir)
	if err != nil {
		t.Fatalf("--skill --install: %v", err)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("output %q should name the directory", out)
	}
	for _, f := range []string{"SKILL.md", filepath.Join("references", "desk.md")} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
}

func TestRootSkill_UsageErrorsExitTwo(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	cases := map[string][]string{
		"relative dir":          {"--skill", "--install", "rel/dir"},
		"missing dir":           {"--skill", "--install", missing},
		"empty dir":             {"--skill", "--install", ""},
		"install without skill": {"--install", t.TempDir()},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runSkillRoot(t, args...)
			if err == nil {
				t.Fatal("want a usage error")
			}
			if got := ExitCode(err); got != 2 {
				t.Errorf("exit code = %d, want 2 (%v)", got, err)
			}
		})
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("a refused install created the directory")
	}
}

func TestRootHelp_DeclaresSkillFlags(t *testing.T) {
	root := skillTestRoot()
	for _, name := range []string{"skill", "install"} {
		if root.Flags().Lookup(name) == nil {
			t.Errorf("root has no --%s flag, so --help cannot list it", name)
		}
	}
}

var skillCodeSpan = regexp.MustCompile("`([^`\n]+)`")

// skillSpans returns every inline code span and every fenced-block line.
func skillSpans(text string) []string {
	var spans []string
	for _, m := range skillCodeSpan.FindAllStringSubmatch(text, -1) {
		spans = append(spans, m[1])
	}
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			spans = append(spans, line)
		}
	}
	return spans
}

func childNamed(cmd *cobra.Command, name string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return c
		}
	}
	return nil
}

// skillExternalSpans are other programs' commands the skill quotes that happen
// to start with a forgectl verb's name (`docker ps` is Docker's, not forgectl docker).
var skillExternalSpans = map[string]bool{"docker ps": true}

var skillWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// checkSkillSpan returns what a span names that the tree lacks. A span counts
// when it starts with `forgectl ` or with a root verb (the skill's tables
// write `surface wait`, `bench open|up`). Alternatives joined by `|` (`\|` in
// a table) are each checked. Below a verb, a word is checked only while the
// verb has subcommands and does not take free-form arguments (launch passes
// harness args through). Flags named in the span must exist on the command it
// resolves to, unless the span names alternatives or an unknown tail.
func checkSkillSpan(root *cobra.Command, span string) []string {
	if skillExternalSpans[span] {
		return nil
	}
	span = strings.ReplaceAll(span, `\|`, "|")
	fields := strings.Fields(span)
	if len(fields) > 0 && fields[0] == "forgectl" {
		fields = fields[1:]
	}
	if len(fields) == 0 || !skillWord.MatchString(fields[0]) || childNamed(root, fields[0]) == nil {
		if len(fields) > 0 && skillWord.MatchString(fields[0]) && strings.HasPrefix(span, "forgectl ") {
			return []string{fields[0]}
		}
		return nil
	}
	cmd := childNamed(root, fields[0])
	path := []string{fields[0]}
	var bad []string
	rest := fields[1:]
	alternatives := false
	for len(rest) > 0 && len(cmd.Commands()) > 0 && !parentTakesArg(cmd) {
		words := strings.Split(rest[0], "|")
		valid := true
		for _, w := range words {
			if !skillWord.MatchString(w) {
				valid = false
			}
		}
		if !valid {
			break
		}
		for _, w := range words {
			if childNamed(cmd, w) == nil {
				bad = append(bad, strings.Join(path, " ")+" "+w)
			}
		}
		if len(words) > 1 || bad != nil {
			alternatives = true
			break
		}
		cmd = childNamed(cmd, words[0])
		path = append(path, words[0])
		rest = rest[1:]
	}
	if alternatives || bad != nil {
		return bad
	}
	for _, f := range rest {
		if f == "--" {
			break // everything after is passed through to another program
		}
		name, ok := strings.CutPrefix(f, "--")
		if !ok || name == "help" {
			continue
		}
		name, _, _ = strings.Cut(name, "=")
		name = strings.TrimRight(name, ",.;:)`")
		if !skillWord.MatchString(name) {
			continue
		}
		if cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil && cmd.PersistentFlags().Lookup(name) == nil {
			bad = append(bad, strings.Join(path, " ")+" --"+name)
		}
	}
	return bad
}

// TestSkill_NamesOnlyCommandsTheTreeHas fails when the embedded skill tells an
// agent to run a verb, subcommand, or flag the command tree no longer has, so a
// rename or removal cannot leave the skill teaching a dead command.
func TestSkill_NamesOnlyCommandsTheTreeHas(t *testing.T) {
	root := skillTestRoot()
	var files []string
	if err := fs.WalkDir(skill.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			files = append(files, p)
		}
		return err
	}); err != nil || len(files) == 0 {
		t.Fatalf("walking the embedded skill: %v (%d files)", err, len(files))
	}

	checked := 0
	for _, f := range files {
		body, err := fs.ReadFile(skill.FS(), f)
		if err != nil {
			t.Fatal(err)
		}
		for _, span := range skillSpans(string(body)) {
			if strings.HasPrefix(strings.TrimSpace(span), "forgectl ") || startsWithRootVerb(root, span) {
				checked++
			}
			for _, bad := range checkSkillSpan(root, span) {
				t.Errorf("%s names `%s` (in %q), which the command tree does not have", f, bad, span)
			}
		}
	}
	if checked < 60 {
		t.Errorf("checked only %d command spans; a scan that matches nothing proves nothing", checked)
	}
}

func startsWithRootVerb(root *cobra.Command, span string) bool {
	fields := strings.Fields(span)
	return len(fields) > 0 && childNamed(root, fields[0]) != nil
}

func TestSkillDrift_CatchesDeadCommandsAndFlags(t *testing.T) {
	root := skillTestRoot()
	dead := []string{
		"forgectl nonesuch", "forgectl env nonesuch", "desk gone", "surface await <name>",
		"bench open|nope", "forgectl tasks done 5 --nope", "surface launch --surface x --nonesuch",
		"forgectl desk lens nonesuch",
	}
	for _, span := range dead {
		if len(checkSkillSpan(root, span)) == 0 {
			t.Errorf("%q should be reported as dead", span)
		}
	}
	live := []string{
		"forgectl env", "forgectl env set KEY", "desk add f --what x --why y", "net positional",
		`quarantine hide\|status\|restore`, "forgectl launch -- --model x", "forgectl tasks done 5 --evidence x --closer y",
		"set KEY", "forgectl desk lens check", "forgectl --skill",
	}
	for _, span := range live {
		if bad := checkSkillSpan(root, span); len(bad) != 0 {
			t.Errorf("%q should resolve, got %v", span, bad)
		}
	}
}
