package clean

// Test plan for docker.go
//
// parseDockerSize (Classification: pure function)
//   [x] Happy: each documented unit (B/KB/MB/GB/TB) parses to the correct
//       byte count, including a fractional value ("1.2GB")
//   [x] Unhappy: an unrecognized unit or unparseable number is (0, false)
//
// parseDockerDF (Classification: hostile-input parser over ndjson)
//   [x] Happy: a well-formed `docker system df --format '{{json .}}'`
//       response maps every category to its DockerKind, parsing Reclaimable
//   [x] Unhappy: a malformed row is skipped; the other rows still parse
//   [x] Unhappy: a category df didn't report at all comes back Skipped
//
// Client.ScanDocker (Classification: I/O boundary over FakeRunner)
//   [x] Happy: a successful `docker system df` call is parsed via
//       parseDockerDF
//   [x] Unhappy: `docker system df` failing skips EVERY category, each with
//       a reason
//
// Client.PruneDocker (Classification: I/O boundary, argv assertions)
//   [x] Happy: each category's prune command matches docker-cleanup.sh's
//       documented argv exactly (including the until= filters)
//   [x] Invariant: a Skipped category is never attempted
//   [x] Invariant: one category's prune failing does not block the others
//       (isolation)

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestParseDockerSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0B", 0, true},
		{"800MB", 800 * 1024 * 1024, true},
		// 1.5 (not 1.2) keeps the expected byte count an exact integer —
		// 1.2 * 1024^3 is not, so it can't be a compile-time int64 constant.
		{"1.5GB", 1536 * 1024 * 1024, true},
		{"5KB", 5 * 1024, true},
		{"2TB", 2 * 1024 * 1024 * 1024 * 1024, true},
		{"nonsense", 0, false},
		{"12XB", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseDockerSize(tc.in)
			if ok != tc.ok {
				t.Fatalf("parseDockerSize(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("parseDockerSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// dfRow renders one `docker system df --format '{{json .}}'` ndjson line.
func dfRow(typ, reclaimable string) string {
	return `{"Type":"` + typ + `","TotalCount":"1","Active":"0","Size":"1GB","Reclaimable":"` + reclaimable + `"}`
}

func TestParseDockerDF_HappyAllCategories(t *testing.T) {
	out := strings.Join([]string{
		dfRow("Images", "500MB (50%)"),
		dfRow("Containers", "10MB (10%)"),
		dfRow("Local Volumes", "1.5GB (75%)"),
		dfRow("Build Cache", "0B"),
	}, "\n")

	items := parseDockerDF(out)
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4: %+v", len(items), items)
	}
	byKind := map[DockerKind]DockerItem{}
	for _, it := range items {
		byKind[it.Kind] = it
	}
	if byKind[DockerImages].Reclaimable != 500*1024*1024 {
		t.Errorf("Images reclaimable = %d, want %d", byKind[DockerImages].Reclaimable, 500*1024*1024)
	}
	if byKind[DockerContainers].Reclaimable != 10*1024*1024 {
		t.Errorf("Containers reclaimable = %d, want %d", byKind[DockerContainers].Reclaimable, 10*1024*1024)
	}
	if byKind[DockerVolumes].Reclaimable != int64(1.5*1024*1024*1024) {
		t.Errorf("Volumes reclaimable = %d, want %d", byKind[DockerVolumes].Reclaimable, int64(1.5*1024*1024*1024))
	}
	if byKind[DockerBuildCache].Reclaimable != 0 {
		t.Errorf("BuildCache reclaimable = %d, want 0", byKind[DockerBuildCache].Reclaimable)
	}
	for _, it := range items {
		if it.Skipped {
			t.Errorf("category %s must not be skipped when df reported it: %+v", it.Kind, it)
		}
	}
}

func TestParseDockerDF_MalformedRowSkipped_OthersSurvive(t *testing.T) {
	out := strings.Join([]string{
		`{not valid json`,
		dfRow("Images", "500MB"),
	}, "\n")

	items := parseDockerDF(out)
	byKind := map[DockerKind]DockerItem{}
	for _, it := range items {
		byKind[it.Kind] = it
	}
	if byKind[DockerImages].Reclaimable != 500*1024*1024 {
		t.Errorf("Images row must still parse despite a malformed sibling row: %+v", byKind[DockerImages])
	}
	if !byKind[DockerContainers].Skipped {
		t.Errorf("a category df never reported must come back Skipped: %+v", byKind[DockerContainers])
	}
}

func TestScanDocker_ParsesSuccessfulDF(t *testing.T) {
	out := strings.Join([]string{
		dfRow("Images", "500MB"),
		dfRow("Containers", "0B"),
		dfRow("Local Volumes", "0B"),
		dfRow("Build Cache", "0B"),
	}, "\n")
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return out, nil
	}}
	client := New(fake)

	items := client.ScanDocker(context.Background())
	byKind := map[DockerKind]DockerItem{}
	for _, it := range items {
		byKind[it.Kind] = it
	}
	if byKind[DockerImages].Reclaimable != 500*1024*1024 {
		t.Errorf("Images reclaimable = %d, want %d", byKind[DockerImages].Reclaimable, 500*1024*1024)
	}
}

