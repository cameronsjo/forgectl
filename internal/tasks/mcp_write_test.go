package tasks

// Test plan for the create_task and add_comment write records and their cap
//
//   [x] One record line per write, sent or refused at the board, with the
//       outcome the board's answer earns: written, unauthorized,
//       write_refused, not_confirmed
//   [x] A record is exactly its keys, names the caller as sanitized, and
//       carries no title, description, comment text, or token
//   [x] No record for a call that sent nothing: a local refusal, a failed
//       pre-read
//   [x] The record does not depend on the logger
//   [x] A record that cannot be written leaves a note on the result
//   [x] The cap: shared by both tools, per session, refused calls answer
//       write_cap and leave a cap record, parallel calls send at most the
//       cap, a nil session is one capped bucket, a failed pre-read spends
//       nothing, a write the board refused spends a slot

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Planted strings: caller text and board text a record has no business
// carrying. Each is distinct so a failure names which one leaked.
const (
	plantedTitle       = "PLANTED-TITLE-7f3a"
	plantedDescription = "PLANTED-DESCRIPTION-91c2"
	plantedComment     = "PLANTED-COMMENT-4be0"
	plantedBoardTitle  = "PLANTED-BOARD-TITLE-d81e"
)

// writeBoard is a stub Vikunja for the two create tools. Project and task 7
// are unreadable (404); every other id reads back, in project 3. putStatus,
// when non-zero, is the status every write is answered with; garbleWrites
// answers a write 201 with a body that is not JSON.
type writeBoard struct {
	mu           sync.Mutex
	puts         int
	putStatus    int
	garbleWrites bool
}

var (
	writeProjectPath = regexp.MustCompile(`^/projects/(\d+)$`)
	writeCreatePath  = regexp.MustCompile(`^/projects/(\d+)/tasks$`)
	writeTaskPath    = regexp.MustCompile(`^/tasks/(\d+)$`)
	writeCommentPath = regexp.MustCompile(`^/tasks/(\d+)/comments$`)
)

func (b *writeBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	id := func(re *regexp.Regexp) (int, bool) {
		m := re.FindStringSubmatch(r.URL.Path)
		if m == nil {
			return 0, false
		}
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	if r.Method == http.MethodGet {
		if n, ok := id(writeProjectPath); ok && n != 7 {
			_, _ = fmt.Fprintf(w, `{"id":%d,"title":%q}`, n, plantedBoardTitle)
			return
		}
		if n, ok := id(writeTaskPath); ok && n != 7 {
			_, _ = fmt.Fprintf(w, `{"id":%d,"title":%q,"project_id":3}`, n, plantedBoardTitle)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"not found"}`))
		return
	}
	_, isCreate := id(writeCreatePath)
	_, isComment := id(writeCommentPath)
	if r.Method != http.MethodPut || (!isCreate && !isComment) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	b.mu.Lock()
	b.puts++
	n, status, garble := b.puts, b.putStatus, b.garbleWrites
	b.mu.Unlock()
	switch {
	case status != 0:
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"refused"}`))
	case garble:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`<html>not json</html>`))
	case isCreate:
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":%d,"title":%q,"project_id":1}`, 900+n, plantedBoardTitle)
	default:
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":%d,"comment":"noted"}`, n)
	}
}

func (b *writeBoard) putCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.puts
}

func (b *writeBoard) set(status int, garble bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.putStatus, b.garbleWrites = status, garble
}

type writeRig struct {
	board   *writeBoard
	client  *Client
	records *lockedBuffer
	cfg     MCPConfig
	server  *mcp.Server
}

