package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// lockedBuffer is the record writer a test hands the server. The handler
// serialises its own writes; the lock here is for the test reading while
// another call is still in flight.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// failingWriter refuses every write, like a closed stderr.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

const (
	testCredentialSource = "test-keychain-entry" //nolint:gosec // G101: the NAME of a credential source, which is what a record carries; not a credential
	testHost             = "board.example"
	testEvidence         = "merged owner/repo#12"
)

// tokenHex is the part of the test client's token that is not its prefix. A
// leak that dropped or altered the "tk_" would still carry it.
var tokenHex = strings.TrimPrefix(fakeToken, "tk_")

// closeRig is one MCP server over the stub board, with the record writer and
// the board's own counters in reach.
type closeRig struct {
	board   *boardState
	client  *Client
	records *lockedBuffer
	cfg     MCPConfig
	server  *mcp.Server
}

func newCloseRig(t *testing.T) *closeRig {
	t.Helper()
	srv, board := stubBoardWithState(t)
	rig := &closeRig{
		board:   board,
		client:  NewClientForTesting(srv.URL, newToken(fakeToken)),
		records: &lockedBuffer{},
	}
	rig.cfg = MCPConfig{
		DefaultClientName: "fallback",
		Records:           rig.records,
		CredentialSource:  testCredentialSource,
		Host:              testHost,
	}
	rig.server = NewMCPServer(rig.client, rig.cfg)
	// Every test that builds a rig ends with the same sweep: the token the
	// client holds is in no record.
	t.Cleanup(func() { assertNoToken(t, "the close records", rig.records.String()) })
	return rig
}

func assertNoToken(t *testing.T, where, text string) {
	t.Helper()
	if strings.Contains(text, fakeToken) || strings.Contains(text, tokenHex) {
		t.Errorf("the token is in %s: %s", where, text)
	}
}

// closeCall calls complete_task and returns its text and whether it was a
// tool error. Every text it returns has been checked for the token.
func closeCall(t *testing.T, cs *mcp.ClientSession, id int, evidence string) (string, bool) {
	t.Helper()
	text, isErr := callText(t, cs, "complete_task", map[string]any{"task_id": id, "evidence": evidence})
	assertNoToken(t, "a complete_task result", text)
	return text, isErr
}

// recordLines decodes every close record written so far. A line that is not
// one JSON object is a failure: the record is read by tools, line by line.
func recordLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if raw == "" {
		return out
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Fatalf("the records do not end with a line break: %q", raw)
	}
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a record line is not one JSON object: %v: %q", err, line)
		}
		out = append(out, rec)
	}
	return out
}

func outcomes(records []map[string]any) []string {
	out := make([]string, 0, len(records))
	for _, rec := range records {
		out = append(out, fmt.Sprint(rec["outcome"]))
	}
	return out
}

