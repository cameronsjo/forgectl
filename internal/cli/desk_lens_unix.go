// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/runview"
)

// lensUnmatchedTop is how many of the most common unmatched events check
// lists.
const lensUnmatchedTop = 8

type lensListJSON struct {
	Dir    string         `json:"dir"`
	Lenses []lensListItem `json:"lenses"`
}

type lensListItem struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Format string `json:"format,omitempty"`
	Rules  int    `json:"rules"`
	About  string `json:"about,omitempty"`
	Error  string `json:"error,omitempty"`
}

func runDeskLensList(cmd *cobra.Command, asJSON bool) error {
	dir, err := config.LensesDir()
	if err != nil {
		return fmt.Errorf("desk lens list: lenses directory: %w", err)
	}
	out := lensListJSON{Dir: safeText(dir), Lenses: []lensListItem{}}
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("desk lens list: %w", err)
	}
	bad := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		it := lensListItem{Name: safeLabel(strings.TrimSuffix(e.Name(), ".toml")), Path: safeText(p)}
		if l, err := runview.LoadLens(p); err != nil {
			it.Error = err.Error()
			bad++
		} else {
			it.Format, it.Rules, it.About = l.Format(), l.Rules(), l.About
		}
		out.Lenses = append(out.Lenses, it)
	}
	if !slices.ContainsFunc(out.Lenses, func(it lensListItem) bool { return it.Name == runview.EventsLensName }) {
		ev := runview.EventsLens()
		out.Lenses = append(out.Lenses, lensListItem{Name: ev.Name, Path: "(built in)", Format: ev.Format(), About: ev.About})
	}
	var verdict error
	if bad > 0 {
		verdict = WithExitCode(fmt.Errorf("desk lens list: %s did not parse", plural(bad, "lens", "lenses")), exitFailed)
	}
	if asJSON {
		if err := writeJSON(cmd.OutOrStdout(), out); err != nil {
			return err
		}
		return jsonVerdict(verdict, true)
	}
	w := &stickyWriter{w: cmd.OutOrStdout()}
	w.printf("lenses: %s\n", safeText(dir))
	if len(out.Lenses) == 0 {
		w.printf("  none yet: write NAME.toml there (forgectl desk lens --help shows one)\n")
	}
	width := 0
	for _, it := range out.Lenses {
		width = max(width, len([]rune(safeLabel(it.Name))))
	}
	for _, it := range out.Lenses {
		if it.Error != "" {
			w.printf("  ✗ %-*s  %s\n", width, safeLabel(it.Name), safeText(it.Error))
			continue
		}
		line := fmt.Sprintf("  %-*s  %-4s  %s", width, safeLabel(it.Name), it.Format, plural(it.Rules, "rule", "rules"))
		if it.About != "" {
			line += "  " + safeText(it.About)
		}
		w.printf("%s\n", line)
	}
	if w.err != nil {
		return w.err
	}
	return verdict
}

type lensCheckJSON struct {
	Lens          string          `json:"lens"`
	Format        string          `json:"format"`
	Lines         int             `json:"lines"`
	Split         int             `json:"split"`
	Events        int             `json:"events"`
	Ignored       int             `json:"ignored"`
	Dropped       int             `json:"dropped"`
	DroppedFields int             `json:"dropped_fields"`
	Rules         []lensRuleJSON  `json:"rules"`
	Steps         []string        `json:"steps"`
	Unmatched     int             `json:"unmatched"`
	UnmatchedTop  []lensCountJSON `json:"unmatched_top"`
	Partial       bool            `json:"partial"`
	Note          string          `json:"note,omitempty"`
}

type lensRuleJSON struct {
	N    int    `json:"n"`
	Rule string `json:"rule"`
	Hits int    `json:"hits"`
}

type lensCountJSON struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func runDeskLensCheck(cmd *cobra.Command, lensName, logPath string, asJSON bool) error {
	lens, err := loadLens(lensName)
	if err != nil {
		return jsonVerdict(err, asJSON)
	}
	abs, err := filepath.Abs(logPath)
	if err != nil {
		return fmt.Errorf("desk lens check: --log: %w", err)
	}
	src, err := runview.NewLensSource(abs, lens)
	if err != nil {
		return jsonVerdict(err, asJSON)
	}
	refs, err := src.List()
	if err != nil {
		return jsonVerdict(fmt.Errorf("desk lens check: %w", err), asJSON)
	}
	r, err := loadRun(src, refs[0])
	if err != nil {
		return jsonVerdict(fmt.Errorf("desk lens check: %w", err), asJSON)
	}
	out := lensCheck(lens, r)
	if asJSON {
		if err := writeJSON(cmd.OutOrStdout(), out); err != nil {
			return err
		}
	} else if err := writeLensCheck(cmd, out, lens.Splits()); err != nil {
		return err
	}
	if err := r.readInPart(); err != nil {
		return jsonVerdict(WithExitCode(fmt.Errorf("desk lens check: %s was read in part: %w", safeLabel(r.ref.Name), err), 1), asJSON)
	}
	return nil
}