func newWriteRig(t *testing.T) *writeRig {
	t.Helper()
	board := &writeBoard{}
	srv := httptest.NewServer(board)
	t.Cleanup(srv.Close)
	rig := &writeRig{
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
	t.Cleanup(func() { assertNoPlantedText(t, "the write records", rig.records.String()) })
	return rig
}

func assertNoPlantedText(t *testing.T, where, text string) {
	t.Helper()
	assertNoToken(t, where, text)
	for _, planted := range []string{plantedTitle, plantedDescription, plantedComment, plantedBoardTitle} {
		if strings.Contains(text, planted) {
			t.Errorf("%s carry %s: %s", where, planted, text)
		}
	}
}

func createArgs(project int) map[string]any {
	return map[string]any{"project_id": project, "title": plantedTitle, "description": plantedDescription}
}

func commentArgs(task int) map[string]any {
	return map[string]any{"task_id": task, "body": plantedComment}
}

func writeCall(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	text, isErr := callText(t, cs, tool, args)
	assertNoToken(t, "a "+tool+" result", text)
	return text, isErr
}

var writeRecordKeys = []string{"caller", "credential", "event", "host", "outcome", "project_id", "task_id", "time", "tool"}

func checkWriteRecord(t *testing.T, rec map[string]any, want map[string]any) {
	t.Helper()
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, writeRecordKeys) {
		t.Errorf("record keys = %v, want %v", keys, writeRecordKeys)
	}
	for k, v := range want {
		if fmt.Sprint(rec[k]) != fmt.Sprint(v) {
			t.Errorf("record %s = %v, want %v (record %v)", k, rec[k], v, rec)
		}
	}
	if _, err := strconv.Atoi(fmt.Sprint(rec["task_id"])); err != nil {
		t.Errorf("record task_id = %v, want a number", rec["task_id"])
	}
}

func TestCreateTask_WritesOneRecordPerWrite(t *testing.T) {
	rig := newWriteRig(t)
	cs := connectSession(t, rig.server, "hermes")
	text, isErr := writeCall(t, cs, "create_task", createArgs(1))
	if isErr {
		t.Fatalf("create_task: %s", text)
	}
	records := recordLines(t, rig.records.String())
	if len(records) != 1 {
		t.Fatalf("%d record lines after one create, want 1: %q", len(records), rig.records.String())
	}
	checkWriteRecord(t, records[0], map[string]any{
		"event": BoardWriteEventTaskCreated, "tool": "create_task", "task_id": 901, "project_id": 1,
		"caller": "hermes", "credential": testCredentialSource, "host": testHost, "outcome": BoardWriteOutcomeWritten,
	})
	if _, isEvent := records[0]["event"]; !isEvent {
		t.Error("the record has no event key, so a close-log reader cannot tell it from a close record")
	}
}

func TestAddComment_WritesOneRecordPerWrite(t *testing.T) {
	rig := newWriteRig(t)
	cs := connectSession(t, rig.server, "hermes")
	if text, isErr := writeCall(t, cs, "add_comment", commentArgs(42)); isErr {
		t.Fatalf("add_comment: %s", text)
	}
	records := recordLines(t, rig.records.String())
	if len(records) != 1 {
		t.Fatalf("%d record lines after one comment, want 1", len(records))
	}
	// The project is the one the pre-read found the task in.
	checkWriteRecord(t, records[0], map[string]any{
		"event": BoardWriteEventCommentAdded, "tool": "add_comment", "task_id": 42, "project_id": 3,
		"caller": "hermes", "credential": testCredentialSource, "host": testHost, "outcome": BoardWriteOutcomeWritten,
	})
}

