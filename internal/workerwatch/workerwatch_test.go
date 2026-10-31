package workerwatch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func TestParseWorkers(t *testing.T) {
	in := strings.Join([]string{
		"# comment",
		"",
		"cos-a|/wt/a|/briefs/a",
		"cos-b|/wt/b/../b|/briefs/b|extra|fields",
		"cos-a|/wt/x|/briefs/x",
		"bad line",
		"cos-c|relative/wt|/briefs/c",
		"-flag|/wt|/b",
		"cos d|/wt|/b",
		"cos-e||",
		"cos-\u202e|/wt|/b",
	}, "\n")
	entries, problems, err := ParseWorkers(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Name: "cos-a", Worktree: "/wt/a", BriefDir: "/briefs/a"},
		{Name: "cos-b", Worktree: "/wt/b", BriefDir: "/briefs/b"},
		{Name: "cos-e"},
	}
	if fmt.Sprint(entries) != fmt.Sprint(want) {
		t.Errorf("entries = %v, want %v", entries, want)
	}
	if len(problems) != 6 {
		t.Errorf("problems = %d %q, want 6 (dup, bad, relative, -flag, space, bidi)", len(problems), problems)
	}
	for _, p := range problems {
		if !strings.HasPrefix(p, "line ") {
			t.Errorf("problem %q does not name its line", p)
		}
	}
}

func TestModelFromArgv(t *testing.T) {
	tests := []struct {
		argv []string
		want string
	}{
		{[]string{"claude", "--model", "opus", "--permission-mode", "auto"}, "opus"},
		{[]string{"claude", "--model=sonnet[1m]"}, "sonnet[1m]"},
		{[]string{"claude", "--model", "a", "--model", "b"}, "b"},
		{[]string{"claude", "--model"}, ""},
		{[]string{"claude", "--model", "\x1b]52;c;x\x07"}, ""},
		{[]string{"claude", "prompt text --model in it"}, ""},
	}
	for _, tt := range tests {
		if got := ModelFromArgv(tt.argv); got != tt.want {
			t.Errorf("ModelFromArgv(%q) = %q, want %q", tt.argv, got, tt.want)
		}
	}
}

const getJSON = `{"address":"t1n","argv":["env","X=1","/bin/claude","--model","opus","Read the brief"],"created_ms":1000000,"cwd":"/wt/a","foreground":{"argv":["/bin/claude","--model","opus"]},"name":"cos-a","state":"live","status":{"app":"claude-code","state":"blocked"}}`

func TestCLIGetAndList(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != "hc" {
			t.Fatalf("ran %s, want hc", name)
		}
		switch args[0] {
		case "get":
			if args[len(args)-1] == "gone" {
				return "", &exec.CommandError{Name: "hc", ExitCode: 3}
			}
			return getJSON, nil
		case "ls":
			return `{"terminals":[{"address":"t1","name":null,"state":"live","status":"done"},{"address":"t2","name":"cos-x","state":"live","status":"working"},{"address":"bad addr","name":"cos-y"}]}`, nil
		}
		return "", errors.New("unexpected")
	}}
	c := CLI{Run: fake}
	got, err := c.Get(context.Background(), "cos-a")
	if err != nil {
		t.Fatal(err)
	}
	want := Terminal{Address: "t1n", Name: "cos-a", State: "live", Status: StatusBlocked, CreatedMS: 1000000, Model: "opus", Cwd: "/wt/a"}
	if got != want {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	if _, err := c.Get(context.Background(), "gone"); !errors.Is(err, ErrGone) {
		t.Errorf("Get(gone) err = %v, want ErrGone", err)
	}
	if _, err := c.Get(context.Background(), "--force"); err == nil {
		t.Error("Get(--force) ran hc; a flag-shaped target must be refused")
	}
	ts, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[1].Name != "cos-x" || ts[1].Status != StatusWorking {
		t.Errorf("List = %+v, want t1 and t2 (cos-x working), bad address dropped", ts)
	}
	for _, call := range fake.Calls {
		for _, a := range call.Args {
			if a == "send" || a == "keys" || a == "kill" || a == "rm" || a == "interrupt" {
				t.Errorf("the watch ran hc %v; it must only read", call.Args)
			}
		}
	}
}

