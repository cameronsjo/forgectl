package merge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

const (
	landedCommit = "1111111111111111111111111111111111111111"
	mainHead     = "2222222222222222222222222222222222222222"
)

// fakeLand is a Lander over in-memory facts and an in-memory audit file.
type fakeLand struct {
	facts   Facts
	hasPR   bool
	readErr error
	moved   []string
	recheck error
	// recheckNames are the required checks Land asked Recheck to compare.
	recheckNames []string
	merges       [][]string
	mergeErr     error
	landing      Landing
	landErr      error
	// foreignMessage, when set, is the merge commit's message; otherwise it
	// is the subject and body of the last gh pr merge call, as GitHub writes
	// a squash this attempt made.
	foreignMessage string
	// landedNeedsCtx makes a landing read fail once its context has ended,
	// as a real gh call does.
	landedNeedsCtx bool
	landings       int
	sleeps         int
	audit          []byte
	auditErr       error
}

func newFakeLand(t *testing.T) *fakeLand {
	f := passingFacts(t)
	return &fakeLand{facts: f, hasPR: true, landing: Landing{State: "MERGED", Merged: true, HeadRefOid: f.PR.HeadRefOid, MergeCommit: landedCommit,
		DefaultBranch: "main", DefaultHead: mainHead, Ancestry: CompareAhead, OnDefault: true}}
}

func (fl *fakeLand) lander() Lander {
	return Lander{
		Read: func(context.Context, Row) (Snapshot, error) {
			return Snapshot{Facts: fl.facts, HasPR: fl.hasPR, NoPR: "merge: no pull request on the worker's branch"}, fl.readErr
		},
		Recheck: func(_ context.Context, _ Facts, checks []string) ([]string, error) {
			fl.recheckNames = checks
			return fl.moved, fl.recheck
		},
		Landed: func(ctx context.Context, _ Facts) (Landing, error) {
			fl.landings++
			if fl.landedNeedsCtx && ctx.Err() != nil {
				return Landing{}, ctx.Err()
			}
			l := fl.landing
			switch {
			case fl.foreignMessage != "":
				l.MergeMessage = fl.foreignMessage
			case len(fl.merges) > 0:
				args := fl.merges[len(fl.merges)-1]
				l.MergeMessage = args[len(args)-3] + "\n\n" + args[len(args)-1]
			}
			return l, fl.landErr
		},
		Merge: func(_ context.Context, args []string) error {
			fl.merges = append(fl.merges, args)
			return fl.mergeErr
		},
		Audit: func(fn func([]byte) ([]byte, error)) error {
			if fl.auditErr != nil {
				return fl.auditErr
			}
			data, err := fn(fl.audit)
			if err == nil {
				fl.audit = append(fl.audit, data...)
			}
			return err
		},
		Now:   func() time.Time { return time.Date(2026, 10, 9, 21, 0, 0, 0, time.UTC) },
		Sleep: func(context.Context, time.Duration) { fl.sleeps++ },
	}
}

func (fl *fakeLand) land(t *testing.T, s config.MergeSettings, by By, dryRun bool) Outcome {
	t.Helper()
	return fl.lander().Land(context.Background(), s, by, fl.facts.Row, dryRun)
}

func (fl *fakeLand) results(t *testing.T) []string {
	t.Helper()
	entries, brk := ParseAudit(fl.audit)
	if brk != nil {
		t.Fatalf("the audit chain broke: %+v", brk)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Line.Result)
	}
	return out
}