// TestBoardWrites_EveryWriteTheBoardAnsweredIsRecorded: a write the board
// refused was still sent, and one whose answer could not be read may have
// landed. Each leaves one line, and the outcome says which.
func TestBoardWrites_EveryWriteTheBoardAnsweredIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		garble  bool
		outcome string
	}{
		{"401", http.StatusUnauthorized, false, BoardWriteOutcomeUnauthorized},
		{"403", http.StatusForbidden, false, BoardWriteOutcomeUnauthorized},
		{"500", http.StatusInternalServerError, false, BoardWriteOutcomeWriteRefused},
		{"422", http.StatusUnprocessableEntity, false, BoardWriteOutcomeWriteRefused},
		{"an answer that is not JSON", 0, true, BoardWriteOutcomeNotConfirmed},
	} {
		for _, call := range []struct {
			tool  string
			args  map[string]any
			event string
		}{
			{"create_task", createArgs(1), BoardWriteEventTaskCreated},
			{"add_comment", commentArgs(42), BoardWriteEventCommentAdded},
		} {
			t.Run(tc.name+"/"+call.tool, func(t *testing.T) {
				rig := newWriteRig(t)
				rig.board.set(tc.status, tc.garble)
				cs := connectSession(t, rig.server, "hermes")
				if text, isErr := writeCall(t, cs, call.tool, call.args); !isErr {
					t.Fatalf("%s against a refusing board succeeded: %s", call.tool, text)
				}
				records := recordLines(t, rig.records.String())
				if len(records) != 1 {
					t.Fatalf("%d record lines, want 1", len(records))
				}
				checkWriteRecord(t, records[0], map[string]any{"event": call.event, "tool": call.tool, "outcome": tc.outcome})
				if call.tool == "create_task" && fmt.Sprint(records[0]["task_id"]) != "0" {
					t.Errorf("task_id = %v for a create with no confirmed id, want 0", records[0]["task_id"])
				}
			})
		}
	}
}

// TestBoardWrites_NoRecordWhenNothingWasSent: a call refused locally, or
// stopped by its pre-read, sent no write and has nothing to record.
func TestBoardWrites_NoRecordWhenNothingWasSent(t *testing.T) {
	rig := newWriteRig(t)
	cs := connectSession(t, rig.server, "hermes")
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"create_task", map[string]any{"project_id": 1, "title": "  "}},
		{"create_task", map[string]any{"project_id": 1, "title": strings.Repeat("t", maxTitleRunes+1)}},
		{"create_task", createArgs(7)},
		{"create_task", createArgs(0)},
		{"add_comment", map[string]any{"task_id": 42, "body": " "}},
		{"add_comment", map[string]any{"task_id": 42, "body": strings.Repeat("c", maxCommentRunes+1)}},
		{"add_comment", commentArgs(7)},
	} {
		if text, isErr := writeCall(t, cs, call.tool, call.args); !isErr {
			t.Fatalf("%s %v succeeded, want a refusal: %s", call.tool, call.args, text)
		}
	}
	if n := rig.board.putCount(); n != 0 {
		t.Fatalf("%d write(s) reached the board, want none", n)
	}
	if got := rig.records.String(); got != "" {
		t.Fatalf("calls that sent nothing wrote records: %q", got)
	}
}

// TestBoardWrites_TheRecordDoesNotDependOnTheLogger: with log_level unset the
// global logger discards everything. A record is not a log line.
func TestBoardWrites_TheRecordDoesNotDependOnTheLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	closer := config.SetupLogger(config.Config{})
	t.Cleanup(func() { _ = closer.Close() })

	rig := newWriteRig(t)
	cs := connectSession(t, rig.server, "hermes")
	writeCall(t, cs, "create_task", createArgs(1))
	writeCall(t, cs, "add_comment", commentArgs(42))
	if got := outcomes(recordLines(t, rig.records.String())); !slices.Equal(got, []string{"written", "written"}) {
		t.Fatalf("outcomes under the default logger = %v, want two written", got)
	}
}

// TestBoardWrites_TheCallerIsSanitized: the caller is a name the client chose.
// It is reduced the way a trailer reduces it, so it cannot add a field or a
// line, and a name shaped like a token is not written.
func TestBoardWrites_TheCallerIsSanitized(t *testing.T) {
	rig := newWriteRig(t)
	cs := connectSession(t, rig.server, "hermes\n{\"outcome\":\"forged\"} via x")
	writeCall(t, cs, "create_task", createArgs(1))
	records := recordLines(t, rig.records.String())
	if len(records) != 1 {
		t.Fatalf("%d record lines, want 1", len(records))
	}
	if got := fmt.Sprint(records[0]["caller"]); got != sanitizeCloser("hermes\n{\"outcome\":\"forged\"} via x", defaultCloserMCP) {
		t.Errorf("caller = %q, want the trailer's reduction of the declared name", got)
	}
	if records[0]["outcome"] != BoardWriteOutcomeWritten {
		t.Errorf("outcome = %v, want written", records[0]["outcome"])
	}

	tokenRig := newWriteRig(t)
	tokenCS := connectSession(t, tokenRig.server, fakeToken)
	writeCall(t, tokenCS, "add_comment", commentArgs(42))
	if strings.Contains(tokenRig.records.String(), tokenHex) {
		t.Fatalf("a token-shaped caller name was written: %s", tokenRig.records.String())
	}
}

