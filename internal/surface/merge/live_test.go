package merge

import (
	"context"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
)

// TestLiveRead runs the real reads against cameronsjo/forgectl#1204, the
// merged drain worker PR the fixtures were captured from. It is read-only
// and runs only with FORGECTL_MERGE_LIVE=1:
//
//	FORGECTL_MERGE_LIVE=1 go test ./internal/surface/merge/ -run TestLiveRead -v
func TestLiveRead(t *testing.T) {
	if os.Getenv("FORGECTL_MERGE_LIVE") != "1" {
		t.Skip("set FORGECTL_MERGE_LIVE=1 to read cameronsjo/forgectl#1204 from GitHub")
	}
	r := Reader{GH: githubauth.Runner(exec.OSRunner{}, githubauth.DefaultHost)}
	snap, err := r.Read(context.Background(), row1204())
	if err != nil {
		t.Fatal(err)
	}
	f := snap.Facts
	if !snap.HasPR || f.PR.Number != 1204 || f.PR.HeadRefOid != head1204 || len(f.Files) != 4 || f.Files[0].HeadMode != "100644" {
		t.Fatalf("snapshot %+v", snap)
	}
	v := evalManual(f)
	t.Logf("verdict %s: %q", v.Result, v.Reasons)
	wantRefusal(t, v, "the PR is MERGED")
}