func TestLandMerges(t *testing.T) {
	fl := newFakeLand(t)
	out := fl.land(t, goodSettings(), ByCLI, false)
	if out.Result != LandMerged || out.MergeCommit != landedCommit || out.PR != 1204 || out.Head != head1204 || out.AuditNote != "" {
		t.Fatalf("outcome %+v", out)
	}
	if len(fl.merges) != 1 {
		t.Fatalf("%d merges", len(fl.merges))
	}
	if want := goodSettings().Repos[0].RequiredChecks; len(want) == 0 || !slices.Equal(fl.recheckNames, want) {
		t.Fatalf("the re-read compared checks %q, want the required %q", fl.recheckNames, want)
	}
	args := fl.merges[0]
	want := []string{"pr", "merge", "1204", "-R", "github.com/cameronsjo/forgectl", "--squash", "--match-head-commit", head1204,
		"--subject", "fix(upgrade): say when brew's update lock is held", "--body"}
	if !slices.Equal(args[:len(want)], want) || len(args) != len(want)+1 {
		t.Fatalf("args %q", args)
	}
	for _, a := range args {
		if a == "--admin" || a == "--auto" || a == "--merge" || a == "--rebase" || a == "--delete-branch" {
			t.Fatalf("args carry %s: %q", a, args)
		}
	}
	body := args[len(args)-1]
	if !strings.Contains(body, "Audit-line: sha256:"+out.AuditLine+"\n") || !strings.HasSuffix(body, "\n\nMerged-By: forgectl-cli\n") {
		t.Fatalf("body:\n%s", body)
	}
	if got := fl.results(t); !slices.Equal(got, []string{AuditMerging, AuditMerged}) {
		t.Fatalf("audit results %q", got)
	}
	entries, _ := ParseAudit(fl.audit)
	if entries[0].Hash != out.AuditLine || entries[1].Line.Attempt != out.AuditLine || entries[1].Line.MergeCommit != landedCommit ||
		entries[1].Line.PolicyHash != PolicyHash(goodSettings()) || len(entries[1].Line.Markers) != 2 || len(entries[1].Line.Checks) == 0 ||
		entries[1].Line.Actor != ByCLI || entries[1].Line.Repo != "cameronsjo/forgectl" || entries[1].Line.RepoID != forgectlID {
		t.Fatalf("audit lines %+v", entries)
	}
	// The drain's merge writes its own trailer, and needs auto.
	fl = newFakeLand(t)
	s := goodSettings()
	s.Mode = config.MergeAuto
	if out := fl.land(t, s, ByDrain, false); out.Result != LandMerged || !strings.HasSuffix(fl.merges[0][len(fl.merges[0])-1], "Merged-By: forgectl-drain\n") {
		t.Fatalf("drain merge: %+v", out)
	}
}

func TestLandRefusesAndAudits(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeLand, *config.MergeSettings)
		by     By
		want   string
	}{
		"a draft PR": {func(fl *fakeLand, _ *config.MergeSettings) { fl.facts.PR.IsDraft = true }, ByCLI, "the PR is a draft"},
		"mode off": {func(_ *fakeLand, s *config.MergeSettings) {
			s.Mode, s.OffReason = config.MergeOff, "[surface.merge] mode is off"
		}, ByCLI, "mode is off"},
		"the drain on manual":  {func(*fakeLand, *config.MergeSettings) {}, ByDrain, `only with [surface.merge] mode "auto"`},
		"no PR":                {func(fl *fakeLand, _ *config.MergeSettings) { fl.hasPR = false }, ByCLI, "no PR: merge: no pull request"},
		"a breaking title":     {func(fl *fakeLand, _ *config.MergeSettings) { fl.facts.PR.Title = "feat!: drop it" }, ByCLI, "is not a subject forgectl merges with"},
		"a title with a ref":   {func(fl *fakeLand, _ *config.MergeSettings) { fl.facts.PR.Title = "fix: closes #9" }, ByCLI, "holds an issue reference"},
		"moved before merging": {func(fl *fakeLand, _ *config.MergeSettings) { fl.moved = []string{"the head is x, it was y"} }, ByCLI, "the PR changed between the verdict and the merge: the head is x"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fl, s := newFakeLand(t), goodSettings()
			c.mutate(fl, &s)
			out := fl.land(t, s, c.by, false)
			if out.Result != LandRefused || !slices.ContainsFunc(out.Reasons, func(r string) bool { return strings.Contains(r, c.want) }) {
				t.Fatalf("outcome %+v; want refused naming %q", out, c.want)
			}
			if len(fl.merges) != 0 {
				t.Fatalf("merged anyway: %q", fl.merges)
			}
			if got := fl.results(t); !slices.Equal(got, []string{AuditRefused}) {
				t.Fatalf("audit %q", got)
			}
			// The same refusal again is not written again.
			again := fl.land(t, s, c.by, false)
			if again.Result != LandRefused || again.AuditNote != "the same refusal is already in the audit file" || len(fl.results(t)) != 1 {
				t.Fatalf("a repeat: %+v, audit %q", again, fl.results(t))
			}
		})
	}
}

