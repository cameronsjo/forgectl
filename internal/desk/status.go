package desk

import (
	"strconv"
	"strings"
	"time"
)

// StepStatus is one row of a batch's done/<name>.d/status.tsv, which the
// runner rewrites as each step starts and ends. ID and Deps are validated step
// ids; State is one of pending, running, ok, failed, cancelled, skipped.
type StepStatus struct {
	ID    string
	State string
	// RC is nil until the step ends.
	RC *int
	// Start and End are zero until known.
	Start time.Time
	End   time.Time
	Deps  []string
}

// Step states as status.tsv spells them. They match the runner's own
// (batch_unix.go), which is Unix-only; these are not, so a frame can be drawn
// anywhere. TestStepStateNamesMatchTheRunner pins the two together.
const (
	StepPending   = "pending"
	StepRunning   = "running"
	StepOK        = "ok"
	StepFailed    = "failed"
	StepCancelled = "cancelled"
	StepSkipped   = "skipped"
)

// ParseStatusTSV reads status.tsv: a header line, then one tab-separated row
// per step (step, state, rc, start, end, deps), "-" for an absent value. A row
// that does not have six fields or whose id is not a step id is dropped, so a
// half-written or foreign file yields fewer rows, never a wrong one.
func ParseStatusTSV(data []byte) []StepStatus {
	var out []StepStatus
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 || line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 || !stepIDRe.MatchString(f[0]) {
			continue
		}
		s := StepStatus{ID: f[0], State: f[1]}
		if rc, err := strconv.Atoi(f[2]); err == nil {
			s.RC = &rc
		}
		s.Start, s.End = epochTime(f[3]), epochTime(f[4])
		if f[5] != "-" && f[5] != "" {
			for _, dep := range strings.Split(f[5], ",") {
				if stepIDRe.MatchString(dep) {
					s.Deps = append(s.Deps, dep)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// epochTime parses status.tsv's "seconds.millis" stamps; "-" or junk is zero.
func epochTime(s string) time.Time {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(v * 1000)).UTC()
}
