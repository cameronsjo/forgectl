//go:build liveprobe

package tasks

// This file is a probe, not a test of this package. It measures what a real
// Vikunja instance does when a task is updated through the credentialed
// client, so CompleteTask is built on measured behaviour instead of on the
// API's documentation. It WRITES TO THE LIVE BOARD and is excluded from every
// normal build by the `liveprobe` tag.
//
// Run it in two phases, with the operator's go, from this package directory:
//
//	go test -tags liveprobe -run TestLiveProbe -count=1 -v . \
//	  -liveprobe.phase=create -liveprobe.project=<id> -liveprobe.out=<abs dir>
//
// then, after the operator has added a label, an assignee, and a reminder to
// scratch task A in the web UI and moved it one column:
//
//	go test -tags liveprobe -run TestLiveProbe -count=1 -v . \
//	  -liveprobe.phase=update -liveprobe.a=<id> -liveprobe.b=<id> \
//	  -liveprobe.c=<id> -liveprobe.d=<id> -liveprobe.out=<abs dir>
//
// The token is a short-expiry scratch bot token stored under the keychain
// service "forgectl-liveprobe-scratch". Delete that entry and revoke the token
// when the probe is finished. The probe talks only to the default host.
//
// The token is read through ReadToken and the client is built by NewClient, so
// host pinning, the TLS floor, and the redirect refusal all apply. Raw
// responses carry real user objects and are written only under -liveprobe.out,
// which must be outside the repository.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// probeMarker is in the title of every task this probe creates, and the update
// phase refuses any id whose title lacks it. It is what stops a mistyped id
// from marking a real task done.
const probeMarker = "[forgectl-liveprobe]"

// Fixed write budgets. A probe that loops is a probe that can flood a shared
// board; each phase makes exactly this many writes or stops.
const (
	probeCreateWrites = 4
	probeUpdateWrites = 4
)

// probeDefaultService is deliberately NOT the name the `done` verb reads. The
// probe runs before the host rule (ADR 0009 §3, D9) is installed, and until
// then any `tasks` verb will send a named keychain entry to any public host.
// The probe's token is a short-expiry scratch token under this throwaway
// name, removed and revoked when the probe is finished.
const probeDefaultService = "forgectl-liveprobe-scratch"

// probeRepeatSeconds is scratch task B's repeat interval: one day.
const probeRepeatSeconds = 86400

var (
	probePhase    = flag.String("liveprobe.phase", "", "create or update")
	probeService  = flag.String("liveprobe.keychain-service", probeDefaultService, "login keychain service holding a short-expiry scratch bot token")
	probeProject  = flag.Int("liveprobe.project", 0, "project id to create the scratch tasks in (create phase)")
	probeOut      = flag.String("liveprobe.out", "", "absolute directory OUTSIDE the repository for raw saves")
	probeA        = flag.Int("liveprobe.a", 0, "scratch task A's id (update phase)")
	probeB        = flag.Int("liveprobe.b", 0, "scratch task B's id (update phase)")
	probeC        = flag.Int("liveprobe.c", 0, "scratch task C's id (update phase)")
	probeD        = flag.Int("liveprobe.d", 0, "scratch task D's id (update phase)")
	probeMissing  = flag.Int("liveprobe.missing-id", 999999999, "an id that does not exist")
	probeForeign  = flag.Int("liveprobe.foreign-id", 0, "optional: a task id in a project this credential is not shared")
	probeFixtures = flag.Bool("liveprobe.fixtures", true, "also write sanitized fixtures beside the raw saves")
)

// probe carries one run's state: the client, the write budget, and every id
// created, so the ids can be printed on every exit path.
type probe struct {
	t          *testing.T
	client     *Client
	outDir     string
	writesLeft int
	created    []int
}

