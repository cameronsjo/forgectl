package pr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// macOS's limits, the tightest of the platforms CI runs: PATH_MAX 1024 and
// NAME_MAX 255.
const (
	fixturePathMax = 1024
	fixtureNameMax = 255
	// fixtureInnerPath is the longest path Prepare creates under $TMPDIR:
	// the workspace dir and the allowlist inside it, digits at their widest.
	fixtureInnerPath = "/forgectl-workflow-18446744073709551615/.claude/settings.local.json"
)

// budgetTempDir is a $TMPDIR that holds n copies of fill, in components of
// at most 200 bytes, under a fresh dir in the current $TMPDIR (not
// t.TempDir(), whose test-named path would spend a tenth of PATH_MAX). It
// fails the test, rather than skipping, when Prepare's deepest path under it
// would pass PATH_MAX or a component NAME_MAX, so the fixture cannot quietly
// stop being creatable.
func budgetTempDir(t *testing.T, fill byte, n int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for n > 0 {
		k := min(n, 200)
		dir = filepath.Join(dir, strings.Repeat(string(fill), k))
		n -= k
	}
	if l := len(dir) + len(fixtureInnerPath); l >= fixturePathMax {
		t.Fatalf("fixture path would be %d bytes, past PATH_MAX %d; shorten it", l, fixturePathMax)
	}
	for _, c := range strings.Split(dir, string(filepath.Separator)) {
		if len(c) > fixtureNameMax {
			t.Fatalf("fixture component is %d bytes, past NAME_MAX %d", len(c), fixtureNameMax)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// overflowingTempDir is a $TMPDIR of just enough '<' that a workspace under
// it takes more than the record's room, as workspaceRoom computes it for the
// record Prepare writes. encoding/json writes each '<' as six bytes, so
// about 800 of them overflow while the path stays far under macOS's
// PATH_MAX. It also returns the '<' count, for a plain control of the same
// length.
func overflowingTempDir(t *testing.T) (string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("'<' is not a valid character in a Windows file name")
	}
	// An empty provenance is the shortest the record can hold, so this room
	// is at least the real one, and a dir that overflows it overflows that.
	room, err := workspaceRoom(Breadcrumb{
		Ref: "cameronsjo/forgectl#42", Host: "github.com", Agent: "claude",
		CreatedAt: time.Now().UTC(), Version: breadcrumbVersion, Phase: PhasePrepared, Revision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// budgetTempDir's root is longer than os.TempDir(), so this n is enough.
	n := (room-len(os.TempDir()))/6 + 1
	dir := budgetTempDir(t, '<', n)
	enc, err := json.Marshal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc)-2 <= room {
		t.Fatalf("fixture encodes to %d bytes, within the %d-byte room; it would not exercise the refusal", len(enc)-2, room)
	}
	return dir, n
}

// TestPrepare_RefusesAWorkspaceItCouldNotPark is #974 item 7. Before the
// fix, Prepare wrote the record under an escape-dense $TMPDIR, and the first
// park (markNeedsRepair with an error text at its byte cap) overflowed
// maxBreadcrumbRecordBytes, leaving the record stuck in its phase. Prepare
// now refuses it, names the budget, writes no record, and tears the
// workspace down. The control is a plain $TMPDIR of the same length, which
// Prepare accepts and a park still fits.
//
// Mutation that turns it red: drop the checkParkHeadroom call from
// recordPrepared's unreserved branch.
func TestPrepare_RefusesAWorkspaceItCouldNotPark(t *testing.T) {
	tmp, n := overflowingTempDir(t)
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}

	t.Run("plain control", func(t *testing.T) {
		t.Setenv("TMPDIR", budgetTempDir(t, 'w', n))
		c := testClient(t, ghViewRunner())
		sess, err := c.Prepare(context.Background(), ref, PrepareOpts{Agent: "claude"})
		if err != nil {
			t.Fatalf("Prepare refused a plain workspace of the same length: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(sess.Workspace) })
		if err := c.markNeedsRepair(context.Background(), sess.Path, strings.Repeat("<", 5000)); err != nil {
			t.Fatalf("park of the accepted plain workspace: %v", err)
		}
	})

	t.Setenv("TMPDIR", tmp)
	c := testClient(t, ghViewRunner())
	sess, err := c.Prepare(context.Background(), ref, PrepareOpts{Agent: "claude"})
	if err == nil {
		t.Cleanup(func() { _ = os.RemoveAll(sess.Workspace) })
		if perr := c.markNeedsRepair(context.Background(), sess.Path, strings.Repeat("<", 5000)); perr != nil {
			t.Fatalf("Prepare accepted a workspace its own park then could not write: %v", perr)
		}
		t.Fatal("Prepare accepted a workspace that takes more than the record's headroom")
	}
	if !strings.Contains(err.Error(), "too long to record") || !strings.Contains(err.Error(), "TMPDIR") {
		t.Errorf("error = %q; want it to name the cause and the TMPDIR remedy", err)
	}
	if strings.Contains(err.Error(), "<<") {
		t.Errorf("error = %.80q…; want the path left out of it", err)
	}
	if entries, rerr := os.ReadDir(c.SessionsDir()); rerr != nil || len(entries) != 0 {
		t.Errorf("sessions dir = %v (%v); want no record written", entries, rerr)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "forgectl-workflow-*")); len(left) != 0 {
		t.Errorf("workspace left behind: %d dirs; want it torn down", len(left))
	}
}

// TestPrepare_ReservedRecordRefusesAWorkspaceItCouldNotPark is item 7's
// other branch: completing a reservation checks the merged record before it
// writes, so the reservation keeps its empty workspace and its phase.
//
// Mutation that turns it red: drop the checkParkHeadroom call from
// recordPrepared's transition.
func TestPrepare_ReservedRecordRefusesAWorkspaceItCouldNotPark(t *testing.T) {
	tmp, _ := overflowingTempDir(t)
	t.Setenv("TMPDIR", tmp)
	c := testClient(t, ghViewRunner())
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	path := seedPhaseRecord(t, c, ref, PhasePreparing, "")

	sess, err := c.Prepare(context.Background(), ref, PrepareOpts{Agent: "claude", RecordPath: path})
	if err == nil {
		t.Cleanup(func() { _ = os.RemoveAll(sess.Workspace) })
		t.Fatal("Prepare completed a reservation with a workspace that takes more than the record's headroom")
	}
	if !strings.Contains(err.Error(), "too long to record") {
		t.Errorf("error = %q; want the headroom refusal", err)
	}
	if bc := readRecord(t, path); bc.Phase != PhasePreparing || bc.Workspace != "" {
		t.Errorf("reservation = phase %q workspace %d bytes; want it untouched in preparing", bc.Phase, len(bc.Workspace))
	}
}

// TestCheckParkHeadroomMatchesThePark ties the Prepare check to the write it
// protects: across workspaces straddling the boundary, every record the
// check accepts still encodes once a park fills both free-text fields with
// hostile text and widens its counters, and a plain PATH_MAX workspace with
// the longest ref and host is accepted.
//
// Mutations that turn it red: leave LastError (or RepairReason) out of
// checkParkHeadroom's widest record, and some accepted workspace overflows
// at park; double the free-text widths, and the PATH_MAX control is refused.
func TestCheckParkHeadroomMatchesThePark(t *testing.T) {
	const maxInt = "9223372036854775807"
	base := Breadcrumb{
		Ref:        strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100) + "#" + maxInt,
		Host:       strings.Repeat("h", 253),
		Agent:      "claude",
		CreatedAt:  time.Now().UTC(),
		Provenance: "operator-authored",
		Version:    breadcrumbVersion,
		Phase:      PhasePrepared,
		Revision:   1,
	}
	plain := base
	plain.Workspace = "/" + strings.Repeat("w", 4094)
	if err := checkParkHeadroom(plain); err != nil {
		t.Fatalf("a plain PATH_MAX workspace was refused: %v", err)
	}
	accepted := 0
	for n := 400; n < 1000; n++ {
		bc := base
		bc.Workspace = "/" + strings.Repeat("<", n)
		if checkParkHeadroom(bc) != nil {
			continue
		}
		accepted++
		for _, text := range []string{strings.Repeat("<", 5000), strings.Repeat("\U0001F600", 5000)} {
			parked := bc
			parked.Phase = PhaseNeedsRepair
			parked.Revision = 1 << 62
			parked.Attempts = 1 << 62
			parked.WindowID = widestWindowID
			parked.LastError = breadcrumbText(text)
			parked.RepairReason = breadcrumbText("drain: " + strconv.Itoa(1<<30) + " attempts, last: " + parked.LastError)
			parked.LastAttempt = time.Now().UTC()
			if _, err := encodeBreadcrumb(parked); err != nil {
				t.Fatalf("a %d-'<' workspace passed the check, then its park failed: %v", n, err)
			}
		}
	}
	if accepted == 0 || accepted == 600 {
		t.Fatalf("accepted %d of 600 workspaces; the range must straddle the boundary", accepted)
	}
}
