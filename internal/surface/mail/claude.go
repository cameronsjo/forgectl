package mail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ClaudeAdapter delivers into a running Claude Code session over its inbox
// socket, the path Anthropic documents for "a script or hook to post into a
// session".
//
// The frame format, the session registry (<config>/sessions/<pid>.json) and
// the auth-key file are not documented. They follow what
// github.com/PeterSR/claude-code-socket-transport read out of Claude Code
// v2.1.233 and moltenbits/sideband confirmed on v2.1.263. They are Claude Code
// internals with no compatibility promise, so every failure here is soft: the
// message stays queued with the reason.
type ClaudeAdapter struct {
	// ConfigDir is $CLAUDE_CONFIG_DIR, or ~/.claude.
	ConfigDir string
	// Timeout bounds one send, from dial to the receiver closing. Zero is 5s.
	Timeout time.Duration
}

// ClaudeConfigDir resolves Claude Code's config directory.
func ClaudeConfigDir(getenv func(string) string) (string, error) {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

type claudeSession struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	CWD        string `json:"cwd"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
	Socket     string `json:"messagingSocketPath"`
	Kind       string `json:"kind"`
}

type claudeAuth struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

type claudeContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeFrame struct {
	MsgV      int           `json:"msgV"`
	MsgID     string        `json:"msg_id"`
	Type      string        `json:"type"`
	Message   claudeContent `json:"message"`
	Priority  string        `json:"priority,omitempty"`
	SessionID string        `json:"session_id,omitempty"`
}

const claudeProbeTimeout = 250 * time.Millisecond

// Deliver posts text as a user frame. The frame carries the session id read
// from the registry just now, so a socket path reused by another session
// drops it instead of misdelivering.
func (a ClaudeAdapter) Deliver(ctx context.Context, w Worker, m Message, text string) (string, error) {
	s, err := a.resolve(ctx, w)
	if err != nil {
		return "", err
	}
	frame := claudeFrame{
		MsgV:      1,
		MsgID:     m.ID,
		Type:      "user",
		Message:   claudeContent{Role: "user", Content: text},
		Priority:  string(m.Priority),
		SessionID: s.SessionID,
	}
	if err := a.post(ctx, s.Socket, a.token(s.PID, s.Socket), frame); err != nil {
		return "", err
	}
	if s.PID > 0 {
		return fmt.Sprintf("posted to claude pid %d", s.PID), nil
	}
	return "posted to claude inbox", nil
}

// State maps the registry's status: busy and shell are busy, waiting means the
// session is blocked on the operator (a permission prompt, a question).
func (a ClaudeAdapter) State(ctx context.Context, w Worker) (WorkerState, error) {
	s, err := a.resolve(ctx, w)
	if err != nil {
		if IsRetryable(err) {
			return StateAbsent, nil
		}
		return StateUnknown, err
	}
	switch s.Status {
	case "idle":
		return StateIdle, nil
	case "busy", "shell":
		return StateBusy, nil
	case "waiting":
		return StateWaiting, nil
	}
	return StateUnknown, nil
}

// resolve finds the live session for a worker: the socket the ledger already
// knows, else the registry entry whose cwd is the worker's worktree.
func (a ClaudeAdapter) resolve(ctx context.Context, w Worker) (claudeSession, error) {
	sessions, err := a.sessions()
	if err != nil {
		return claudeSession{}, NotReady("read claude session registry: %v", err)
	}
	if w.Socket != "" {
		for _, s := range sessions {
			if samePath(s.Socket, w.Socket) {
				return s, nil
			}
		}
		if !probe(ctx, w.Socket, claudeProbeTimeout) {
			return claudeSession{}, NotReady("claude inbox %s is not answering", w.Socket)
		}
		return claudeSession{Socket: w.Socket}, nil
	}

	want, err := canonicalPath(w.Worktree)
	if err != nil {
		return claudeSession{}, NotReady("worktree %s: %v", w.Worktree, err)
	}
	var live []claudeSession
	for _, s := range sessions {
		got, err := canonicalPath(s.CWD)
		if err != nil || got != want {
			continue
		}
		if !probe(ctx, s.Socket, claudeProbeTimeout) {
			continue
		}
		live = append(live, s)
	}
	switch len(live) {
	case 0:
		return claudeSession{}, NotReady("no live claude session in %s yet", w.Worktree)
	case 1:
		return live[0], nil
	}
	var named []claudeSession
	for _, s := range live {
		if s.Name == w.Name {
			named = append(named, s)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	return claudeSession{}, fmt.Errorf("%d live claude sessions in %s and none uniquely named %s", len(live), w.Worktree, quoteTrunc(w.Name))
}

func (a ClaudeAdapter) sessions() ([]claudeSession, error) {
	dir := filepath.Join(a.ConfigDir, "sessions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []claudeSession
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: a registry entry ReadDir listed under the Claude config dir
		if err != nil {
			continue
		}
		var s claudeSession
		if json.Unmarshal(data, &s) != nil || s.Socket == "" {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// token reads the peer token the session published beside its registry entry,
// <config>/sessions/<pid>.<sha256(socket path)>.key. Without one the frame goes
// unauthenticated, which macOS and Linux accept; the token tells the receiver
// which permission class the sender belongs to.
func (a ClaudeAdapter) token(pid int, socket string) string {
	if pid <= 0 || socket == "" {
		return ""
	}
	var candidates []string
	if abs, err := filepath.Abs(socket); err == nil {
		candidates = append(candidates, abs)
	}
	if resolved, err := filepath.EvalSymlinks(socket); err == nil {
		candidates = append(candidates, resolved)
	}
	for _, p := range candidates {
		sum := sha256.Sum256([]byte(p))
		keyPath := filepath.Join(a.ConfigDir, "sessions", fmt.Sprintf("%d.%s.key", pid, hex.EncodeToString(sum[:])))
		data, err := os.ReadFile(keyPath) //nolint:gosec // G304: a key file name built from a pid and a hash, under the Claude config dir
		if err != nil {
			continue
		}
		var k struct {
			PeerToken string `json:"peerToken"`
		}
		if json.Unmarshal(data, &k) == nil && k.PeerToken != "" {
			return k.PeerToken
		}
	}
	return ""
}

// post writes the optional auth line and one frame, half-closes, and waits for
// the receiver to close its side.
func (a ClaudeAdapter) post(ctx context.Context, socket, token string, frame claudeFrame) error {
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if token != "" {
		if err := enc.Encode(claudeAuth{Type: "auth", Token: token}); err != nil {
			return err
		}
	}
	if err := enc.Encode(frame); err != nil {
		return err
	}
	if buf.Len() > maxRecord {
		return fmt.Errorf("frame is %d bytes, over the inbox's 1 MiB line cap", buf.Len())
	}

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return NotReady("dial claude inbox %s: %v", socket, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return NotReady("write claude inbox %s: %v", socket, err)
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	_, _ = io.Copy(io.Discard, conn)
	return nil
}

// probe reports whether something accepts connections on a unix socket.
func probe(ctx context.Context, socket string, timeout time.Duration) bool {
	if socket == "" {
		return false
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// canonicalPath makes two spellings of one directory compare equal, including
// macOS's /var and /private/var.
func canonicalPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca, errA := canonicalPath(a)
	cb, errB := canonicalPath(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ca == cb
}