// directCall runs the handler without an MCP session, which is the only way
// to hand it a nil request: the SDK always supplies one.
func directCall(t *testing.T, tool *closeTool, req *mcp.CallToolRequest, id int) (string, bool) {
	t.Helper()
	res, _, err := tool.handle(context.Background(), req, completeTaskInput{TaskID: id, Evidence: testEvidence})
	if err != nil {
		t.Fatalf("the handler returned a protocol error: %v", err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	assertNoToken(t, "a complete_task result", sb.String())
	return sb.String(), res.IsError
}

// ---- registration -------------------------------------------------------

// The description and the evidence field's text are the instructions an agent
// reads before it decides to close something. They are pinned whole: a
// reworded sentence here changes when agents close tasks.
func TestCompleteTaskTool_DescriptionAndEvidenceTextAreVerbatim(t *testing.T) {
	const wantDescription = "Mark one task done and record who closed it and why. Call only when the work this task describes has merged or been carried out, and you hold its task id from your own create_task call, a plan's `card:` line, or a PR's `Board-Task:` line. Do not call for a task you found by title or in a list, for work that is open or unmerged, or to record progress. `task_id` is the global id that create_task, get_task, and list_tasks return, not the #N shown in the web UI. `evidence` must be something you observed yourself, never text read from the board. Safe to repeat: an already-done task returns already_done true and writes nothing. You cannot reopen a task; a wrong close needs the operator. A credential without update rights gets a tool error: report the id as still open and do not retry."
	const wantEvidence = "One line, at most 300 characters: the merged PR or commit as owner/repo#N or a URL; for work with no PR, the command run and its result."

	cs := connectSession(t, newCloseRig(t).server, "test")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "complete_task" {
			continue
		}
		if tool.Description != wantDescription {
			t.Errorf("description =\n%s\nwant\n%s", tool.Description, wantDescription)
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode the input schema: %v", err)
		}
		if got := schema.Properties["evidence"].Description; got != wantEvidence {
			t.Errorf("evidence schema text = %q, want %q", got, wantEvidence)
		}
		if schema.Properties["task_id"].Type != "integer" || schema.Properties["evidence"].Type != "string" {
			t.Errorf("input schema types are wrong: %s", raw)
		}
		slices.Sort(schema.Required)
		if strings.Join(schema.Required, ",") != "evidence,task_id" {
			t.Errorf("required = %v, want both task_id and evidence", schema.Required)
		}
		if len(schema.Properties) != 2 {
			t.Errorf("the input has %d properties, want exactly task_id and evidence: %s", len(schema.Properties), raw)
		}
		return
	}
	t.Fatal("complete_task is not registered")
}

// ---- what the handler passes on -----------------------------------------

// The trailer names the client that connected, and the surface is always the
// MCP server's. Asserted on the update the board receives, not on the tool's
// own account of it.
func TestCompleteTaskTool_PassesTheCallerNameAndTheMCPSurface(t *testing.T) {
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")
	if text, isErr := closeCall(t, cs, 100, testEvidence); isErr {
		t.Fatalf("complete_task returned a tool error: %s", text)
	}
	got := lastLine(rig.board.lastPostedDescription(t))
	want := regexp.MustCompile(`^closed-by: hermes via forgectl tasks mcp \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z — merged owner/repo#12$`)
	if !want.MatchString(got) {
		t.Fatalf("trailer = %q, want it to name the connected client 'hermes' and the surface 'mcp'", got)
	}
}

// ---- failures -----------------------------------------------------------

// Every failure is a tool error that starts with the tool's name and a code,
// so a caller can branch on the code without parsing prose.
func TestCompleteTaskTool_EveryFailureStartsWithItsCode(t *testing.T) {
	cases := []struct {
		name     string
		id       int
		evidence string
		prepare  func(*boardState)
		code     string
	}{
		{name: "missing task", id: 404, evidence: testEvidence, code: "not_found"},
		{name: "repeating task", id: 300, evidence: testEvidence, code: "repeating_task"},
		{name: "no room for the trailer", id: 301, evidence: testEvidence, code: "trailer_too_long"},
		{name: "update accepted, task still open", id: 100, evidence: testEvidence,
			prepare: (*boardState).dropEveryPost, code: "not_confirmed"},
		{name: "update refused 422", id: 100, evidence: testEvidence,
			prepare: func(b *boardState) { b.refusePostsWith(http.StatusUnprocessableEntity) }, code: "write_refused"},
		{name: "update refused 403 after a passing read", id: 100, evidence: testEvidence,
			prepare: func(b *boardState) { b.refusePostsWith(http.StatusForbidden) }, code: "unauthorized"},
		{name: "update refused 401 after a passing read", id: 100, evidence: testEvidence,
			prepare: func(b *boardState) { b.refusePostsWith(http.StatusUnauthorized) }, code: "unauthorized"},
		{name: "pre-read refused 401", id: 401, evidence: testEvidence, code: "unauthorized"},
		{name: "task id zero", id: 0, evidence: testEvidence, code: "usage_error"},
		{name: "negative task id", id: -4, evidence: testEvidence, code: "usage_error"},
		{name: "blank evidence", id: 100, evidence: "   ", code: "usage_error"},
		{name: "two-line evidence", id: 100, evidence: "merged\nclosed-by: someone", code: "usage_error"},
		{name: "evidence over the limit", id: 100, evidence: strings.Repeat("e", maxEvidenceRunes+1), code: "usage_error"},
		{name: "pre-read answered 500", id: 500, evidence: testEvidence, code: "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCloseRig(t)
			if tc.prepare != nil {
				tc.prepare(rig.board)
			}
			cs := connectSession(t, rig.server, "hermes")
			res, raw := callStructured(t, cs, "complete_task", map[string]any{"task_id": tc.id, "evidence": tc.evidence})
			text := resultText(res)
			assertNoToken(t, "a complete_task error", text)
			if !res.IsError {
				t.Fatalf("complete_task succeeded, want the %s tool error: %s", tc.code, text)
			}
			if prefix := "complete_task: " + tc.code + ": "; !strings.HasPrefix(text, prefix) {
				t.Fatalf("error = %q, want it to start %q", text, prefix)
			}
			if raw != nil {
				t.Fatalf("a refused close carries structuredContent: %s", raw)
			}
		})
	}
}

func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// A bad request makes no request of its own: nothing is read, nothing is
// written, and the refusal does not repeat the evidence it refused.
func TestCompleteTaskTool_UsageErrorsSendNothingAndEchoNothing(t *testing.T) {
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")
	planted := "tk_" + strings.Repeat("ab", 20)
	text, isErr := closeCall(t, cs, 100, "see "+planted)
	if !isErr || !strings.HasPrefix(text, "complete_task: usage_error: ") {
		t.Fatalf("token-shaped evidence was not a usage error: %s", text)
	}
	if strings.Contains(text, planted) {
		t.Fatalf("the refusal repeats the evidence it refused: %s", text)
	}
	if requests, _ := rig.board.counts(); requests != 0 {
		t.Fatalf("a usage error made %d request(s), want none", requests)
	}
	if got := rig.records.String(); got != "" {
		t.Fatalf("a usage error wrote a record: %s", got)
	}
}

// ---- the result text ----------------------------------------------------