func TestScanEventsWakesOnRowEventsOnly(t *testing.T) {
	stream := strings.Join([]string{
		`{"channel":null,"seq":1,"terminals":[]}`,
		`{"method":"event","params":{"seq":2,"type":"heartbeat"}}`,
		`{"method":"event","params":{"seq":3,"type":"title","terminal":"t1"}}`,
		`not json`,
		`{"method":"event","params":{"seq":4,"type":"status","terminal":"t1"}}`,
		`{"method":"event","params":{"seq":5,"type":"turn","terminal":"t1"}}`,
	}, "\n")
	wake := make(chan struct{}, 1)
	ScanEvents(strings.NewReader(stream), wake)
	select {
	case <-wake:
	default:
		t.Fatal("no wake for a status event")
	}
	select {
	case <-wake:
		t.Fatal("two wakes queued; bursts must coalesce to one")
	default:
	}

	quiet := make(chan struct{}, 1)
	ScanEvents(strings.NewReader(`{"terminals":[]}`+"\n"+`{"params":{"type":"heartbeat"}}`), quiet)
	if len(quiet) != 0 {
		t.Error("a heartbeat woke the view")
	}
}

func TestEventsWithoutStreamCloses(t *testing.T) {
	ch := CLI{}.Events(context.Background())
	if _, ok := <-ch; ok {
		t.Fatal("Events with no stream runner should close at once, so the view polls")
	}
}

func TestScanReports(t *testing.T) {
	old := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	newer := old.Add(time.Hour)
	long := strings.Repeat("x", 10_000) + "\n\nREPORT-COMPLETE\n\n"
	fsys := fstest.MapFS{
		"brief.md":           {Data: []byte("the brief"), ModTime: old},
		"report.md":          {Data: []byte("# Report\nall done\nREPORT-COMPLETE\n"), ModTime: old},
		"report-round2.md":   {Data: []byte("half way"), ModTime: newer},
		"report-long.md":     {Data: []byte(long), ModTime: old.Add(-time.Hour)},
		"report-quoted.md":   {Data: []byte("REPORT-COMPLETE is what I will write\nnot yet"), ModTime: old.Add(-2 * time.Hour)},
		"report-link.md":     {Data: []byte("/etc/passwd"), Mode: fs.ModeSymlink, ModTime: newer.Add(time.Hour)},
		"report-.md":         {Data: []byte("x"), ModTime: old},
		"notes-report.md":    {Data: []byte("REPORT-COMPLETE"), ModTime: old},
		"report-crlf.md":     {Data: []byte("done\r\nREPORT-COMPLETE\r\n"), ModTime: old.Add(-3 * time.Hour)},
		"report-sub.md/x":    {Data: []byte("x")},
		"blocked-sources.md": {Data: []byte("x")},
	}
	r := ScanReports(fsys)
	var names []string
	for _, f := range r.Files {
		names = append(names, fmt.Sprintf("%s:%v", f.Name, f.Complete))
	}
	want := "report-round2.md:false report.md:true report-long.md:true report-quoted.md:false report-crlf.md:true"
	if strings.Join(names, " ") != want {
		t.Errorf("files = %s\nwant    %s", strings.Join(names, " "), want)
	}
	if n, _ := r.Newest(); n.Name != "report-round2.md" {
		t.Errorf("newest = %s, want report-round2.md", n.Name)
	}
	if r.CompleteCount() != 3 {
		t.Errorf("complete = %d, want 3", r.CompleteCount())
	}
	if got := ScanReportDir("/no/such/dir/for/test"); got.Error != "the brief dir does not exist" {
		t.Errorf("missing dir error = %q", got.Error)
	}
}

const sha = "0123456789abcdef0123456789abcdef01234567"

