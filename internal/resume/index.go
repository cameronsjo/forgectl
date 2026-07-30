// Package resume reads the session records Claude Code leaves on disk, merges
// them with the snapshots forgectl takes of what Claude Code deletes, and
// hands `forgectl resume` one list: every recent session across every repo,
// newest first, with the cwd needed to resume it and the tasks needed to make
// that resume useful.
//
// Two halves with different root causes, which is why the package is shaped
// this way:
//
//   - Session IDENTITY is already durable. ~/.claude/history.jsonl records
//     every prompt with its sessionId, project (= cwd) and timestamp, and the
//     per-project transcript carries cwd, gitBranch and an ai-title. Neither
//     is pruned. Identity therefore needs a reader (Scan), not a persister.
//   - Session TASKS are not. Claude Code removes the JSON under
//     ~/.claude/tasks/<dir>/ when a session ends, leaving only a
//     .highwatermark to prove they existed. Rescuing them needs a real
//     snapshot (Snapshot) and a write-back (Restore).
//
// Every entry point takes an explicit Paths, so the risk-bearing core never
// calls os.UserHomeDir() and the tests run against a fixture $HOME — the same
// shape as launch.resolve.
package resume

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Paths are the roots this package reads and writes. ClaudeHome is the
// ~/.claude tree Claude Code owns (read-only to us); StoreDir is forgectl's
// own snapshot store, the only thing here we write.
type Paths struct {
	ClaudeHome string
	StoreDir   string
}

// DefaultPaths resolves the real locations. It is the one place in the package
// that touches the environment; every other function takes Paths.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	store, err := config.ResumeStoreDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{ClaudeHome: filepath.Join(home, ".claude"), StoreDir: store}, nil
}

func (p Paths) historyPath() string { return filepath.Join(p.ClaudeHome, "history.jsonl") }
func (p Paths) projectsDir() string { return filepath.Join(p.ClaudeHome, "projects") }
func (p Paths) registryDir() string { return filepath.Join(p.ClaudeHome, "sessions") }
func (p Paths) tasksDir() string    { return filepath.Join(p.ClaudeHome, "tasks") }
func (p Paths) teamsDir() string    { return filepath.Join(p.ClaudeHome, "teams") }

