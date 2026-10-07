package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// humanListLimit is the default row cap of a list verb's human table. The
// JSON default stays unbounded: ADR-0008 rule 2 lets JSON shapes change only
// additively, and a default cap would silently drop rows from every script
// that reads the bare array today. A caller opts into a bounded JSON read with
// an explicit --limit.
const humanListLimit = 100

// usageFailure renders a flag-value error (a bad --limit, an unknown --fields
// name) under the --json contract with the same usage_error code an unknown
// flag gets. It changes no exit code: jsonFailure keeps the one err already
// carries (1 for these plain errors).
func usageFailure(cmd *cobra.Command, err error, asJSON bool) error {
	return jsonFailure(cmd, err, asJSON, jsonCodeUsage)
}

// listBound is the --limit/--fields behaviour shared by the list verbs
// (`review`, `projects list`). The zero value is "no flags given".
type listBound struct {
	limit  int
	fields string
	set    bool // --limit was given on the command line
}

// addFlags registers --limit and --fields on cmd. jsonKeys names the fields
// --fields accepts, in wire order.
func (b *listBound) addFlags(cmd *cobra.Command, jsonKeys []string) {
	cmd.Flags().IntVar(&b.limit, "limit", 0, fmt.Sprintf(
		"show at most N rows (0 = all). The table defaults to %d; --json defaults to every row and, once --limit is given, emits {truncated,total,shown,limit,hint,notes,items} instead of a bare array",
		humanListLimit))
	cmd.Flags().StringVar(&b.fields, "fields", "", "with --json: comma-separated row fields to keep ("+strings.Join(jsonKeys, ", ")+")")
}

// resolve reads whether --limit was given and rejects a negative value. Call
// it first in RunE, once flags are parsed.
func (b *listBound) resolve(cmd *cobra.Command) error {
	b.set = cmd.Flags().Changed("limit")
	if b.limit < 0 {
		return fmt.Errorf("invalid --limit %d: use a positive row count, or 0 for every row", b.limit)
	}
	return nil
}

// fieldList parses --fields against the allowed keys. An unknown name is an
// error that lists the valid ones: a typo must not read as an empty column.
func (b *listBound) fieldList(allowed []string) ([]string, error) {
	if b.fields == "" {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Split(b.fields, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		ok := false
		for _, a := range allowed {
			if a == f {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("unknown --fields name %s; valid: %s",
				termsafe.QuoteArgMax(f, termsafe.ArgEchoMaxRunes), strings.Join(allowed, ", "))
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--fields names no field; valid: %s", strings.Join(allowed, ", "))
	}
	return out, nil
}

// tableCap is the row cap for the human table: the explicit --limit, else the
// default. 0 means every row.
func (b *listBound) tableCap() int {
	if b.set {
		return b.limit
	}
	return humanListLimit
}

// listWindow is the slice of a list that a bound keeps.
type listWindow struct {
	Total     int
	Shown     int
	Limit     int
	Truncated bool
}

// window returns how many of total rows survive a cap (0 = no cap).
func window(total, limit int) listWindow {
	w := listWindow{Total: total, Shown: total, Limit: limit}
	if limit > 0 && total > limit {
		w.Shown = limit
		w.Truncated = true
	}
	return w
}

// narrowHint is the one-line hint printed when rows were dropped. narrow names
// the filters this verb has ("--kind, --repo").
func (w listWindow) narrowHint(narrow string) string {
	return fmt.Sprintf("showing %d of %d; narrow with %s, or raise --limit (0 = all)", w.Shown, w.Total, narrow)
}

// boundedJSON is the document `--limit` turns a list verb's bare JSON array
// into. Items is the (possibly field-projected) rows; Truncated is true when
// Shown < Total; Notes carries the verb's stderr degradation notes (including
// an upstream "results may be truncated at N") so a caller reading only stdout
// still sees them.
//
// Field order is the wire order, and items comes last on purpose: a caller that
// reads only the first bytes of a large document (head -c) still sees whether
// it was cut.
type boundedJSON struct {
	Truncated bool     `json:"truncated"`
	Total     int      `json:"total"`
	Shown     int      `json:"shown"`
	Limit     int      `json:"limit"`
	Hint      string   `json:"hint,omitempty"`
	Notes     []string `json:"notes"`
	Items     any      `json:"items"`
}

func newBoundedJSON(items any, w listWindow, hint string, notes []string) boundedJSON {
	b := boundedJSON{Items: items, Total: w.Total, Shown: w.Shown, Limit: w.Limit, Truncated: w.Truncated, Notes: []string{}}
	if w.Truncated {
		b.Hint = hint
	}
	for _, n := range notes {
		b.Notes = append(b.Notes, safeText(n))
	}
	return b
}

// orderedRow is one JSON object whose keys keep the order --fields named.
type orderedRow struct {
	keys []string
	vals map[string]json.RawMessage
}

// MarshalJSON writes the kept keys in order. The keys come from an allowlist,
// so they need no escaping; the values are already-encoded JSON.
func (o orderedRow) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		// termsafe:allow-raw-json an allowlisted key, never external text; the document is written by termsafe.JSONEncoder
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		v, ok := o.vals[k]
		if !ok {
			v = json.RawMessage("null")
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// projectRows keeps only fields of each row, in the order given. Rows are
// round-tripped through their own wire encoding, so a projected row is
// byte-for-byte the value the full row carries for that key. An omitempty key
// absent from a row reads as null.
func projectRows[T any](rows []T, fields []string) ([]orderedRow, error) {
	out := make([]orderedRow, 0, len(rows))
	for _, r := range rows {
		// termsafe:allow-raw-json intermediate form only; the projected rows are written by termsafe.JSONEncoder
		raw, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out = append(out, orderedRow{keys: fields, vals: m})
	}
	return out, nil
}

// emitListJSON writes a list verb's --json document: the bare array of rows
// (unchanged from before --limit existed) unless --limit was given, then the
// boundedJSON document. rows is already capped by the caller.
func emitListJSON(out io.Writer, items any, b *listBound, w listWindow, narrow string, notes []string) error {
	enc := termsafe.JSONEncoder(out)
	enc.SetIndent("", "  ")
	if !b.set {
		return enc.Encode(items)
	}
	return enc.Encode(newBoundedJSON(items, w, w.narrowHint(narrow), notes))
}