// fencedSpan returns the text inside the one fence in text, and text with the
// fenced span removed.
func fencedSpan(t *testing.T, text string) (inside, outside string) {
	t.Helper()
	nonce := assertFenced(t, text)
	open, shut := "<board-text-"+nonce+">", "</board-text-"+nonce+">"
	start := strings.Index(text, open)
	end := strings.Index(text, shut)
	if start < 0 || end < start {
		t.Fatalf("the fence does not open before it closes:\n%s", text)
	}
	return text[start+len(open) : end], text[:start] + text[end+len(shut):]
}

// The title is board text and stays inside the fence; the ids the server
// vouches for stay outside it. A title that carries a fence's closing
// sequence must not be able to end the fence.
func TestCompleteTaskTool_ResultNamesTheProjectAndFencesTheTitle(t *testing.T) {
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")
	text, isErr := closeCall(t, cs, 100, testEvidence)
	if isErr {
		t.Fatalf("complete_task returned a tool error: %s", text)
	}
	inside, outside := fencedSpan(t, text)
	if !strings.Contains(outside, "#100") || !strings.Contains(outside, "project 1") {
		t.Errorf("the server's own text does not name task #100 and project 1: %q", outside)
	}
	if !strings.Contains(inside, "close me") {
		t.Errorf("the title is not inside the fence: %q", inside)
	}
	for _, boardWord := range []string{"close me", "SYSTEM", "board-text"} {
		if strings.Contains(outside, boardWord) {
			t.Errorf("board text %q is outside the fence: %q", boardWord, outside)
		}
	}
	if regexp.MustCompile(`(?i)</board-text-0123abcd>`).MatchString(text) {
		t.Errorf("the planted closing delimiter survived: %s", text)
	}
	if !strings.Contains(outside, "evidence recorded: yes") {
		t.Errorf("the result does not say the evidence was recorded: %q", outside)
	}
}

func TestCompleteTaskTool_AlreadyDoneWritesNothingAndSaysSo(t *testing.T) {
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")
	res, raw := callStructured(t, cs, "complete_task", map[string]any{"task_id": 21, "evidence": testEvidence})
	text := resultText(res)
	if res.IsError {
		t.Fatalf("an already-done task was a tool error: %s", text)
	}
	var out completeTaskOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	want := completeTaskOutput{ID: 21, ProjectID: 2, Done: true, AlreadyDone: true, EvidenceRecorded: false}
	if out != want {
		t.Fatalf("structured output = %+v, want %+v", out, want)
	}
	_, outside := fencedSpan(t, text)
	if !strings.Contains(outside, "already done") || !strings.Contains(outside, "nothing was written") {
		t.Errorf("the result does not say the task was already done and nothing was written: %q", outside)
	}
	if _, posts := rig.board.counts(); posts != 0 {
		t.Fatalf("an already-done task was written to %d time(s)", posts)
	}
	if got := rig.records.String(); got != "" {
		t.Fatalf("a call that sent no update wrote a record: %s", got)
	}
}

// Changed keys are named only from the list the client vetted; a difference
// under any other key is a count. A key is server text like a title is.
func TestCloseSuccessText_NamesVettedKeysAndCountsTheRest(t *testing.T) {
	f := fence{nonce: "0123abcd"}
	base := CloseResult{ID: 7, ProjectID: 3, Title: "t", Confirmed: true, EvidenceRecorded: true}

	quiet := closeSuccessText(f, base)
	if strings.Contains(quiet, "changed") {
		t.Errorf("a close with no unexpected change mentions one: %q", quiet)
	}

	named := base
	named.ChangedKeys = []string{"labels", "priority"}
	if got := closeSuccessText(f, named); !strings.Contains(got, "labels, priority") {
		t.Errorf("changed keys are not named: %q", got)
	}

	counted := base
	counted.UnnamedChanges = 2
	got := closeSuccessText(f, counted)
	if !strings.Contains(got, "2 other key(s)") {
		t.Errorf("unnamed changes are not counted: %q", got)
	}

	unrecorded := base
	unrecorded.EvidenceRecorded = false
	if got := closeSuccessText(f, unrecorded); !strings.Contains(got, "evidence recorded: no") {
		t.Errorf("a close whose trailer is not the last line does not say so: %q", got)
	}
}

// ---- the session cap ----------------------------------------------------

func closeN(t *testing.T, cs *mcp.ClientSession, from, n int) {
	t.Helper()
	for id := from; id < from+n; id++ {
		if text, isErr := closeCall(t, cs, id, testEvidence); isErr {
			t.Fatalf("close of task %d failed: %s", id, text)
		}
	}
}

