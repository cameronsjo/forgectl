package drain

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// MaxSpendDays is the widest window `surface usage` sums.
const MaxSpendDays = 365

// SpendDay is one UTC day's cost.
type SpendDay struct {
	Day  string  `json:"day"`
	USD  float64 `json:"usd"`
	Rows int     `json:"rows"`
}

// Spend is the cost of the drain's workers over a window of UTC days.
// Skipped counts usage-daily.jsonl lines that did not parse as a valid
// UsageLine, and lines or rows dated after today. Unpriced counts the
// finished queue rows in the window (closed or failed, with a launch) that
// carry no cost: while it is above zero, TotalUSD is a floor, not the total.
type Spend struct {
	Days     int        `json:"days"`
	TotalUSD float64    `json:"total_usd"`
	ByDay    []SpendDay `json:"by_day"`
	Skipped  int        `json:"skipped"`
	Unpriced int        `json:"unpriced"`
}

// SumSpend adds the cost of every priced row whose UTC day falls in the
// last days days ending at now: the lines of usage-daily.jsonl (usage) and
// the queue rows not yet pruned, read the way prune writes them
// (UsageLines). A row is identified by (name, launch_id); when it is in both,
// the file line counts, since prune writes it before it removes the row. A
// row with no launch id has nothing to match on and always counts. Rows
// without a cost are not counted.
func SumSpend(usage []byte, rows []worker.QueueRow, now time.Time, days int) Spend {
	today := now.UTC()
	first := today.AddDate(0, 0, -(days - 1)).Format(worker.UTCDayLayout)
	last := today.Format(worker.UTCDayLayout)
	type key struct{ name, launchID string }
	seen := map[key]bool{}
	byDay := map[string]SpendDay{}
	total := 0.0
	out := Spend{Days: days, ByDay: []SpendDay{}}
	add := func(u UsageLine) {
		if !u.Valid() || u.Day > last {
			out.Skipped++
			return
		}
		if u.LaunchID != "" {
			k := key{u.Name, u.LaunchID}
			if seen[k] {
				return
			}
			seen[k] = true
		}
		if u.Day < first {
			return
		}
		d := byDay[u.Day]
		d.Day = u.Day
		d.USD += u.CostUSD
		d.Rows++
		byDay[u.Day] = d
		total += u.CostUSD
	}
	for line := range bytes.Lines(usage) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var u UsageLine
		if err := json.Unmarshal(line, &u); err != nil {
			out.Skipped++
			continue
		}
		add(u)
	}
	for _, u := range UsageLines(rows) {
		add(u)
	}
	for _, r := range rows {
		day := r.StateAt.UTC().Format(worker.UTCDayLayout)
		if r.CostUSD == nil && r.LaunchID != "" && (r.State == worker.QueueClosed || r.State == worker.QueueFailed) && day >= first && day <= last {
			out.Unpriced++
		}
	}
	for _, d := range byDay {
		d.USD = roundCents(d.USD)
		out.ByDay = append(out.ByDay, d)
	}
	slices.SortFunc(out.ByDay, func(a, b SpendDay) int { return strings.Compare(a.Day, b.Day) })
	// The total rounds the raw sum, so many small costs are not lost to
	// per-day rounding.
	out.TotalUSD = roundCents(total)
	return out
}

// roundCents rounds to the cent, so sums of float costs print cleanly.
func roundCents(v float64) float64 {
	return math.Round(v*100) / 100
}