// lensCheck tallies what the lens made of the run's lines.
func lensCheck(lens *runview.Lens, r *loadedRun) lensCheckJSON {
	d := r.delta
	out := lensCheckJSON{
		Lens: lens.Name, Format: lens.Format(), Lines: d.Lines, Split: d.Split, Events: r.folder.Len(),
		Ignored: d.Ignored, Dropped: d.Dropped, DroppedFields: d.DroppedFields,
		Rules: make([]lensRuleJSON, 0, lens.Rules()), Steps: []string{}, UnmatchedTop: []lensCountJSON{},
		Partial: d.Partial,
	}
	if d.Err != nil {
		out.Note = d.Err.Error()
	}
	for i := 1; i <= lens.Rules(); i++ {
		hits := 0
		if i <= len(d.RuleHits) {
			hits = d.RuleHits[i-1]
		}
		out.Rules = append(out.Rules, lensRuleJSON{N: i, Rule: lens.RuleText(i), Hits: hits})
	}
	for _, st := range r.folder.At(r.folder.Len()).Steps {
		if st.Status != runview.StepPending { // a declared step the log never reached is not found
			out.Steps = append(out.Steps, st.ID)
		}
	}
	// Unmatched lines are counted by shape, their digits folded to #, so
	// lines that differ only in an id or a time count as one kind of line.
	counts := map[string]int{}
	for _, e := range r.folder.Events() {
		if !slices.ContainsFunc(e.Fields, func(f runview.Field) bool { return f.Key == runview.LensActionField }) {
			out.Unmatched++
			counts[lineShape(e.Name)]++
		}
	}
	for name, n := range counts {
		out.UnmatchedTop = append(out.UnmatchedTop, lensCountJSON{Name: name, Count: n})
	}
	slices.SortFunc(out.UnmatchedTop, func(a, b lensCountJSON) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Name, b.Name))
	})
	out.UnmatchedTop = out.UnmatchedTop[:min(len(out.UnmatchedTop), lensUnmatchedTop)]
	return out
}

// digitRun is a run of digits, which lineShape folds.
var digitRun = regexp.MustCompile(`[0-9]+`)

// lineShape is a line with each run of digits replaced by #.
func lineShape(s string) string { return digitRun.ReplaceAllString(s, "#") }

func writeLensCheck(cmd *cobra.Command, c lensCheckJSON, splits bool) error {
	w := &stickyWriter{w: cmd.OutOrStdout()}
	w.printf("lens %s · %s · %s\n", safeLabel(c.Lens), c.Format, plural(len(c.Rules), "rule", "rules"))
	parts := []string{plural(c.Events, "event", "events")}
	if c.Ignored > 0 {
		parts = append(parts, fmt.Sprintf("%d ignored", c.Ignored))
	}
	if c.Dropped > 0 {
		parts = append(parts, plural(c.Dropped, "line dropped", "lines dropped"))
	}
	if c.DroppedFields > 0 {
		parts = append(parts, plural(c.DroppedFields, "field dropped", "fields dropped"))
	}
	w.printf("read: %s\n", strings.Join(parts, " · "))
	if splits {
		what := "the pattern split"
		if c.Format == runview.LensJSON {
			what = "parsed as JSON"
		}
		w.printf("format: %d of %s %s\n", c.Split, plural(c.Lines, "line", "lines"), what)
		if c.Lines > 0 && c.Split == 0 {
			w.printf("warning: no line fit the format, so nothing has a time, step or fields; fix the format before the rules\n")
		}
	}
	w.printf("rules (first match wins):\n")
	unused := 0
	for _, r := range c.Rules {
		mark := ""
		if r.Hits == 0 {
			mark = "  · never matched"
			unused++
		}
		w.printf("  %2d  %-6d %s%s\n", r.N, r.Hits, safeText(r.Rule), mark)
	}
	if len(c.Steps) == 0 {
		w.printf("steps: none found\n")
	} else {
		safe := make([]string, len(c.Steps))
		for i, s := range c.Steps {
			safe[i] = safeLabel(s)
		}
		w.printf("steps: %s\n", strings.Join(safe, ", "))
	}
	if c.Unmatched == 0 {
		w.printf("unmatched: none: every event matched a rule\n")
	} else {
		w.printf("unmatched: %s no rule matched; the most common shapes (digits as #):\n", plural(c.Unmatched, "event", "events"))
		for _, u := range c.UnmatchedTop {
			w.printf("  %6d×  %s\n", u.Count, safeText(u.Name))
		}
	}
	if c.Partial {
		w.printf("partial: past the 32 MiB cap\n")
	}
	if c.Note != "" {
		w.printf("note: %s\n", safeText(c.Note))
	}
	if unused > 0 {
		w.printf("next: a rule that never matched is wrong for this log, or its line never came; desk show --events lists the lines as read\n")
	}
	return w.err
}
