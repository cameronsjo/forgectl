package sweep

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/attention"
)

// testdata/ledger_v2.json is one read-only run of `forgectl sweep` on a real
// host, with every owner, repository, worker and PR title replaced by a
// synthetic one (acme, repoNN, cos-workerNN, "synthetic ... title N"). It pins
// the shape a real ledger takes: the queue's reader accepts it whole, the v1
// fields are all there, and the ids follow the documented format.
func loadFixture(t *testing.T) ([]byte, Ledger) {
	t.Helper()
	data, err := os.ReadFile("testdata/ledger_v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var l Ledger
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatal(err)
	}
	return data, l
}

func TestFixtureIsSyntheticAndWellFormed(t *testing.T) {
	data, l := loadFixture(t)
	if strings.Contains(string(data), "cameronsjo") {
		t.Error("the fixture names the real owner")
	}
	worker := regexp.MustCompile(`^cos-worker\d\d$`)
	repo := regexp.MustCompile(`^acme/repo\d\d$`)
	if l.SchemaVersion != SchemaVersion || len(l.Workers) == 0 || len(l.PRs) == 0 || len(l.Opinions) == 0 {
		t.Fatalf("fixture is empty: %+v", l)
	}
	for _, w := range l.Workers {
		if !worker.MatchString(w.Name) {
			t.Errorf("worker %q is not synthetic", w.Name)
		}
	}
	for _, p := range l.PRs {
		if !repo.MatchString(p.Repo) || !strings.HasPrefix(p.Title, "synthetic ") || (p.Worker != "" && !worker.MatchString(p.Worker)) {
			t.Errorf("pr %+v is not synthetic", p)
		}
		switch p.State {
		case StateBlocked, StateMarkerClean, StateWaiting, StateDraft:
		default:
			t.Errorf("pr state %q", p.State)
		}
	}
	idRe := regexp.MustCompile(`^(pr:[^#\s]+#\d+|worker:[A-Za-z0-9_-]+:(blocked|plan|report)|sweep:[a-z]+)$`)
	seen := map[string]bool{}
	for _, o := range l.Opinions {
		if !idRe.MatchString(o.ID) || seen[o.ID] {
			t.Errorf("opinion id %q is malformed or repeated", o.ID)
		}
		seen[o.ID] = true
		if o.Action == ActionReview && (o.Target == nil || o.Target.Number == 0) {
			t.Errorf("review opinion %q has no PR target", o.ID)
		}
		if o.Action == ActionUnblock && (o.Target == nil || o.Target.Worker == "") {
			t.Errorf("unblock opinion %q has no worker target", o.ID)
		}
		if strings.HasSuffix(o.ID, ":plan") && (o.Action != ActionNone || o.Target != nil) {
			t.Errorf("plan opinion %q offers an action", o.ID)
		}
	}
}

func TestFixtureCarriesTheV1Fields(t *testing.T) {
	data, _ := loadFixture(t)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"generatedAt", "merged", "dependencyBumps", "reposTouched", "sweepRepos", "openPrs", "filed", "opinions", "workers", "topRepos", "recent"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("v1 field %s is missing", k)
		}
	}
}

func TestFixtureIsAcceptedByTheQueueReader(t *testing.T) {
	data, l := loadFixture(t)
	sw, skipped, err := attention.ParseSweep(data)
	if err != nil || skipped != 0 {
		t.Fatalf("ParseSweep: skipped=%d err=%v", skipped, err)
	}
	if len(sw.Workers) != len(l.Workers) || len(sw.PRs) != len(l.PRs) {
		t.Errorf("reader kept %d/%d workers and %d/%d PRs", len(sw.Workers), len(l.Workers), len(sw.PRs), len(l.PRs))
	}
}