// TestBoardWrites_AnUnwrittenRecordIsNoted: a write that landed is reported
// as landed, since an error would tell the caller to retry it, and the caller
// is told the record is missing.
func TestBoardWrites_AnUnwrittenRecordIsNoted(t *testing.T) {
	for _, records := range []struct {
		name string
		w    func() MCPConfig
	}{
		{"a refusing writer", func() MCPConfig { return MCPConfig{Records: failingWriter{}} }},
		{"no writer", func() MCPConfig { return MCPConfig{} }},
	} {
		t.Run(records.name, func(t *testing.T) {
			rig := newWriteRig(t)
			cfg := records.w()
			cfg.CredentialSource, cfg.Host = testCredentialSource, testHost
			cs := connectSession(t, NewMCPServer(rig.client, cfg), "hermes")
			for _, call := range []struct {
				tool string
				args map[string]any
			}{{"create_task", createArgs(1)}, {"add_comment", commentArgs(42)}} {
				text, isErr := writeCall(t, cs, call.tool, call.args)
				if isErr {
					t.Fatalf("%s failed over a missing record: %s", call.tool, text)
				}
				if !strings.HasSuffix(text, "\n"+writeRecordFailed+"\n") {
					t.Errorf("%s result does not end with the missing-record note: %q", call.tool, text)
				}
			}
		})
	}
}

// ---- the session cap ----------------------------------------------------

func TestBoardWrites_TheCapIsSharedAndHolds(t *testing.T) {
	if maxBoardWritesPerSession != 20 {
		t.Fatalf("maxBoardWritesPerSession = %d, want 20", maxBoardWritesPerSession)
	}
	rig := newWriteRig(t)
	cs := connectSession(t, rig.server, "hermes")
	for i := range maxBoardWritesPerSession {
		tool, args := "create_task", createArgs(1)
		if i%2 == 1 {
			tool, args = "add_comment", commentArgs(40+i)
		}
		if text, isErr := writeCall(t, cs, tool, args); isErr {
			t.Fatalf("write %d (%s) failed: %s", i+1, tool, text)
		}
	}
	for _, call := range []struct {
		tool string
		args map[string]any
	}{{"create_task", createArgs(1)}, {"add_comment", commentArgs(42)}} {
		text, isErr := writeCall(t, cs, call.tool, call.args)
		if !isErr || !strings.HasPrefix(text, call.tool+": write_cap: ") {
			t.Fatalf("%s past the cap = %q (error %v), want the write_cap tool error", call.tool, text, isErr)
		}
		if !strings.Contains(text, "new session") || !strings.Contains(text, "operator") {
			t.Errorf("the refusal does not say how to go on: %q", text)
		}
	}
	if n := rig.board.putCount(); n != maxBoardWritesPerSession {
		t.Fatalf("%d writes reached the board, want exactly %d", n, maxBoardWritesPerSession)
	}
	records := recordLines(t, rig.records.String())
	want := append(slices.Repeat([]string{"written"}, maxBoardWritesPerSession), "cap", "cap")
	if got := outcomes(records); !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	last := records[len(records)-1]
	checkWriteRecord(t, last, map[string]any{"tool": "add_comment", "event": BoardWriteEventCommentAdded, "task_id": 42, "project_id": 0})
}