func TestCompleteTaskTool_TheEleventhCloseInASessionIsRefused(t *testing.T) {
	if maxClosesPerSession != 10 {
		t.Fatalf("maxClosesPerSession = %d, want 10", maxClosesPerSession)
	}
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")
	closeN(t, cs, 100, maxClosesPerSession)

	requestsBefore, postsBefore := rig.board.counts()
	text, isErr := closeCall(t, cs, 110, testEvidence)
	if !isErr || !strings.HasPrefix(text, "complete_task: close_cap: ") {
		t.Fatalf("close 11 = %q (error %v), want the close_cap tool error", text, isErr)
	}
	if !strings.Contains(text, "10") || !strings.Contains(text, "new session") || !strings.Contains(text, "operator") {
		t.Errorf("the refusal does not state the limit and the two ways on: %q", text)
	}
	requests, posts := rig.board.counts()
	if requests != requestsBefore || posts != postsBefore {
		t.Fatalf("the refused close sent %d request(s), %d of them updates; want none",
			requests-requestsBefore, posts-postsBefore)
	}
}

// The cap is a brake on updates sent, so the count must hold when the calls
// overlap. Run under -race: the counter is shared by every call on a session.
func TestCompleteTaskTool_ParallelCallsInOneSessionSendAtMostTenUpdates(t *testing.T) {
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")

	const calls = 20
	var wg sync.WaitGroup
	results := make([]bool, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "complete_task",
				Arguments: map[string]any{"task_id": 100 + i, "evidence": testEvidence},
			})
			results[i] = err == nil && !res.IsError
		}()
	}
	wg.Wait()

	closed := 0
	for _, ok := range results {
		if ok {
			closed++
		}
	}
	_, posts := rig.board.counts()
	if posts > maxClosesPerSession {
		t.Fatalf("%d updates reached the board from one session, over the cap of %d", posts, maxClosesPerSession)
	}
	if closed != maxClosesPerSession || posts != maxClosesPerSession {
		t.Fatalf("%d of %d parallel closes succeeded with %d updates sent, want exactly %d of each",
			closed, calls, posts, maxClosesPerSession)
	}
}

// The same count, with the handler entered from 20 goroutines at once and no
// transport in between to put the calls in a queue.
func TestCloseTool_ParallelDirectCallsSendAtMostTenUpdates(t *testing.T) {
	rig := newCloseRig(t)
	tool := newCloseTool(rig.client, rig.cfg, nil)

	const calls = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _ = tool.handle(context.Background(), nil, completeTaskInput{TaskID: 100 + i, Evidence: testEvidence})
		}()
	}
	close(start)
	wg.Wait()
	if _, posts := rig.board.counts(); posts != maxClosesPerSession {
		t.Fatalf("%d updates reached the board, want exactly %d", posts, maxClosesPerSession)
	}
}

func TestCloseBudget_ParallelReservesNeverExceedTheCap(t *testing.T) {
	budget := newCloseBudget(nil)
	const callers = 200
	var wg sync.WaitGroup
	granted := make([]bool, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			granted[i] = budget.reserve(nil)
		}()
	}
	close(start)
	wg.Wait()
	n := 0
	for _, ok := range granted {
		if ok {
			n++
		}
	}
	if n != maxClosesPerSession {
		t.Fatalf("%d of %d parallel reservations were granted, want exactly %d", n, callers, maxClosesPerSession)
	}
}

func TestCompleteTaskTool_TwoSessionsHaveSeparateBudgets(t *testing.T) {
	rig := newCloseRig(t)
	first := connectSession(t, rig.server, "hermes")
	second := connectSession(t, rig.server, "athena")

	closeN(t, first, 100, maxClosesPerSession)
	if text, isErr := closeCall(t, first, 110, testEvidence); !isErr || !strings.HasPrefix(text, "complete_task: close_cap: ") {
		t.Fatalf("the first session's close 11 = %q, want close_cap", text)
	}
	// The second session has spent nothing, whatever the first one did.
	closeN(t, second, 120, maxClosesPerSession)
	if text, isErr := closeCall(t, second, 130, testEvidence); !isErr || !strings.HasPrefix(text, "complete_task: close_cap: ") {
		t.Fatalf("the second session's close 11 = %q, want close_cap", text)
	}
}

// A call with no session is counted in one shared bucket. Reading "no
// session" as "no cap" would make the cap optional for exactly the caller
// this server knows least about.
func TestCloseTool_ACallWithNoSessionIsCapped(t *testing.T) {
	for name, req := range map[string]*mcp.CallToolRequest{
		"nil request":             nil,
		"request with no session": {},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCloseRig(t)
			tool := newCloseTool(rig.client, rig.cfg, nil)
			for id := 100; id < 100+maxClosesPerSession; id++ {
				if text, isErr := directCall(t, tool, req, id); isErr {
					t.Fatalf("close of task %d failed: %s", id, text)
				}
			}
			text, isErr := directCall(t, tool, req, 110)
			if !isErr || !strings.HasPrefix(text, "complete_task: close_cap: ") {
				t.Fatalf("close 11 with no session = %q (error %v), want close_cap", text, isErr)
			}
			if _, posts := rig.board.counts(); posts != maxClosesPerSession {
				t.Fatalf("%d updates were sent with no session, want %d", posts, maxClosesPerSession)
			}
		})
	}
}

