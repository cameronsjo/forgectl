package gitleaks

import (
	"encoding/json"
	"errors"
	"io"
)

// maxReportBytes caps how much of one report is read.
const maxReportBytes = 32 << 20

// errReportTooLarge is a report past maxReportBytes.
var errReportTooLarge = errors.New("gitleaks report too large")

// errReportMalformed is a report that is not a JSON array of findings.
var errReportMalformed = errors.New("gitleaks report malformed")

// wireFinding is everything forgectl decodes from a gitleaks JSON finding.
// Secret, Match and Line are deliberately absent, so even a report written
// without --redact never puts secret text into a forgectl value: the
// decoder skips a field the struct does not name.
type wireFinding struct {
	RuleID      string
	File        string
	StartLine   int
	Fingerprint string
}

// decodeReport streams a JSON array of findings from r, keeping at most
// budget of them (truncated reports the rest were left unread) and reading
// at most maxReportBytes. A null report is an empty one.
func decodeReport(r io.Reader, budget int) ([]wireFinding, bool, error) {
	lr := &io.LimitedReader{R: r, N: maxReportBytes + 1}
	dec := json.NewDecoder(lr)
	fail := func() ([]wireFinding, bool, error) {
		if lr.N <= 0 {
			return nil, false, errReportTooLarge
		}
		return nil, false, errReportMalformed
	}
	tok, err := dec.Token()
	if err != nil {
		return fail()
	}
	if tok == nil {
		return []wireFinding{}, false, nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fail()
	}
	out := []wireFinding{}
	for dec.More() {
		if len(out) >= budget {
			return out, true, nil
		}
		var w wireFinding
		if err := dec.Decode(&w); err != nil {
			return fail()
		}
		out = append(out, w)
	}
	if _, err := dec.Token(); err != nil {
		return fail()
	}
	if lr.N <= 0 {
		return nil, false, errReportTooLarge
	}
	return out, false, nil
}