func TestLiveProbe(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		if os.Getenv(name) != "" {
			t.Fatalf("%s is set: refusing to run a credentialed probe through a proxy", name)
		}
	}
	outDir := probeOutDir(t)

	ctx := context.Background()
	runner := exec.OSRunner{}
	token, err := ReadToken(ctx, runner, *probeService)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	// No host flag: the probe only ever talks to the default host.
	client, err := NewClient(ctx, runner, DefaultHost, token)
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	p := &probe{t: t, client: client, outDir: outDir}
	t.Cleanup(func() {
		t.Logf("PROBE IDS CREATED THIS RUN: %v (delete them in the web UI when the probe is finished)", p.created)
	})

	switch *probePhase {
	case "create":
		p.writesLeft = probeCreateWrites
		p.phaseCreate(ctx)
	case "update":
		p.writesLeft = probeUpdateWrites
		p.phaseUpdate(ctx)
	default:
		t.Fatalf("-liveprobe.phase must be create or update, got %q", *probePhase)
	}
}

// probeOutDir validates -liveprobe.out: absolute, existing, and not inside the
// module this test file lives in.
func probeOutDir(t *testing.T) string {
	t.Helper()
	if *probeOut == "" || !filepath.IsAbs(*probeOut) {
		t.Fatalf("-liveprobe.out must be an absolute directory, got %q", *probeOut)
	}
	out, err := filepath.EvalSymlinks(*probeOut)
	if err != nil {
		t.Fatalf("-liveprobe.out %q: %v", *probeOut, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, err := filepath.EvalSymlinks(moduleRoot(t, wd))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	rel, err := filepath.Rel(root, out)
	if err != nil {
		t.Fatalf("relate %q to the module root: %v", out, err)
	}
	if rel == "." || !strings.HasPrefix(rel, "..") {
		t.Fatalf("-liveprobe.out %q is inside the repository; raw saves carry real user objects and must live outside it", out)
	}
	return out
}

func moduleRoot(t *testing.T, dir string) string {
	t.Helper()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

// write spends one unit of the write budget, then issues the request. The
// budget is checked BEFORE the call so a bug in the probe cannot exceed it.
func (p *probe) write(ctx context.Context, method, path string, payload any) ([]byte, error) {
	p.t.Helper()
	if p.writesLeft <= 0 {
		p.t.Fatalf("write budget exhausted before %s %s", method, path)
	}
	p.writesLeft--
	return p.client.do(ctx, method, path, nil, payload, unauthorizedOnWrite)
}

func (p *probe) phaseCreate(ctx context.Context) {
	t := p.t
	if *probeProject <= 0 {
		t.Fatal("-liveprobe.project is required for the create phase")
	}
	// One GET must pass first: a project this credential cannot read is one
	// the probe must not write to.
	if _, err := p.client.FetchProject(ctx, *probeProject); err != nil {
		t.Fatalf("pre-read of project %d failed, no write attempted: %v", *probeProject, err)
	}

	stamp := time.Now().UTC().Format(time.RFC3339)
	due := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339)
	specs := []struct {
		name    string
		payload map[string]any
	}{
		{"A", map[string]any{
			"title":       probeMarker + " A " + stamp,
			"description": "Scratch task A for the forgectl update probe. Safe to delete.",
			"priority":    3,
			"due_date":    due,
		}},
		{"B", map[string]any{
			"title":        probeMarker + " B " + stamp,
			"description":  "Scratch task B (repeating) for the forgectl update probe. Safe to delete.",
			"due_date":     due,
			"repeat_after": probeRepeatSeconds,
		}},
		{"C", map[string]any{
			"title":       probeMarker + " C " + stamp,
			"description": "Scratch task C (repeat_mode set, repeat_after zero) for the forgectl update probe. Safe to delete.",
			"due_date":    due,
			"repeat_mode": 1,
		}},
		{"D", map[string]any{
			"title":       probeMarker + " D " + stamp,
			"description": "Scratch task D (minimal-body update) for the forgectl update probe. Safe to delete.",
			"priority":    2,
			"due_date":    due,
		}},
	}
	for _, spec := range specs {
		body, err := p.write(ctx, http.MethodPut, fmt.Sprintf("/projects/%d/tasks", *probeProject), spec.payload)
		if err != nil {
			t.Fatalf("create %s: unexpected failure, stopping: %v", spec.name, err)
		}
		var created Task
		if err := json.Unmarshal(body, &created); err != nil {
			t.Fatalf("create %s: decode: %v", spec.name, err)
		}
		p.created = append(p.created, created.ID)
		if created.ProjectID != *probeProject {
			t.Fatalf("create %s: task %d landed in project %d, intended %d — stopping",
				spec.name, created.ID, created.ProjectID, *probeProject)
		}
		p.save("create-"+spec.name+".json", body)
		t.Logf("RESULT created %s: id=%d project=%d", spec.name, created.ID, created.ProjectID)
	}
	t.Log("NEXT (operator, web UI): add a label, an assignee, and a reminder to A and move it one column, then run the update phase.")
}

func (p *probe) phaseUpdate(ctx context.Context) {
	t := p.t
	if *probeA <= 0 || *probeB <= 0 || *probeC <= 0 || *probeD <= 0 {
		t.Fatal("-liveprobe.a, -liveprobe.b, -liveprobe.c, and -liveprobe.d are required for the update phase")
	}

	// Task A: the full raw object, echoed back with done and description
	// replaced. This is the request CompleteTask will make.
	before := p.readMarked(ctx, *probeA)
	p.saveRaw("task-A-before.json", before)
	trailer := fmt.Sprintf("closed-by: liveprobe via forgectl tasks liveprobe %s — probe evidence",
		time.Now().UTC().Format(time.RFC3339))
	sent := cloneRaw(before)
	sent["done"] = json.RawMessage("true")
	description, ok := strictString(before["description"])
	if !ok {
		t.Fatalf("task %d's description is not a JSON string: refusing to build an update from it", *probeA)
	}
	sent["description"] = mustJSON(t, appendTrailer(description, trailer))
	respBody, err := p.write(ctx, http.MethodPost, fmt.Sprintf("/tasks/%d", *probeA), sent)
	if err != nil {
		t.Fatalf("RESULT update A refused after a passing read, stopping: %v", err)
	}
	p.save("task-A-update-response.json", respBody)
	after := p.readRaw(ctx, *probeA)
	p.saveRaw("task-A-after.json", after)

	changed := changedKeys(before, after)
	t.Logf("RESULT A changed keys (before read vs after read): %v", changed)
	t.Logf("RESULT A done after update: %s", string(after["done"]))
	t.Logf("RESULT A trailer is the last line: %v", strings.HasSuffix(rawString(after["description"]), trailer))
	t.Logf("RESULT A keys holding a user object (names only): %v", userObjectKeys(after))
	t.Logf("RESULT A user-object keys that changed: %v", intersect(changed, userObjectKeys(after)))

	// Task B: a repeating task marked done. Does it stay done, or reset?
	beforeB := p.readMarked(ctx, *probeB)
	p.saveRaw("task-B-before.json", beforeB)
	sentB := cloneRaw(beforeB)
	sentB["done"] = json.RawMessage("true")
	if _, err := p.write(ctx, http.MethodPost, fmt.Sprintf("/tasks/%d", *probeB), sentB); err != nil {
		t.Fatalf("RESULT update B refused, stopping: %v", err)
	}
	afterB := p.readRaw(ctx, *probeB)
	p.saveRaw("task-B-after.json", afterB)
	t.Logf("RESULT B (repeating) done after update: %s", string(afterB["done"]))
	t.Logf("RESULT B changed keys: %v", changedKeys(beforeB, afterB))

	// Task C: repeat_mode set with repeat_after zero. A refusal keyed on
	// repeat_after alone would miss this one.
	beforeC := p.readMarked(ctx, *probeC)
	p.saveRaw("task-C-before.json", beforeC)
	t.Logf("RESULT C repeat fields before: repeat_after=%s repeat_mode=%s",
		string(beforeC["repeat_after"]), string(beforeC["repeat_mode"]))
	sentC := cloneRaw(beforeC)
	sentC["done"] = json.RawMessage("true")
	if _, err := p.write(ctx, http.MethodPost, fmt.Sprintf("/tasks/%d", *probeC), sentC); err != nil {
		t.Fatalf("RESULT update C refused, stopping: %v", err)
	}
	afterC := p.readRaw(ctx, *probeC)
	p.saveRaw("task-C-after.json", afterC)
	t.Logf("RESULT C (repeat_mode) done after update: %s", string(afterC["done"]))
	t.Logf("RESULT C changed keys: %v", changedKeys(beforeC, afterC))

	// Task D: a MINIMAL body, done and description only. Every other key that
	// changes here is one the server reset because it was left out — the
	// reason CompleteTask echoes the whole object.
	beforeD := p.readMarked(ctx, *probeD)
	p.saveRaw("task-D-before.json", beforeD)
	minimal := map[string]json.RawMessage{
		"done":        json.RawMessage("true"),
		"description": beforeD["description"],
	}
	if _, err := p.write(ctx, http.MethodPost, fmt.Sprintf("/tasks/%d", *probeD), minimal); err != nil {
		t.Fatalf("RESULT minimal-body update of D refused, stopping: %v", err)
	}
	afterD := p.readRaw(ctx, *probeD)
	p.saveRaw("task-D-after.json", afterD)
	t.Logf("RESULT D keys reset by a minimal body: %v", changedKeys(beforeD, afterD))

	// Statuses for the two reads CompleteTask must tell apart.
	_, err = p.client.get(ctx, fmt.Sprintf("/tasks/%d", *probeMissing), nil)
	t.Logf("RESULT read of nonexistent id %d: %s", *probeMissing, classify(err))
	if *probeForeign > 0 {
		_, err = p.client.get(ctx, fmt.Sprintf("/tasks/%d", *probeForeign), nil)
		t.Logf("RESULT read of a task in an unshared project: %s", classify(err))
	} else {
		t.Log("RESULT read of a task in an unshared project: NOT MEASURED (-liveprobe.foreign-id not given)")
	}

	if *probeFixtures {
		p.saveRaw("fixture-task-update-before.json", sanitizeFixture(before))
		p.saveRaw("fixture-task-update-after.json", sanitizeFixture(after))
		t.Logf("sanitized fixtures written under %s; review them, then copy to internal/tasks/testdata/", p.outDir)
	}
	t.Log("NEXT (operator, web UI): say how the trailer renders on A, reopen A, and say what the reopen restored.")
}

// readMarked reads id raw and refuses to continue unless its title carries the
// probe marker.
func (p *probe) readMarked(ctx context.Context, id int) map[string]json.RawMessage {
	p.t.Helper()
	raw := p.readRaw(ctx, id)
	title, ok := strictString(raw["title"])
	if !ok || !strings.Contains(title, probeMarker) {
		p.t.Fatalf("task %d's title lacks %q: refusing to update a task this probe did not create", id, probeMarker)
	}
	return raw
}

func (p *probe) readRaw(ctx context.Context, id int) map[string]json.RawMessage {
	p.t.Helper()
	body, err := p.client.get(ctx, fmt.Sprintf("/tasks/%d", id), nil)
	if err != nil {
		p.t.Fatalf("read task %d: %v", id, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		p.t.Fatalf("read task %d: decode: %v", id, err)
	}
	return raw
}

func (p *probe) save(name string, body []byte) {
	p.t.Helper()
	if err := os.WriteFile(filepath.Join(p.outDir, name), body, 0o600); err != nil {
		p.t.Fatalf("save %s: %v", name, err)
	}
}

func (p *probe) saveRaw(name string, raw map[string]json.RawMessage) {
	p.t.Helper()
	body, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		p.t.Fatalf("encode %s: %v", name, err)
	}
	p.save(name, body)
}

// classify renders a read failure as its class, never the server's text.
func classify(err error) string {
	switch {
	case err == nil:
		return "200 (readable)"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized (401/403): " + err.Error()
	case errors.Is(err, ErrUnexpectedStatus):
		return "unexpected status: " + err.Error()
	case errors.Is(err, ErrUnreachable):
		return "unreachable"
	default:
		return "other: " + err.Error()
	}
}

func cloneRaw(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// strictString decodes raw as a JSON string and reports whether it was one. A
// caller building a WRITE from the value must use this, not rawString: a
// description that is not a string must stop the update, never read as empty.
func strictString(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// rawString is the lenient form, for logging and comparison only.
func rawString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func appendTrailer(description, trailer string) string {
	if strings.TrimSpace(description) == "" {
		return trailer
	}
	return description + "\n\n" + trailer
}

// changedKeys names every key whose value differs semantically between a and
// b, including keys present on only one side.
func changedKeys(a, b map[string]json.RawMessage) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range []map[string]json.RawMessage{a, b} {
		for k := range m {
			if seen[k] {
				continue
			}
			seen[k] = true
			if !semanticEqual(a[k], b[k]) {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

func semanticEqual(a, b json.RawMessage) bool {
	var av, bv any
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// userObjectKeys names top-level keys whose value is an object, or a list of
// objects, carrying a "username" field.
func userObjectKeys(raw map[string]json.RawMessage) []string {
	var out []string
	for k, v := range raw {
		var obj map[string]json.RawMessage
		if json.Unmarshal(v, &obj) == nil {
			if _, ok := obj["username"]; ok {
				out = append(out, k)
			}
			continue
		}
		var list []map[string]json.RawMessage
		if json.Unmarshal(v, &list) == nil {
			for _, item := range list {
				if _, ok := item["username"]; ok {
					out = append(out, k)
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, k := range b {
		in[k] = true
	}
	var out []string
	for _, k := range a {
		if in[k] {
			out = append(out, k)
		}
	}
	return out
}

// fixtureKeys is the allowlist a sanitized fixture keeps. Anything else the
// server returns is dropped, and every kept value is replaced by a synthetic
// one of the same shape, so a fixture carries the task's STRUCTURE and nothing
// from the board.
var fixtureKeys = []string{
	"id", "title", "description", "done", "done_at", "due_date", "start_date", "end_date",
	"reminders", "project_id", "repeat_after", "repeat_mode", "priority", "assignees", "labels",
	"hex_color", "percent_done", "identifier", "index", "related_tasks", "attachments",
	"cover_image_attachment_id", "is_favorite", "created", "updated", "bucket_id", "position",
	"reactions", "created_by",
}

func sanitizeFixture(raw map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, k := range fixtureKeys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		out[k] = synthetic(k, v)
	}
	return out
}

// synthetic replaces v with a fixed value of the same JSON shape.
func synthetic(key string, v json.RawMessage) json.RawMessage {
	var decoded any
	if err := json.Unmarshal(v, &decoded); err != nil {
		return json.RawMessage("null")
	}
	var replaced any
	switch val := decoded.(type) {
	case nil, bool:
		replaced = val
	case float64:
		replaced = syntheticNumber(key, val)
	case string:
		replaced = syntheticString(key, val)
	case []any:
		if len(val) == 0 {
			replaced = []any{}
		} else {
			replaced = []any{map[string]any{"id": 1}}
		}
	case map[string]any:
		if _, isUser := val["username"]; isUser {
			replaced = map[string]any{"id": 1, "username": "synthetic-bot"}
		} else {
			replaced = map[string]any{}
		}
	}
	b, err := json.Marshal(replaced)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

func syntheticNumber(key string, val float64) float64 {
	switch key {
	case "id":
		return 101
	case "project_id":
		return 7
	case "bucket_id":
		return 3
	case "index":
		return 1
	case "cover_image_attachment_id":
		return 0
	default:
		// priority, repeat_after, repeat_mode, percent_done, position: the
		// value is structure, not board content.
		return val
	}
}

// zeroTime is how Vikunja spells an unset timestamp.
const zeroTime = "0001-01-01T00:00:00Z"

func syntheticString(key, val string) string {
	if _, err := time.Parse(time.RFC3339, val); err == nil {
		if val == zeroTime {
			return zeroTime
		}
		return "2026-01-02T03:04:05Z"
	}
	switch key {
	case "title":
		return "Synthetic task title"
	case "description":
		if strings.Contains(val, "closed-by: ") {
			return "Synthetic description.\n\nclosed-by: synthetic via forgectl tasks liveprobe 2026-01-02T03:04:05Z — synthetic evidence"
		}
		return "Synthetic description."
	case "identifier":
		return "SYN-1"
	case "hex_color":
		return ""
	default:
		return "synthetic"
	}
}