func TestScanDocker_DFFailure_AllCategoriesSkipped(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", errors.New("docker: daemon not running")
	}}
	client := New(fake)

	items := client.ScanDocker(context.Background())
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4", len(items))
	}
	for _, it := range items {
		if !it.Skipped || it.SkipReason == "" {
			t.Errorf("every category must be skipped with a reason on df failure: %+v", it)
		}
	}
}

func TestPruneDocker_ArgvMatchesDockerCleanupShape(t *testing.T) {
	fake := &exec.FakeRunner{}
	client := New(fake)

	items := []DockerItem{
		{Kind: DockerContainers, Reclaimable: 1},
		{Kind: DockerImages, Reclaimable: 1},
		{Kind: DockerVolumes, Reclaimable: 1},
		{Kind: DockerBuildCache, Reclaimable: 1},
	}
	result := client.PruneDocker(context.Background(), items)
	for _, item := range result {
		if item.Err != nil || !item.Applied {
			t.Errorf("%s: expected a successful prune, got %+v", item.Kind, item)
		}
	}

	want := map[string]bool{
		"docker container prune -f --filter until=24h": true,
		"docker image prune -af --filter until=168h":   true,
		"docker volume prune -f":                       true,
		"docker builder prune -f --filter until=168h":  true,
	}
	if len(fake.Calls) != len(want) {
		t.Fatalf("got %d Runner calls, want %d: %+v", len(fake.Calls), len(want), fake.Calls)
	}
	for _, call := range fake.Calls {
		argv := call.Name + " " + strings.Join(call.Args, " ")
		if !want[argv] {
			t.Errorf("unexpected argv: %q", argv)
		}
	}
}

func TestPruneDocker_SkippedCategoryNeverAttempted(t *testing.T) {
	fake := &exec.FakeRunner{}
	client := New(fake)

	items := []DockerItem{{Kind: DockerImages, Skipped: true, SkipReason: "docker unreachable"}}
	result := client.PruneDocker(context.Background(), items)
	if len(fake.Calls) != 0 {
		t.Errorf("a skipped category must never reach the Runner; saw %d calls: %+v", len(fake.Calls), fake.Calls)
	}
	if result[0].Applied || result[0].Err != nil {
		t.Errorf("skipped category must stay untouched: %+v", result[0])
	}
}

func TestPruneDocker_OneFailureDoesNotBlockTheOthers(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		for _, a := range args {
			if a == "container" {
				return "", errors.New("docker: permission denied")
			}
		}
		return "", nil
	}}
	client := New(fake)

	items := []DockerItem{
		{Kind: DockerContainers, Reclaimable: 1},
		{Kind: DockerImages, Reclaimable: 1},
	}
	result := client.PruneDocker(context.Background(), items)

	byKind := map[DockerKind]DockerItem{}
	for _, it := range result {
		byKind[it.Kind] = it
	}
	if byKind[DockerContainers].Err == nil || byKind[DockerContainers].Applied {
		t.Errorf("containers should have failed, not applied: %+v", byKind[DockerContainers])
	}
	if byKind[DockerImages].Err != nil || !byKind[DockerImages].Applied {
		t.Errorf("images prune must still run despite containers' failure (isolation): %+v", byKind[DockerImages])
	}
}