func TestBoardWrites_TwoSessionsHaveSeparateBudgets(t *testing.T) {
	rig := newWriteRig(t)
	first := connectSession(t, rig.server, "hermes")
	second := connectSession(t, rig.server, "athena")
	for range maxBoardWritesPerSession {
		writeCall(t, first, "add_comment", commentArgs(42))
	}
	if text, isErr := writeCall(t, first, "add_comment", commentArgs(42)); !isErr || !strings.HasPrefix(text, "add_comment: write_cap: ") {
		t.Fatalf("the first session past its cap = %q, want write_cap", text)
	}
	if text, isErr := writeCall(t, second, "add_comment", commentArgs(42)); isErr {
		t.Fatalf("the second session was refused for the first one's writes: %s", text)
	}
}

// Run under -race: the count is shared by every call on a session, from both
// tools at once.
func TestBoardWrites_ParallelCallsInOneSessionSendAtMostTheCap(t *testing.T) {
	rig := newWriteRig(t)
	cs := connectSession(t, rig.server, "hermes")
	const calls = 3 * maxBoardWritesPerSession
	var wg sync.WaitGroup
	results := make([]bool, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, args := "create_task", createArgs(1)
			if i%2 == 1 {
				name, args = "add_comment", commentArgs(42)
			}
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
			results[i] = err == nil && !res.IsError
		}()
	}
	wg.Wait()
	written := 0
	for _, ok := range results {
		if ok {
			written++
		}
	}
	if n := rig.board.putCount(); n != maxBoardWritesPerSession || written != maxBoardWritesPerSession {
		t.Fatalf("%d of %d parallel writes succeeded with %d sent, want exactly %d of each",
			written, calls, n, maxBoardWritesPerSession)
	}
}

// The same count with the handlers entered directly from many goroutines and
// a nil request, so the calls share the no-session bucket and no transport
// puts them in a queue.
func TestBoardWriteTool_ParallelDirectCallsWithNoSessionSendAtMostTheCap(t *testing.T) {
	rig := newWriteRig(t)
	tool := newBoardWriteTool(rig.client, rig.cfg, nil)
	const calls = 3 * maxBoardWritesPerSession
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				_, _, _ = tool.createTask(context.Background(), nil, createTaskInput{ProjectID: 1, Title: plantedTitle})
				return
			}
			_, _, _ = tool.addComment(context.Background(), nil, addCommentInput{TaskID: 42, Body: plantedComment})
		}()
	}
	close(start)
	wg.Wait()
	if n := rig.board.putCount(); n != maxBoardWritesPerSession {
		t.Fatalf("%d writes reached the board, want exactly %d", n, maxBoardWritesPerSession)
	}
}

// A call with no session is counted in one shared bucket: "no session" is
// not "no cap".
func TestBoardWriteTool_ACallWithNoSessionIsCapped(t *testing.T) {
	for name, req := range map[string]*mcp.CallToolRequest{
		"nil request":             nil,
		"request with no session": {},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newWriteRig(t)
			tool := newBoardWriteTool(rig.client, rig.cfg, nil)
			for range maxBoardWritesPerSession {
				res, _, err := tool.addComment(context.Background(), req, addCommentInput{TaskID: 42, Body: plantedComment})
				if err != nil || res.IsError {
					t.Fatalf("comment failed: %v %s", err, resultText(res))
				}
			}
			res, _, err := tool.createTask(context.Background(), req, createTaskInput{ProjectID: 1, Title: plantedTitle})
			if err != nil || !res.IsError || !strings.HasPrefix(resultText(res), "create_task: write_cap: ") {
				t.Fatalf("write past the cap with no session = %q, want write_cap", resultText(res))
			}
			if n := rig.board.putCount(); n != maxBoardWritesPerSession {
				t.Fatalf("%d writes were sent with no session, want %d", n, maxBoardWritesPerSession)
			}
		})
	}
}