func TestParsePR(t *testing.T) {
	body := func(reviews ...string) string {
		var nodes []string
		for _, r := range reviews {
			nodes = append(nodes, fmt.Sprintf(`{"body":%q}`, r))
		}
		return `{"data":{"repository":{"pullRequests":{"nodes":[{"number":12,"state":"OPEN","isDraft":true,"headRefOid":"` + sha +
			`","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"PENDING"}}}]},"reviews":{"nodes":[` + strings.Join(nodes, ",") + `]}}]}}}}`
	}
	p, err := parsePR("o/r", body(
		"<!-- cadence-review: pr-reviewer head="+sha+" crit=1 imp=2 -->\nolder",
		"<!-- cadence-review: security-reviewer head=ffffffffffffffffffffffffffffffffffffffff crit=0 imp=0 -->\nnewest, stale head",
		"plain comment",
	))
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 12 || p.State != "OPEN" || !p.Draft || p.Head != sha || p.Checks != "PENDING" {
		t.Errorf("PR = %+v", p)
	}
	if p.Marker == nil || p.Marker.Reviewer != "security-reviewer" || p.Marker.AtHead {
		t.Errorf("marker = %+v, want the newest (security-reviewer, stale)", p.Marker)
	}

	p, _ = parsePR("o/r", body("<!-- cadence-review: pr-reviewer head="+sha+" crit=0 imp=3 -->"))
	if p.Marker == nil || !p.Marker.AtHead || p.Marker.Imp != 3 {
		t.Errorf("marker at head = %+v", p.Marker)
	}

	if _, err := parsePR("o/r", `{"data":{"repository":{"pullRequests":{"nodes":[]}}}}`); !errors.Is(err, ErrNoPR) {
		t.Errorf("no nodes: err = %v, want ErrNoPR", err)
	}
	if _, err := parsePR("o/r", `{"errors":[{"message":"x"}]}`); err == nil || errors.Is(err, ErrNoPR) {
		t.Errorf("errors answer: err = %v, want an unusable-answer error", err)
	}
	bad, _ := parsePR("o/r", strings.Replace(strings.Replace(body(), "PENDING", "WEIRD", 1), sha, "not-a-sha", 1))
	if bad.Checks != "" || bad.Head != "" {
		t.Errorf("unknown enum or bad sha kept: %+v", bad)
	}
}