// Session is one merged record — everything the picker shows plus everything
// the resume action needs.
type Session struct {
	ID         string    `json:"id"`
	Cwd        string    `json:"cwd"`
	Name       string    `json:"name,omitempty"`
	NameSource string    `json:"name_source,omitempty"`
	Repo       string    `json:"repo,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	LastPrompt string    `json:"last_prompt,omitempty"`
	LastActive time.Time `json:"last_active"`
	Version    string    `json:"version,omitempty"`
	Live       bool      `json:"live"`
	Pid        int       `json:"pid,omitempty"`
	TaskDir    string    `json:"task_dir,omitempty"`
	Tasks      []Task    `json:"tasks,omitempty"`
}

// Name sources, in the precedence Scan applies (highest first). The source is
// carried alongside the name so the picker and `resume ls --json` can say
// where a label came from — an ai-title is a guess, a /rename name is not.
const (
	NameFromRename  = "rename"
	NameFromStore   = "store"
	NameFromAITitle = "ai-title"
	NameFromLane    = "lane"
)

// Opts tunes a Scan.
type Opts struct {
	// Limit caps how many sessions are returned, newest first. Zero means
	// DefaultLimit. The cut happens BEFORE transcripts are opened, so a
	// thousand-session corpus is not parsed to show ten rows.
	Limit int
	// Filter, when non-empty, keeps only sessions whose repo, name, or cwd
	// contains it (case-insensitive). Applied before the limit cut.
	Filter string
	// IncludeLive keeps sessions whose registry pid is still alive. The
	// picker wants them (shown, then refused with a clear reason); a
	// hypothetical batch caller might not.
	IncludeLive bool
}

// DefaultLimit is how many sessions `forgectl resume` shows when unbounded.
// Large enough to cover a few days of work, small enough that the transcript
// reads behind it stay imperceptible.
const DefaultLimit = 25

// prefilterFactor bounds how many sessions survive the history pass before
// filtering and the limit cut. Filtering can reject most candidates, so the
// pre-cut pool is deliberately wider than Limit.
const prefilterFactor = 8

// historyRecord is the subset of a ~/.claude/history.jsonl line we read. The
// file is the only global, append-only index of sessions, which makes it the
// spine of the merge: every other source is keyed off the ids found here.
type historyRecord struct {
	Display   string `json:"display"`
	Timestamp int64  `json:"timestamp"` // epoch milliseconds
	Project   string `json:"project"`   // the session's cwd
	SessionID string `json:"sessionId"`
}

// Scan merges every source into one newest-first list.
//
// Order matters for cost, not just for precedence: history.jsonl is read in
// full (it is the only index), then filtered and cut, and only the survivors
// have their transcripts opened. The registry, store, and per-repo lane files
// are small enough to read wholesale.
func Scan(p Paths, opts Opts) ([]Session, error) {
	if opts.Limit <= 0 {
		opts.Limit = DefaultLimit
	}

	byID, err := scanHistory(p.historyPath())
	if err != nil {
		return nil, err
	}

	reg := readRegistry(p.registryDir())
	store := LoadAll(p.StoreDir)

	// history.jsonl is the spine, but it is not the whole skeleton: a
	// session's prompts are recorded against the id in force when they were
	// typed, so a session that arrived through /clear can be live, real, and
	// entirely absent from history (measured — every currently-running
	// session on this machine was). Both other sources therefore contribute
	// ids of their own rather than only decorating history's.
	for id, snap := range store {
		if _, ok := byID[id]; ok {
			continue
		}
		byID[id] = &Session{ID: id, Cwd: snap.Cwd, LastActive: snap.LastSeen}
	}
	for id, e := range reg {
		if _, ok := byID[id]; ok {
			continue
		}
		byID[id] = &Session{ID: id, Cwd: e.Cwd, LastActive: e.Updated()}
	}

	sessions := make([]*Session, 0, len(byID))
	for _, s := range byID {
		s.Repo = repoName(s.Cwd)
		if snap, ok := store[s.ID]; ok {
			s.Tasks = snap.Tasks
			s.TaskDir = snap.TaskDir
			if s.Cwd == "" {
				s.Cwd = snap.Cwd
			}
			// A snapshot is written at every turn end, so LastSeen can be
			// newer than the last recorded prompt — and once the registry
			// entry is pruned at process exit it may be the only surviving
			// activity signal. Widening here (not just when the id is new)
			// keeps a session from sorting below its true recency, or
			// falling out of the limit cut altogether.
			if snap.LastSeen.After(s.LastActive) {
				s.LastActive = snap.LastSeen
			}
			if snap.Name != "" {
				s.Name, s.NameSource = snap.Name, NameFromStore
			}
			if snap.Version != "" {
				s.Version = snap.Version
			}
			if snap.Branch != "" {
				s.Branch = snap.Branch
			}
		}
		if e, ok := reg[s.ID]; ok {
			s.Pid, s.Version, s.Live = e.Pid, e.Version, e.Live
			if e.Cwd != "" {
				s.Cwd, s.Repo = e.Cwd, repoName(e.Cwd)
			}
			// The registry reports activity a prompt does not: a session
			// working through a long turn has a fresh updatedAt and a
			// stale last prompt, and "when was I last in this" is what
			// the ordering answers.
			if u := e.Updated(); u.After(s.LastActive) {
				s.LastActive = u
			}
			// The registry's name is the /rename name straight from the
			// live process: the highest authority there is.
			if e.Name != "" {
				s.Name, s.NameSource = e.Name, NameFromRename
			}
		}
		if !opts.IncludeLive && s.Live {
			continue
		}
		sessions = append(sessions, s)
	}

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].LastActive.Equal(sessions[j].LastActive) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].LastActive.After(sessions[j].LastActive)
	})

	// Pre-cut to a pool wider than Limit: enrichment supplies the ai-title
	// and lane name, which Matches then filters on, so cutting to exactly
	// Limit here would hide matches whose only searchable text has not been
	// read yet.
	pool := opts.Limit * prefilterFactor
	if opts.Filter == "" {
		pool = opts.Limit
	}
	if len(sessions) > pool {
		sessions = sessions[:pool]
	}

	out := make([]Session, 0, opts.Limit)
	for _, s := range sessions {
		enrich(p, s)
		if !Matches(*s, opts.Filter) {
			continue
		}
		out = append(out, *s)
		if len(out) == opts.Limit {
			break
		}
	}
	return out, nil
}

// Matches reports whether a session answers to filter — a case-insensitive
// substring of its repo, name, cwd, or id. An empty filter matches everything.
func Matches(s Session, filter string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	for _, field := range []string{s.Repo, s.Name, s.Cwd, s.ID} {
		if strings.Contains(strings.ToLower(field), f) {
			return true
		}
	}
	return false
}

// scanHistory folds history.jsonl into one record per session id, keeping the
// newest prompt and timestamp for each.
//
// It decodes as a JSON *stream* rather than scanning lines: a history entry
// embeds pasted content verbatim, so single lines run to megabytes and a
// bufio.Scanner would need an arbitrary buffer ceiling to match.
func scanHistory(path string) (map[string]*Session, error) {
	f, err := os.Open(path) // #nosec G304 -- a fixed name under the caller's own ~/.claude
	if err != nil {
		// No history is an empty list, not a failure: a fresh machine has
		// nothing to resume and should say so calmly.
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]*Session{}, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	byID := map[string]*Session{}
	// Line-wise with a skip, NOT a json.Decoder stream. A Decoder does not
	// resynchronize at line boundaries, so one corrupt record anywhere ends
	// the whole read — and since history.jsonl is append-only, what that
	// silently discards is everything AFTER the bad line: the newest
	// sessions, which are exactly the ones the picker exists to show.
	forEachLine(f, func(line []byte) bool {
		var rec historyRecord
		if json.Unmarshal(line, &rec) != nil {
			return false
		}
		// Validate at the point of ADMISSION. Ids from this file flow on
		// into filepath.Join (transcript lookup, task dirs) and into
		// claude's argv, and guarding each of those sinks separately is how
		// one gets missed — transcriptPath was exactly that miss.
		if !validSessionID(rec.SessionID) {
			return false
		}
		ts := time.UnixMilli(rec.Timestamp)
		s, ok := byID[rec.SessionID]
		if !ok {
			byID[rec.SessionID] = &Session{
				ID: rec.SessionID, Cwd: rec.Project,
				LastPrompt: firstLine(rec.Display), LastActive: ts,
			}
			return false
		}
		if ts.After(s.LastActive) {
			s.LastActive, s.LastPrompt = ts, firstLine(rec.Display)
			if rec.Project != "" {
				s.Cwd = rec.Project
			}
		}
		return false
	})
	return byID, nil
}

// repoName is the picker's repo column. filepath.Base is not enough on its
// own: it answers "." for an empty path, and a session recovered from a store
// record with no cwd would show a bare dot where a repo name belongs.
func repoName(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

// promptWidth is how much of a prompt the picker can show on one row, in runes.
const promptWidth = 90

// firstLine reduces a prompt to a single truncated line. Prompts are
// multi-line and carry pasted blocks; the picker has one row.
//
// The clip is by RUNE, not byte. A byte slice through a multi-byte character
// leaves invalid UTF-8, which json.Marshal then silently rewrites to U+FFFD —
// a mangled prompt in --json output with nothing to indicate why.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > promptWidth {
		return strings.TrimSpace(string(r[:promptWidth])) + "…"
	}
	return s
}

// enrich fills in what only the transcript and the per-repo lane file know:
// the git branch, the ai-title fallback name, and the lane name below it.
// Both reads are best-effort — a session with neither is still resumable.
func enrich(p Paths, s *Session) {
	branch, title := readTranscript(p, s.ID, s.Cwd)
	if s.Branch == "" {
		s.Branch = branch
	}
	if s.Name == "" && title != "" {
		s.Name, s.NameSource = title, NameFromAITitle
	}
	if s.Name == "" {
		if lane := readLaneName(s.Cwd, s.ID); lane != "" {
			s.Name, s.NameSource = lane, NameFromLane
		}
	}
}

// transcriptRecord is the subset of a transcript line we read.
type transcriptRecord struct {
	Type      string `json:"type"`
	GitBranch string `json:"gitBranch"`
	AITitle   string `json:"aiTitle"`
}

// transcriptBufSize is the read buffer for a transcript line. Transcript lines
// embed whole message bodies and pasted blocks, so some run far past this; a
// longer line is skipped (see forEachLine) rather than buffered, since neither
// field we want lives on one.
const transcriptBufSize = 64 << 10 // 64 KiB

// forEachLine calls fn for every line in r that fits the read buffer, skipping
// any longer one and continuing.
//
// It uses bufio.Reader rather than bufio.Scanner deliberately. A Scanner with a
// size cap does NOT skip an over-long token — it fails with ErrTooLong and stops
// UNRECOVERABLY, returning false from every later Scan with the underlying
// reader advanced an arbitrary distance. Against transcripts, where one pasted
// blob can dwarf every other line, that turns a bound that reads like a safety
// limit into a silent truncation point: the fields we want sit on ordinary short
// records, and a single huge line early on would cost us all of them.
func forEachLine(r io.Reader, fn func(line []byte) (stop bool)) {
	br := bufio.NewReaderSize(r, transcriptBufSize)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Over-long line: drain to its newline and move on.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			if err != nil {
				return
			}
			continue
		}
		if len(line) > 0 && fn(line) {
			return
		}
		if err != nil {
			return
		}
	}
}

// readTranscript pulls the git branch and the ai-title out of a session's
// transcript.
//
// It STREAMS rather than reading the file whole, and that is the point: a
// transcript is tens of megabytes, both fields appear within the first handful
// of records, and Scan calls this once per candidate session — up to
// Limit*prefilterFactor of them when a filter is set. Reading each file into
// memory first made the early exit below decorative: the I/O had already
// happened. Streaming makes it real, so the common case touches a few kilobytes
// per session instead of the whole file.
//
// The substring pre-filter is the second half: a transcript is almost entirely
// message bodies, and only a small minority of lines carry either field, so
// most lines are rejected without a JSON decode.
func readTranscript(p Paths, id, cwd string) (branch, title string) {
	path := transcriptPath(p, id, cwd)
	if path == "" {
		return "", ""
	}
	f, err := os.Open(path) // #nosec G304 -- resolved under the caller's own ~/.claude/projects
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()

	forEachLine(f, func(line []byte) bool {
		wantsBranch := branch == "" && bytes.Contains(line, []byte(`"gitBranch"`))
		wantsTitle := title == "" && bytes.Contains(line, []byte(`"ai-title"`))
		if !wantsBranch && !wantsTitle {
			return false
		}
		var rec transcriptRecord
		if json.Unmarshal(line, &rec) != nil {
			return false
		}
		if wantsBranch && rec.GitBranch != "" {
			branch = rec.GitBranch
		}
		if wantsTitle && rec.Type == "ai-title" && rec.AITitle != "" {
			title = rec.AITitle
		}
		return branch != "" && title != ""
	})
	return branch, title
}

// transcriptPath locates <projects>/<slug>/<id>.jsonl. The slug is the cwd
// with every non-alphanumeric byte replaced by '-', but that encoding is
// Claude Code's, not ours — so a miss falls back to searching the project
// dirs by filename rather than trusting the derivation.
func transcriptPath(p Paths, id, cwd string) string {
	if id == "" {
		return ""
	}
	name := id + ".jsonl"
	if cwd != "" {
		guess := filepath.Join(p.projectsDir(), slugify(cwd), name)
		if _, err := os.Stat(guess); err == nil {
			return guess
		}
	}
	entries, err := os.ReadDir(p.projectsDir())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(p.projectsDir(), e.Name(), name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// slugify reproduces Claude Code's project-directory encoding: every byte
// outside [A-Za-z0-9] becomes '-', so /Users/x/.dotfiles is -Users-x--dotfiles.
func slugify(cwd string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, cwd)
}

// laneFile is the per-repo session lane record some tooling writes into
// <repo>/.claude/sessions/. It is the lowest-precedence name source.
type laneFile struct {
	Name      string `json:"name"`
	SessionID string `json:"session_id"`
}

// readLaneName returns the lane name declared for this session inside its own
// repo, if one is there.
func readLaneName(cwd, id string) string {
	if cwd == "" || id == "" {
		return ""
	}
	dir := filepath.Join(cwd, ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- a .json under the session's own repo
		if err != nil {
			continue
		}
		var lf laneFile
		if json.Unmarshal(data, &lf) != nil {
			continue
		}
		if lf.SessionID == id && lf.Name != "" {
			return lf.Name
		}
	}
	return ""
}