// A failed pre-read sends no write and spends no slot. A session whose reads
// fail all day can still write its full allowance.
func TestBoardWrites_AFailedPreReadSpendsNothing(t *testing.T) {
	rig := newWriteRig(t)
	cs := connectSession(t, rig.server, "hermes")
	for range maxBoardWritesPerSession + 5 {
		if _, isErr := writeCall(t, cs, "create_task", createArgs(7)); !isErr {
			t.Fatal("create_task into an unreadable project succeeded")
		}
		if _, isErr := writeCall(t, cs, "add_comment", commentArgs(7)); !isErr {
			t.Fatal("add_comment on an unreadable task succeeded")
		}
		if _, isErr := writeCall(t, cs, "create_task", map[string]any{"project_id": 1, "title": " "}); !isErr {
			t.Fatal("create_task with a blank title succeeded")
		}
	}
	for i := range maxBoardWritesPerSession {
		if text, isErr := writeCall(t, cs, "add_comment", commentArgs(42)); isErr {
			t.Fatalf("write %d after failed pre-reads was refused: %s", i+1, text)
		}
	}
	if text, isErr := writeCall(t, cs, "add_comment", commentArgs(42)); !isErr || !strings.HasPrefix(text, "add_comment: write_cap: ") {
		t.Fatalf("write past the cap = %q, want write_cap", text)
	}
}

// A write the board refused was sent, so it spends a slot: the cap bounds
// what a session sends, not what the board accepts.
func TestBoardWrites_ARefusedWriteSpendsASlot(t *testing.T) {
	rig := newWriteRig(t)
	rig.board.set(http.StatusInternalServerError, false)
	cs := connectSession(t, rig.server, "hermes")
	for range maxBoardWritesPerSession {
		writeCall(t, cs, "create_task", createArgs(1))
	}
	if text, isErr := writeCall(t, cs, "create_task", createArgs(1)); !isErr || !strings.HasPrefix(text, "create_task: write_cap: ") {
		t.Fatalf("write past the cap = %q, want write_cap", text)
	}
	if n := rig.board.putCount(); n != maxBoardWritesPerSession {
		t.Fatalf("%d writes reached the board, want %d", n, maxBoardWritesPerSession)
	}
}

// TestWriteBoardWriteRecord_RefusesWhatItCannotName: an unknown event or
// outcome is a bug in the caller, and a line carrying one would be read as
// something it is not.
func TestWriteBoardWriteRecord_RefusesWhatItCannotName(t *testing.T) {
	var buf strings.Builder
	for _, rec := range []BoardWriteRecord{
		{Event: "task_deleted", Outcome: BoardWriteOutcomeWritten},
		{Event: BoardWriteEventTaskCreated, Outcome: "closed"},
	} {
		if err := WriteBoardWriteRecord(&buf, rec); err == nil {
			t.Errorf("WriteBoardWriteRecord(%+v) = nil, want a refusal", rec)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("a refused record wrote %q", buf.String())
	}
	if err := WriteBoardWriteRecord(nil, BoardWriteRecord{Event: BoardWriteEventTaskCreated, Outcome: BoardWriteOutcomeWritten}); err == nil {
		t.Error("WriteBoardWriteRecord(nil writer) = nil, want an error")
	}
}

// TestBoardWriteOutcome: only a host refusal is known to have sent nothing.
// Every other failure on the request path may have reached the board, so it
// is recorded and spends its slot.
func TestBoardWriteOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome string
		sent    bool
	}{
		{"success", nil, BoardWriteOutcomeWritten, true},
		{"host refused", fmt.Errorf("dial: %w", ErrHostRefused), "", false},
		{"401", fmt.Errorf("%w: /tasks/1/comments -> 401", ErrUnauthorized), BoardWriteOutcomeUnauthorized, true},
		{"500", statusError{path: "/projects/1/tasks", code: 500}, BoardWriteOutcomeWriteRefused, true},
		{"unreachable", fmt.Errorf("%w: timeout", ErrUnreachable), BoardWriteOutcomeNotConfirmed, true},
		{"malformed answer", malformedJSON("create", fmt.Errorf("bad")), BoardWriteOutcomeNotConfirmed, true},
	} {
		outcome, sent := boardWriteOutcome(tc.err)
		if outcome != tc.outcome || sent != tc.sent {
			t.Errorf("%s: boardWriteOutcome = (%q, %v), want (%q, %v)", tc.name, outcome, sent, tc.outcome, tc.sent)
		}
	}
}