func TestRepoFromRemote(t *testing.T) {
	tests := map[string]string{
		"git@github.com:o/r.git":         "o/r",
		"https://github.com/o/r":         "o/r",
		"https://gitlab.com/o/r":         "",
		"https://tok@github.com/o/r.git": "o/r",
	}
	for in, want := range tests {
		got, _ := RepoFromRemote(in)
		if got != want {
			t.Errorf("RepoFromRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeHC answers Get from a map.
type fakeHC struct {
	mu    sync.Mutex
	terms map[string]Terminal
	list  []Terminal
	reads int
}

func (f *fakeHC) Get(_ context.Context, target string) (Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.terms[target]
	if !ok {
		return Terminal{}, ErrGone
	}
	return t, nil
}

func (f *fakeHC) List(context.Context) ([]Terminal, error) { return f.list, nil }

func (f *fakeHC) Read(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return "", nil
}

type fakeGit struct{}

func (fakeGit) Branch(_ context.Context, wt string) (string, error) {
	if wt == "/wt/detached" {
		return "", nil
	}
	return "branch-" + wt[len("/wt/"):], nil
}

func (fakeGit) OriginURL(context.Context, string) (string, error) {
	return "git@github.com:o/r.git", nil
}

type fakeGH struct {
	mu    sync.Mutex
	calls []string
	fail  bool
}

func (f *fakeGH) PRFor(_ context.Context, repo, branch string) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, branch)
	if f.fail {
		return PR{}, errors.New("rate limited")
	}
	if branch == "branch-nopr" {
		return PR{}, ErrNoPR
	}
	return PR{Repo: repo, Number: len(f.calls), State: "OPEN", Head: sha}, nil
}

func TestCollectorCachesAndSpreadsGitHubReads(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	hc := &fakeHC{terms: map[string]Terminal{
		"cos-a": {Address: "t1", Name: "cos-a", State: "live", Status: StatusWorking, CreatedMS: now.Add(-5 * time.Minute).UnixMilli(), Model: "opus"},
		"cos-b": {Address: "t2", Name: "cos-b", State: "live", Status: StatusBlocked},
		"cos-c": {Address: "t3", Name: "cos-c", State: "live", Status: StatusDone},
	}}
	gh := &fakeGH{}
	c := &Collector{
		HC: hc, Git: fakeGit{}, GH: gh,
		Reports: func(string) Reports { return Reports{Files: []ReportFile{{Name: "report.md", Complete: true}}} },
		Now:     func() time.Time { return now },
		Entries: []Entry{{Name: "cos-a", Worktree: "/wt/a", BriefDir: "/b/a"}, {Name: "cos-b", Worktree: "/wt/b", BriefDir: "/b/b"}, {Name: "cos-c", Worktree: "/wt/c"}, {Name: "cos-gone", Worktree: "/wt/gone"}},
		GHTTL:   time.Minute,
	}
	s := c.Collect(context.Background(), false)
	if len(gh.calls) != ghPerCollect {
		t.Fatalf("first collect made %d gh calls, want %d (spread over collects)", len(gh.calls), ghPerCollect)
	}
	if s.Workers[0].AgeSeconds != 300 || s.Workers[0].Model != "opus" || s.Workers[0].Reports.CompleteCount() != 1 {
		t.Errorf("row a = %+v", s.Workers[0])
	}
	if s.Workers[3].HCError != "no hc terminal by this name" {
		t.Errorf("gone row = %q", s.Workers[3].HCError)
	}
	if got := s.NeedingYou(); len(got) != 1 || got[0].Name != "cos-b" {
		t.Errorf("needing you = %+v, want cos-b", got)
	}

	c.Collect(context.Background(), false)
	if len(gh.calls) != 4 {
		t.Fatalf("second collect: %d gh calls total, want 4 (the two cold rows)", len(gh.calls))
	}
	c.Collect(context.Background(), false)
	if len(gh.calls) != 4 {
		t.Fatalf("third collect inside the TTL made gh calls: %v", gh.calls)
	}
	now = now.Add(5 * time.Second)
	c.Collect(context.Background(), true)
	if len(gh.calls) != 4 {
		t.Fatalf("a forced refresh 5s after a read called gh again: %v", gh.calls)
	}
	now = now.Add(ForceMinAge)
	c.Collect(context.Background(), true)
	if len(gh.calls) != 4+ghPerCollect {
		t.Fatalf("a forced refresh past ForceMinAge: %d calls, want %d", len(gh.calls), 4+ghPerCollect)
	}

	gh.fail = true
	now = now.Add(time.Hour)
	s = c.Collect(context.Background(), false)
	for _, w := range s.Workers[2:] { // c and d are the stalest, so they refresh first
		if w.PR == nil || !strings.Contains(w.PRNote, "could not be read") {
			t.Errorf("failed refresh lost the last good PR or its note: %+v", w)
		}
	}
}

func TestCollectorTTLFloor(t *testing.T) {
	now := time.Now()
	gh := &fakeGH{}
	c := &Collector{HC: &fakeHC{terms: map[string]Terminal{}}, Git: fakeGit{}, GH: gh, Now: func() time.Time { return now },
		Entries: []Entry{{Name: "cos-a", Worktree: "/wt/a"}}, GHTTL: time.Second}
	c.Collect(context.Background(), false)
	now = now.Add(10 * time.Second)
	c.Collect(context.Background(), false)
	if len(gh.calls) != 1 {
		t.Errorf("a 1s --gh-ttl was honoured below the %s floor: %d calls", MinGHTTL, len(gh.calls))
	}
}

func TestCollectorCos(t *testing.T) {
	hc := &fakeHC{
		terms: map[string]Terminal{"cos-z": {Address: "t9", Name: "cos-z", State: "live", Status: StatusIdle, Cwd: "/wt/detached"}},
		list: []Terminal{
			{Address: "t9", Name: "cos-z", State: "live"},
			{Address: "t8", Name: "other", State: "live"},
			{Address: "t7", Name: "cos-old", State: "finished"},
			{Address: "t6", Name: "cos-a", State: "live"},
		},
	}
	c := &Collector{HC: hc, Git: fakeGit{}, GH: &fakeGH{}, Cos: true, Entries: []Entry{{Name: "cos-a"}}}
	s := c.Collect(context.Background(), false)
	if len(s.Workers) != 2 || s.Workers[1].Name != "cos-z" || s.Workers[1].Source != "hc" || s.Workers[0].Source != "file" {
		t.Fatalf("workers = %+v, want cos-a (file) then cos-z (hc)", s.Workers)
	}
	if s.Workers[1].Worktree != "/wt/detached" || s.Workers[1].PRNote != "the worktree has no branch checked out" {
		t.Errorf("cos find did not use hc's cwd as its worktree: %+v", s.Workers[1])
	}
}