func TestLandUnreadableWritesNothing(t *testing.T) {
	for name, mutate := range map[string]func(*fakeLand){
		"the read":    func(fl *fakeLand) { fl.readErr = errors.New("HTTP 502") },
		"the recheck": func(fl *fakeLand) { fl.recheck = errors.New("HTTP 502") },
	} {
		t.Run(name, func(t *testing.T) {
			fl := newFakeLand(t)
			mutate(fl)
			out := fl.land(t, goodSettings(), ByCLI, false)
			if out.Result != LandUnreadable || out.Err == nil || len(fl.merges) != 0 || len(fl.audit) != 0 {
				t.Fatalf("outcome %+v, merges %d, audit %q", out, len(fl.merges), fl.audit)
			}
		})
	}
}

func TestLandDryRun(t *testing.T) {
	fl := newFakeLand(t)
	if out := fl.land(t, goodSettings(), ByCLI, true); out.Result != LandWouldMerge || len(fl.merges) != 0 || len(fl.audit) != 0 {
		t.Fatalf("a passing dry run: %+v, %d merges, audit %q", out, len(fl.merges), fl.audit)
	}
	fl = newFakeLand(t)
	fl.facts.PR.IsDraft = true
	if out := fl.land(t, goodSettings(), ByCLI, true); out.Result != LandRefused || len(fl.audit) != 0 {
		t.Fatalf("a refused dry run: %+v, audit %q", out, fl.audit)
	}
}