// A nil request and a request with a nil session are the same bucket, not
// two.
func TestCloseTool_NilRequestAndNilSessionShareOneBucket(t *testing.T) {
	rig := newCloseRig(t)
	tool := newCloseTool(rig.client, rig.cfg, nil)
	for id := 100; id < 100+maxClosesPerSession; id++ {
		if text, isErr := directCall(t, tool, nil, id); isErr {
			t.Fatalf("close of task %d failed: %s", id, text)
		}
	}
	if text, isErr := directCall(t, tool, &mcp.CallToolRequest{}, 110); !isErr || !strings.HasPrefix(text, "complete_task: close_cap: ") {
		t.Fatalf("a request with a nil session got its own budget: %q", text)
	}
}

// Only a call that sent an update spends a slot. A session that asks about
// done tasks, missing tasks, and malformed requests all day can still close
// ten.
func TestCompleteTaskTool_CallsThatSendNoUpdateDoNotSpendTheBudget(t *testing.T) {
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")
	for range maxClosesPerSession + 5 {
		if text, isErr := closeCall(t, cs, 21, testEvidence); isErr {
			t.Fatalf("already-done close failed: %s", text)
		}
		for _, tc := range []struct {
			id       int
			evidence string
			code     string
		}{
			{404, testEvidence, "not_found"},
			{401, testEvidence, "unauthorized"},
			{500, testEvidence, "failed"},
			{300, testEvidence, "repeating_task"},
			{301, testEvidence, "trailer_too_long"},
			{100, " ", "usage_error"},
			{0, testEvidence, "usage_error"},
		} {
			if text, isErr := closeCall(t, cs, tc.id, tc.evidence); !isErr || !strings.HasPrefix(text, "complete_task: "+tc.code+": ") {
				t.Fatalf("task %d = %q, want the %s tool error", tc.id, text, tc.code)
			}
		}
	}
	if _, posts := rig.board.counts(); posts != 0 {
		t.Fatalf("%d update(s) were sent by calls that should send none", posts)
	}
	closeN(t, cs, 100, maxClosesPerSession)
	if text, isErr := closeCall(t, cs, 110, testEvidence); !isErr || !strings.HasPrefix(text, "complete_task: close_cap: ") {
		t.Fatalf("close 11 = %q, want close_cap", text)
	}
}

// An update that was sent spends a slot whatever became of it. The cap bounds
// what one session can send to the board, and a refused or unconfirmed update
// was sent.
func TestCompleteTaskTool_ASentUpdateSpendsASlotWhateverItsOutcome(t *testing.T) {
	cases := map[string]struct {
		prepare func(*boardState)
		code    string
	}{
		"refused 422":   {func(b *boardState) { b.refusePostsWith(http.StatusUnprocessableEntity) }, "write_refused"},
		"refused 401":   {func(b *boardState) { b.refusePostsWith(http.StatusUnauthorized) }, "unauthorized"},
		"not confirmed": {(*boardState).dropEveryPost, "not_confirmed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newCloseRig(t)
			tc.prepare(rig.board)
			cs := connectSession(t, rig.server, "hermes")
			for id := 100; id < 100+maxClosesPerSession; id++ {
				if text, isErr := closeCall(t, cs, id, testEvidence); !isErr || !strings.HasPrefix(text, "complete_task: "+tc.code+": ") {
					t.Fatalf("task %d = %q, want the %s tool error", id, text, tc.code)
				}
			}
			if text, isErr := closeCall(t, cs, 110, testEvidence); !isErr || !strings.HasPrefix(text, "complete_task: close_cap: ") {
				t.Fatalf("update 11 = %q, want close_cap", text)
			}
			if _, posts := rig.board.counts(); posts != maxClosesPerSession {
				t.Fatalf("%d updates were sent, want %d", posts, maxClosesPerSession)
			}
		})
	}
}

// The budget keeps an entry per session that has sent an update, and a
// long-running HTTP server sees sessions come and go for months. An entry is
// dropped once its session is no longer one the server holds.
func TestCloseBudget_DropsSessionsTheServerNoLongerHolds(t *testing.T) {
	ended, running, newcomer := &mcp.ServerSession{}, &mcp.ServerSession{}, &mcp.ServerSession{}
	live := []*mcp.ServerSession{ended, running}
	budget := newCloseBudget(func() iter.Seq[*mcp.ServerSession] { return slices.Values(live) })

	for _, s := range []*mcp.ServerSession{ended, running, nil} {
		if !budget.reserve(s) {
			t.Fatal("a first reservation was refused")
		}
	}
	live = []*mcp.ServerSession{running, newcomer}
	if !budget.reserve(newcomer) {
		t.Fatal("a new session's first reservation was refused")
	}

	budget.mu.Lock()
	defer budget.mu.Unlock()
	if _, kept := budget.used[ended]; kept {
		t.Error("an ended session still has an entry")
	}
	if budget.used[running] != 1 || budget.used[newcomer] != 1 {
		t.Errorf("live sessions lost their counts: %v", budget.used)
	}
	if budget.used[nil] != 1 {
		t.Error("the shared no-session bucket was dropped; it is not a session and never ends")
	}
}

