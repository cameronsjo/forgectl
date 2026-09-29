package docs

import (
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Trust signals from the Open Knowledge Format (OKF v0.2, SPEC.md at
// GoogleCloudPlatform/open-knowledge-format@ad30107):
//
//   - §5.4 status: "draft | stable | deprecated", a lowercase string enum.
//     Only an exact "deprecated" is a signal; absent means stable.
//   - §5.5 stale_after: an absolute instant. A doc is stale when
//     now >= stale_after. §5 requires every OKF timestamp to be an ISO 8601
//     datetime with an explicit UTC offset, and §11 has consumers ignore
//     anything else, so a date-only or offset-less value is never stale.
//
// The upstream format changed inside "Version 0.2" without a version bump:
// OKF commit 3dc3029 (2026-08-20) turned stale_after from a date-only
// YYYY-MM-DD ("stale when today >= stale_after") into the datetime rule
// above, and 0b87c52/6a2845d later dropped the explicit "MUST ignore
// date-only" sentence as a duplicate. okf_version "0.2" cannot tell the two
// variants apart, so a bundle written against the older text shows no stale
// signal here. That matches the reference implementation's is_stale, which
// is strict rather than falling back to midnight UTC.
//
// Trust fields are read from the yaml.Node's raw scalar text, never from a
// map[string]any decode the way aliasesFromNode reads aliases: yaml.v3
// decodes both "2026-09-23T00:00:00Z" and the date-only "2026-09-23" into
// time.Time, which erases exactly the distinction the spec draws.

// trustNow is the one clock the trust signals read: the properties block,
// the status bar, and Check all go through it. Tests set it only through
// withTrustNow.
var trustNow = time.Now

// trustState is a doc's evaluated trust signals at one instant.
type trustState struct {
	Deprecated bool
	Stale      bool
	// StaleAfter is the raw authored value, set only when Stale.
	StaleAfter string
}

// trustFields returns the raw text of a frontmatter mapping's status and
// stale_after scalars. A non-scalar value (a list, a mapping) yields "".
func trustFields(mapping *yaml.Node) (status, staleAfter string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return "", ""
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if value.Kind != yaml.ScalarNode {
			continue
		}
		switch key.Value {
		case "status":
			status = value.Value
		case "stale_after":
			staleAfter = value.Value
		}
	}
	return status, staleAfter
}

// orphanOKField reports whether a frontmatter mapping carries the boolean
// scalar orphan_ok: true. Anything else (false, a string such as "yes", a
// list, a mapping; yaml.v3 refuses to decode a string into a bool) is not an opt-out, so a typo leaves the orphan reported.
func orphanOKField(mapping *yaml.Node) bool {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i], mapping.Content[i+1]
		if key.Value != "orphan_ok" || value.Kind != yaml.ScalarNode {
			continue
		}
		var ok bool
		return value.Decode(&ok) == nil && ok
	}
	return false
}

// frontmatterTrust returns a YAML frontmatter block's raw status and
// stale_after values. A TOML (+++) block yields none: OKF frontmatter is
// YAML, the same scoping frontmatterRoot applies.
func frontmatterTrust(fm frontmatterBlock) (status, staleAfter string) {
	return trustFields(frontmatterRoot(fm))
}

// rfc3339DateTime is RFC 3339 §5.6's date-time grammar, with uppercase T and
// Z only, as time.Parse requires (the RFC also permits lowercase).
// time.Parse(time.RFC3339, …) also accepts a comma fraction, a one-digit hour,
// and an offset of +24:00 or with minute 60, none of which the grammar
// allows; this check rejects them before the parse. Year 0000 is valid
// (date-fullyear is 4DIGIT) and is kept.
var rfc3339DateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]([01]\d|2[0-3]):[0-5]\d)$`)

// evalTrust evaluates raw trust fields at now. status matches "deprecated"
// exactly (§5.4 is lowercase). staleAfter must match RFC 3339's date-time
// grammar (so it carries an offset) and parse to a real instant; anything
// else is not a timestamp and is never stale.
func evalTrust(status, staleAfter string, now time.Time) trustState {
	tr := trustState{Deprecated: status == "deprecated"}
	if !rfc3339DateTime.MatchString(staleAfter) {
		return tr
	}
	t, err := time.Parse(time.RFC3339, staleAfter)
	if err != nil {
		return tr
	}
	if !now.Before(t) {
		tr.Stale = true
		tr.StaleAfter = staleAfter
	}
	return tr
}