func TestLandFailures(t *testing.T) {
	t.Run("an unwritable attempt line merges nothing", func(t *testing.T) {
		fl := newFakeLand(t)
		fl.auditErr = errors.New("disk full")
		out := fl.land(t, goodSettings(), ByCLI, false)
		if out.Result != LandFailed || len(fl.merges) != 0 || !strings.Contains(out.Reasons[0], "nothing was merged") {
			t.Fatalf("%+v, %d merges", out, len(fl.merges))
		}
	})
	t.Run("gh fails and the PR is not merged", func(t *testing.T) {
		fl := newFakeLand(t)
		fl.mergeErr = errors.New("Pull request is not mergeable")
		fl.landing = Landing{State: "OPEN"}
		out := fl.land(t, goodSettings(), ByCLI, false)
		if out.Result != LandFailed || fl.landings != 1 || !strings.Contains(out.Reasons[0], "gh pr merge failed: Pull request is not mergeable") {
			t.Fatalf("%+v, %d landing reads", out, fl.landings)
		}
		if got := fl.results(t); !slices.Equal(got, []string{AuditMerging, AuditFailed}) {
			t.Fatalf("audit %q", got)
		}
	})
	t.Run("gh fails but GitHub says it merged", func(t *testing.T) {
		fl := newFakeLand(t)
		fl.mergeErr = errors.New("signal: killed")
		out := fl.land(t, goodSettings(), ByCLI, false)
		if out.Result != LandMerged || !slices.ContainsFunc(out.Reasons, func(r string) bool { return strings.Contains(r, "gh pr merge reported: signal: killed") }) {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("the merge commit never shows on the default branch", func(t *testing.T) {
		fl := newFakeLand(t)
		fl.landing.OnDefault, fl.landing.Ancestry = false, "diverged"
		out := fl.land(t, goodSettings(), ByCLI, false)
		if out.Result != LandUnconfirmed || fl.landings != landingTries || fl.sleeps != landingTries-1 || !strings.Contains(out.Reasons[0], `compare status "diverged"`) {
			t.Fatalf("%+v, %d reads, %d sleeps", out, fl.landings, fl.sleeps)
		}
		if got := fl.results(t); !slices.Equal(got, []string{AuditMerging, AuditUnconfirmed}) {
			t.Fatalf("audit %q", got)
		}
	})
	t.Run("merged at another head", func(t *testing.T) {
		fl := newFakeLand(t)
		fl.landing.HeadRefOid = base1204
		if out := fl.land(t, goodSettings(), ByCLI, false); out.Result != LandUnconfirmed || !strings.Contains(out.Reasons[0], "merged at head a398e7258d0a") {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("a deadline kills gh after GitHub merged", func(t *testing.T) {
		// The merge's context ends with gh (the 3-minute cap), after GitHub
		// made the squash: the confirmation still reads it back, on a
		// context of its own (T10.4 security review I2).
		fl := newFakeLand(t)
		fl.landedNeedsCtx = true
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		l := fl.lander()
		l.Merge = func(_ context.Context, args []string) error {
			fl.merges = append(fl.merges, args)
			cancel()
			return errors.New("signal: killed")
		}
		out := l.Land(ctx, goodSettings(), ByCLI, fl.facts.Row, false)
		if out.Result != LandMerged || out.MergeCommit != landedCommit {
			t.Fatalf("%+v", out)
		}
		if got := fl.results(t); !slices.Equal(got, []string{AuditMerging, AuditMerged}) {
			t.Fatalf("audit %q", got)
		}
	})
	t.Run("something else merged it", func(t *testing.T) {
		for name, mergeErr := range map[string]error{"gh failed": errors.New("Pull request is already merged"), "gh succeeded": nil} {
			t.Run(name, func(t *testing.T) {
				fl := newFakeLand(t)
				fl.mergeErr = mergeErr
				fl.foreignMessage = "fix: merged in the web UI\n\nCloses #12\n"
				out := fl.land(t, goodSettings(), ByCLI, false)
				if out.Result != LandMergedElsewhere || !strings.Contains(out.Reasons[0], "does not carry this attempt's Audit-line") {
					t.Fatalf("%+v", out)
				}
				if got := fl.results(t); !slices.Equal(got, []string{AuditMerging, AuditMergedElsewhere}) {
					t.Fatalf("audit %q", got)
				}
			})
		}
	})
	t.Run("gh fails and GitHub cannot be read after it", func(t *testing.T) {
		fl := newFakeLand(t)
		fl.mergeErr = errors.New("signal: killed")
		fl.landErr = errors.New("HTTP 502")
		out := fl.land(t, goodSettings(), ByCLI, false)
		if out.Result != LandUnknown || fl.landings != 1 || !strings.Contains(out.Reasons[0], "check it by hand") {
			t.Fatalf("%+v, %d landing reads", out, fl.landings)
		}
		if got := fl.results(t); !slices.Equal(got, []string{AuditMerging, AuditUnknown}) {
			t.Fatalf("audit %q", got)
		}
	})
	t.Run("gh succeeds but GitHub does not say merged", func(t *testing.T) {
		// A merge queue: gh exits 0 having enqueued the PR.
		fl := newFakeLand(t)
		fl.landing = Landing{State: "OPEN"}
		out := fl.land(t, goodSettings(), ByCLI, false)
		if out.Result != LandUnconfirmed || fl.landings != landingTries || !strings.Contains(out.Reasons[0], "merge queue") {
			t.Fatalf("%+v, %d landing reads", out, fl.landings)
		}
		if got := fl.results(t); !slices.Equal(got, []string{AuditMerging, AuditUnconfirmed}) {
			t.Fatalf("audit %q", got)
		}
	})
	t.Run("the landing read fails", func(t *testing.T) {
		fl := newFakeLand(t)
		fl.landErr = errors.New("HTTP 502")
		if out := fl.land(t, goodSettings(), ByCLI, false); out.Result != LandUnconfirmed || out.Err == nil {
			t.Fatalf("%+v", out)
		}
	})
}

func TestCarriesAuditLine(t *testing.T) {
	h := strings.Repeat("a", 64)
	for msg, want := range map[string]bool{
		"fix: x\n\nMerged by forgectl\n\nAudit-line: sha256:" + h + "\nPolicy: sha256:b\n": true,
		"fix: x\r\n\r\nAudit-line: sha256:" + h + "\r\n":                                   true,
		"fix: x\n\nAudit-line: sha256:" + h:                                                true,
		"fix: x\n\nAudit-line: sha256:" + h + "0\n":                                        false,
		"fix: x\n\n Audit-line: sha256:" + h + "\n":                                        false,
		"fix: x Audit-line: sha256:" + h + "\n":                                            false,
		"":                                                                                 false,
	} {
		if got := CarriesAuditLine(msg, h); got != want {
			t.Errorf("CarriesAuditLine(%q) = %v, want %v", msg, got, want)
		}
	}
	if CarriesAuditLine("Audit-line: sha256:\n", "") {
		t.Fatal("an empty hash matched")
	}
}