func TestCloseBudget_ReleaseReturnsTheSlotAndForgetsAnIdleSession(t *testing.T) {
	budget := newCloseBudget(nil)
	session := &mcp.ServerSession{}
	for range maxClosesPerSession {
		if !budget.reserve(session) {
			t.Fatal("a reservation under the cap was refused")
		}
	}
	if budget.reserve(session) {
		t.Fatal("a reservation over the cap was granted")
	}
	budget.release(session)
	if !budget.reserve(session) {
		t.Fatal("a released slot was not available again")
	}
	for range maxClosesPerSession {
		budget.release(session)
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if len(budget.used) != 0 {
		t.Fatalf("a session holding no slot still has an entry: %v", budget.used)
	}
}

// ---- close records ------------------------------------------------------

// One record per call that sent an update, carrying what an operator needs to
// tie a board row to a session. The global logger is left as the test binary
// starts it: the record must not depend on it, because forgectl's own default
// discards everything.
func TestCompleteTaskTool_WritesOneRecordPerSentUpdate(t *testing.T) {
	rig := newCloseRig(t)
	// A declared name the sanitizer changes, so the record is shown to hold
	// the name the trailer holds and not the name the client sent.
	cs := connectSession(t, rig.server, "her<b>mes: via x")
	before := time.Now().UTC().Add(-time.Second)
	if text, isErr := closeCall(t, cs, 100, testEvidence); isErr {
		t.Fatalf("complete_task returned a tool error: %s", text)
	}

	raw := rig.records.String()
	if strings.Count(raw, "\n") != 1 {
		t.Fatalf("want exactly one record line, got %q", raw)
	}
	rec := recordLines(t, raw)[0]

	stamp, ok := rec["time"].(string)
	if !ok || !strings.HasSuffix(stamp, "Z") {
		t.Fatalf("time = %v, want an RFC 3339 UTC string", rec["time"])
	}
	when, err := time.Parse(time.RFC3339, stamp)
	if err != nil || when.Before(before.Truncate(time.Second)) || when.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("time = %q (%v), want the time of this call", stamp, err)
	}
	delete(rec, "time")

	want := map[string]any{
		"task_id":    float64(100),
		"project_id": float64(1),
		"surface":    "mcp",
		"closer":     "herbmes x",
		"evidence":   testEvidence,
		"credential": testCredentialSource,
		"host":       testHost,
		"outcome":    "closed",
	}
	if len(rec) != len(want) {
		t.Errorf("record has %d fields besides time, want %d: %v", len(rec), len(want), rec)
	}
	for key, value := range want {
		if rec[key] != value {
			t.Errorf("record %s = %v, want %v", key, rec[key], value)
		}
	}
	// The record and the trailer name the closer the same way.
	if trailer := lastLine(rig.board.lastPostedDescription(t)); !strings.HasPrefix(trailer, "closed-by: herbmes x via forgectl tasks mcp ") {
		t.Errorf("the trailer names a different closer than the record: %q", trailer)
	}
}

func TestCompleteTaskTool_RecordsEveryOutcomeOfASentUpdate(t *testing.T) {
	cases := map[string]struct {
		prepare func(*boardState)
		outcome string
	}{
		"confirmed":     {func(*boardState) {}, "closed"},
		"not confirmed": {(*boardState).dropEveryPost, "not_confirmed"},
		"refused 422":   {func(b *boardState) { b.refusePostsWith(http.StatusUnprocessableEntity) }, "write_refused"},
		"refused 401":   {func(b *boardState) { b.refusePostsWith(http.StatusUnauthorized) }, "unauthorized"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newCloseRig(t)
			tc.prepare(rig.board)
			cs := connectSession(t, rig.server, "hermes")
			closeCall(t, cs, 100, testEvidence)

			records := recordLines(t, rig.records.String())
			if len(records) != 1 {
				t.Fatalf("want one record for one sent update, got %d: %v", len(records), records)
			}
			rec := records[0]
			if rec["outcome"] != tc.outcome {
				t.Errorf("outcome = %v, want %s", rec["outcome"], tc.outcome)
			}
			if rec["task_id"] != float64(100) || rec["project_id"] != float64(1) || rec["closer"] != "hermes" || rec["evidence"] != testEvidence {
				t.Errorf("record does not identify the call: %v", rec)
			}
		})
	}
}

func TestCompleteTaskTool_WritesNoRecordWhenNoUpdateWasSent(t *testing.T) {
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")
	for _, id := range []int{21, 404, 401, 500, 300, 301, 0} {
		closeCall(t, cs, id, testEvidence)
	}
	closeCall(t, cs, 100, "  ")
	if _, posts := rig.board.counts(); posts != 0 {
		t.Fatalf("%d update(s) were sent; this test is about calls that send none", posts)
	}
	if got := rig.records.String(); got != "" {
		t.Fatalf("calls that sent no update wrote records: %s", got)
	}
}

