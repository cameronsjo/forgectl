// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package attention

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strconv"
)

// RowDrain is the drain's stuck-hold row (derived from drain.json at read
// time).
const RowDrain = "drain"

// Tiers: what a row earns on a glanceable surface (the notch). The policy
// lives here, beside the costs, so every consumer reads the same answer.
const (
	// TierRaise rows may expand the surface once: a blocked worker, a
	// waiting desk item, a stuck drain.
	TierRaise = "raise"
	// TierCount rows are counted in a badge only.
	TierCount = "count"
	// TierHidden rows stay in the list and are not shown on the surface.
	TierHidden = "hidden"
)

// KeyLen is the length of a row key in hex characters.
const KeyLen = 16

var keyRe = regexp.MustCompile("^[0-9a-f]{16}$")

// ValidKey reports whether s is a row key as RowKey writes it.
func ValidKey(s string) bool { return keyRe.MatchString(s) }

// RowKey is a stable id for the wait a row stands for: the first 16 hex of
// sha256 over a length-prefixed tuple of the row's kind and the fields fixed
// when the wait was created. Fields that a merge or a herdr read fills in
// later (pane, session on a desk row, worker on a desk row, origin) never
// enter it, so a herdr flap or a sweep merge does not change a row's key.
//
//   - desk: the item name;
//   - q / a: session, turn, item number (a new turn is a new ask);
//   - worker: worker name and block;
//   - pr: owner/repo#n;
//   - drain: the constant "slots-hold".
func RowKey(r Row) string {
	var parts []string
	switch r.Kind {
	case RowDesk:
		parts = []string{r.Tag}
	case KindQuestion, KindAction:
		parts = []string{r.Session, r.Turn, strconv.Itoa(r.N)}
	case RowWorker:
		parts = []string{r.Worker, r.Tag}
	case RowPR:
		parts = []string{r.Tag}
	case RowDrain:
		parts = []string{"slots-hold"}
	default:
		parts = []string{r.Tag, r.Session, r.Turn, strconv.Itoa(r.N), r.Text}
	}
	h := sha256.New()
	field := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])      //nolint:errcheck // hash writes cannot fail
		h.Write([]byte(s)) //nolint:errcheck // hash writes cannot fail
	}
	field(r.Kind)
	for _, p := range parts {
		field(p)
	}
	return hex.EncodeToString(h.Sum(nil))[:KeyLen]
}

// RowTier is the surface tier for a row. Record-derived rows (q / a) never
// raise, whatever their origin: any process of yours can write a record.
func RowTier(r Row) string {
	switch r.Kind {
	case RowDesk, RowWorker, RowDrain:
		return TierRaise
	case RowPR:
		return TierCount
	case KindAction:
		if r.Actor == ActorCameron {
			return TierCount
		}
		return TierHidden
	default:
		return TierHidden
	}
}

// stamp sets each row's key and tier, then drops a row whose key an earlier
// row already holds: the same wait listed twice by one source (a sweep that
// names a worker twice) collapses to its first, lowest-cost row.
func stamp(rows []Row) []Row {
	seen := make(map[string]bool, len(rows))
	out := rows[:0]
	for _, r := range rows {
		r.Key, r.Tier = RowKey(r), RowTier(r)
		if seen[r.Key] {
			continue
		}
		seen[r.Key] = true
		out = append(out, r)
	}
	return out
}