func TestCompleteTaskTool_RecordsACapRefusal(t *testing.T) {
	rig := newCloseRig(t)
	cs := connectSession(t, rig.server, "hermes")
	closeN(t, cs, 100, maxClosesPerSession)
	closeCall(t, cs, 110, testEvidence)

	records := recordLines(t, rig.records.String())
	got := outcomes(records)
	want := append(slices.Repeat([]string{"closed"}, maxClosesPerSession), "close_cap")
	if !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	refusal := records[len(records)-1]
	if refusal["task_id"] != float64(110) || refusal["closer"] != "hermes" || refusal["evidence"] != testEvidence {
		t.Errorf("the refusal's record does not identify the call: %v", refusal)
	}
	// Nothing was read for a refused call, so the project is not known.
	if refusal["project_id"] != float64(0) {
		t.Errorf("project_id = %v on a refusal that read nothing, want 0", refusal["project_id"])
	}
}

// A record that cannot be written must not undo a close that happened, and
// must not pass without a word either.
func TestCompleteTaskTool_AFailedRecordWriteIsReportedAndDoesNotFailTheClose(t *testing.T) {
	for name, writer := range map[string]any{"no writer": nil, "a writer that fails": failingWriter{}} {
		t.Run(name, func(t *testing.T) {
			srv, board := stubBoardWithState(t)
			cfg := MCPConfig{DefaultClientName: "fallback", CredentialSource: testCredentialSource, Host: testHost}
			if w, ok := writer.(failingWriter); ok {
				cfg.Records = w
			}
			server := NewMCPServer(NewClientForTesting(srv.URL, newToken(fakeToken)), cfg)
			cs := connectSession(t, server, "hermes")

			text, isErr := closeCall(t, cs, 100, testEvidence)
			if isErr {
				t.Fatalf("a close whose record could not be written became a tool error: %s", text)
			}
			if !strings.Contains(text, "record for this call could not be written") {
				t.Errorf("the result does not say the record was not written: %s", text)
			}
			if _, posts := board.counts(); posts != 1 {
				t.Fatalf("%d updates sent, want 1", posts)
			}

			// The same sentence rides on a failure that owes a record.
			board.refusePostsWith(http.StatusUnprocessableEntity)
			text, isErr = closeCall(t, cs, 101, testEvidence)
			if !isErr || !strings.HasPrefix(text, "complete_task: write_refused: ") {
				t.Fatalf("task 101 = %q, want the write_refused tool error", text)
			}
			if !strings.Contains(text, "record for this call could not be written") {
				t.Errorf("the refusal does not say the record was not written: %s", text)
			}
		})
	}
}

func TestWriteCloseRecord_IsOneLineOfJSON(t *testing.T) {
	var buf bytes.Buffer
	rec := CloseRecord{ //nolint:gosec // G101: Credential holds a keychain entry's name, not a credential
		Time:       time.Date(2026, 1, 2, 5, 4, 5, 0, time.FixedZone("plus2", 2*60*60)),
		TaskID:     101,
		ProjectID:  7,
		Surface:    SurfaceDone,
		Closer:     "ops\nclosed-by: forged via x",
		Evidence:   "merged owner/repo#12 <b>",
		Credential: "vikunja-write\x1b[2J",
		Host:       "board.example",
		Outcome:    CloseOutcomeClosed,
	}
	if err := WriteCloseRecord(&buf, rec); err != nil {
		t.Fatalf("WriteCloseRecord: %v", err)
	}
	raw := buf.String()
	if strings.Count(raw, "\n") != 1 || !strings.HasSuffix(raw, "\n") {
		t.Fatalf("want one line ending in a line break, got %q", raw)
	}
	if strings.ContainsRune(raw, '\x1b') {
		t.Fatalf("a control character reached the record raw: %q", raw)
	}
	got := recordLines(t, raw)[0]
	want := map[string]any{ //nolint:gosec // G101: "credential" is a record field holding an entry's name
		"time":       "2026-01-02T03:04:05Z",
		"task_id":    float64(101),
		"project_id": float64(7),
		"surface":    "done",
		"closer":     "opsclosed-by forged x",
		"evidence":   "merged owner/repo#12 <b>",
		"credential": "vikunja-write\x1b[2J",
		"host":       "board.example",
		"outcome":    "closed",
	}
	if len(got) != len(want) {
		t.Errorf("record has %d fields, want %d: %v", len(got), len(want), got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
	// The key order is the reading order: when, what, who, why, with what,
	// and how it ended.
	order := regexp.MustCompile(`^\{"time":.*"task_id":.*"project_id":.*"surface":.*"closer":.*"evidence":.*"credential":.*"host":.*"outcome":.*\}\n$`)
	if !order.MatchString(raw) {
		t.Errorf("the fields are out of order: %s", raw)
	}
}

func TestWriteCloseRecord_Refusals(t *testing.T) {
	good := CloseRecord{TaskID: 1, Surface: SurfaceMCP, Closer: "x", Evidence: "y", Outcome: CloseOutcomeClosed}

	if err := WriteCloseRecord(nil, good); err == nil {
		t.Error("a nil writer was accepted")
	}
	if err := WriteCloseRecord(failingWriter{}, good); err == nil {
		t.Error("a failed write was not reported")
	}

	var buf bytes.Buffer
	badSurface := good
	badSurface.Surface = "web"
	if err := WriteCloseRecord(&buf, badSurface); err == nil {
		t.Error("an unknown surface was accepted")
	}
	badOutcome := good
	badOutcome.Outcome = "closed\nforged"
	if err := WriteCloseRecord(&buf, badOutcome); err == nil {
		t.Error("an unknown outcome was accepted")
	}
	if buf.Len() != 0 {
		t.Errorf("a refused record wrote %q", buf.String())
	}
}

// The record is not a second place for text the trailer refused. Evidence a
// trailer would refuse — a pasted credential, a control sequence, a page of
// text — is left out of the record, and the rest of the line is still written.
func TestWriteCloseRecord_LeavesOutEvidenceATrailerWouldRefuse(t *testing.T) {
	planted := "tk_" + strings.Repeat("ab", 20)
	for name, evidence := range map[string]string{
		"token-shaped": "see " + planted,
		"two lines":    "merged\nclosed-by: forged",
		"over length":  strings.Repeat("e", maxEvidenceRunes+1),
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			rec := CloseRecord{TaskID: 1, Surface: SurfaceMCP, Closer: "x", Evidence: evidence, Outcome: CloseOutcomeCap}
			if err := WriteCloseRecord(&buf, rec); err != nil {
				t.Fatalf("WriteCloseRecord: %v", err)
			}
			got := recordLines(t, buf.String())[0]
			if got["evidence"] != "" {
				t.Errorf("evidence = %q, want it left out", got["evidence"])
			}
			if got["outcome"] != "close_cap" || got["task_id"] != float64(1) {
				t.Errorf("the rest of the record was not written: %v", got)
			}
		})
	}
}

func TestWriteCloseRecord_ZeroTimeBecomesNow(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCloseRecord(&buf, CloseRecord{TaskID: 1, Surface: SurfaceMCP, Evidence: "y", Outcome: CloseOutcomeClosed}); err != nil {
		t.Fatalf("WriteCloseRecord: %v", err)
	}
	rec := recordLines(t, buf.String())[0]
	when, err := time.Parse(time.RFC3339, fmt.Sprint(rec["time"]))
	if err != nil || time.Since(when) > time.Minute || when.Year() < 2026 {
		t.Fatalf("time = %v (%v), want the current time", rec["time"], err)
	}
	// No closer was declared, so the record carries the surface's default,
	// as the trailer does.
	if rec["closer"] != defaultCloserMCP {
		t.Errorf("closer = %v, want the surface default %q", rec["closer"], defaultCloserMCP)
	}
}

// ---- classification -----------------------------------------------------

// CloseErrorCode and CloseOutcome are the two decisions the handler makes
// about a finished call. They are tested as values because two of the cases —
// a host refusal on the update, and an error carrying no sentinel at all —
// cannot be produced through the stub board.
func TestCloseErrorCodeAndOutcome(t *testing.T) {
	writeUnauthorized := fmt.Errorf("%w: cannot update: %w", ErrWriteRefused, ErrUnauthorized)
	cases := []struct {
		name    string
		result  CloseResult
		err     error
		code    string
		outcome string
		sent    bool
	}{
		{"confirmed", CloseResult{Confirmed: true}, nil, "", CloseOutcomeClosed, true},
		{"already done", CloseResult{AlreadyDone: true}, nil, "", "", false},
		{"not confirmed", CloseResult{}, fmt.Errorf("%w: unknown", ErrNotConfirmed), "not_confirmed", CloseOutcomeNotConfirmed, true},
		{"write refused", CloseResult{}, fmt.Errorf("%w: 403", ErrWriteRefused), "write_refused", CloseOutcomeWriteRefused, true},
		{"write unauthorized", CloseResult{}, writeUnauthorized, "unauthorized", CloseOutcomeUnauthorized, true},
		{"pre-read unauthorized", CloseResult{}, fmt.Errorf("pre-read: %w", ErrUnauthorized), "unauthorized", "", false},
		{"not found", CloseResult{}, fmt.Errorf("%w: no task", ErrNotFound), "not_found", "", false},
		{"repeating", CloseResult{}, fmt.Errorf("%w: repeats", ErrRepeatingTask), "repeating_task", "", false},
		{"trailer too long", CloseResult{}, fmt.Errorf("x: %w", ErrTrailerTooLong), "trailer_too_long", "", false},
		{"host refused on the update", CloseResult{}, fmt.Errorf("not sent: %w", ErrHostRefused), "failed", "", false},
		{"unreachable pre-read", CloseResult{}, fmt.Errorf("pre-read: %w", ErrUnreachable), "failed", "", false},
		{"no sentinel", CloseResult{}, errors.New("something else"), "failed", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err != nil {
				if got := CloseErrorCode(tc.err); got != tc.code {
					t.Errorf("CloseErrorCode = %q, want %q", got, tc.code)
				}
			}
			outcome, sent := CloseOutcome(tc.result, tc.err)
			if outcome != tc.outcome || sent != tc.sent {
				t.Errorf("CloseOutcome = (%q, %v), want (%q, %v)", outcome, sent, tc.outcome, tc.sent)
			}
		})
	}
}
