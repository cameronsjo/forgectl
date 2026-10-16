package attention

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Validation patterns. An id is only ever a lookup key or, for a session,
// the stem of one file name in the spool; it never becomes a path.
var (
	sessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	paneRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_-]{0,63}$`)
	turnRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	letterTag = regexp.MustCompile(`^[A-Z]$`)
)

const (
	maxSocketBytes = 1024
	maxCwdRunes    = 255
)

// ValidSession reports whether s is a session id the spool accepts.
func ValidSession(s string) bool { return sessionRe.MatchString(s) }

// ValidPane reports whether s is a herdr pane id (herdr-bridge's ID_RE).
func ValidPane(s string) bool { return paneRe.MatchString(s) }

// Record is one session's latest reply, as the spool holds it.
type Record struct {
	Session string `json:"session"`
	// Pane and Socket are the session's HERDR_PANE_ID and HERDR_SOCKET_PATH
	// as the session reported them. Prune compares them with herdr; a viewer
	// never jumps to Pane.
	Pane   string `json:"pane,omitempty"`
	Socket string `json:"socket,omitempty"`
	// Cwd is the basename of the session's working directory.
	Cwd  string `json:"cwd,omitempty"`
	Turn string `json:"turn"`
	// At is when the record was written. Since is the At of the record it
	// replaced, so roughly when this turn began; nil for a session's first.
	At    time.Time  `json:"at"`
	Since *time.Time `json:"since,omitempty"`
	Items []Item     `json:"items"`
}

// errInvalid wraps every validation refusal.
var errInvalid = errors.New("attention: invalid record")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errInvalid, fmt.Sprintf(format, args...))
}

// sanitize checks every field of r and returns it with its items capped to
// MaxItems and each text cleaned and cut. The refusals name a field, never
// echo its value: the spool is untrusted, and a refusal can reach a terminal.
func (r Record) sanitize() (Record, error) {
	switch {
	case !ValidSession(r.Session):
		return Record{}, invalid("session is not a session id")
	case r.Pane != "" && !ValidPane(r.Pane):
		return Record{}, invalid("pane is not a herdr pane id")
	case r.Socket != "" && !validSocket(r.Socket):
		return Record{}, invalid("socket is not a clean absolute path")
	case r.Cwd != "" && !validCwd(r.Cwd):
		return Record{}, invalid("cwd is not one directory name")
	case !turnRe.MatchString(r.Turn):
		return Record{}, invalid("turn is not a turn id")
	case r.At.IsZero():
		return Record{}, invalid("at is missing")
	}
	if len(r.Items) > MaxItems {
		r.Items = r.Items[:MaxItems]
	}
	items := make([]Item, 0, len(r.Items))
	for i, it := range r.Items {
		clean, err := it.sanitize()
		if err != nil {
			return Record{}, invalid("item %d: %v", i+1, err)
		}
		items = append(items, clean)
	}
	r.Items = items
	r.At = r.At.UTC()
	if r.Since != nil {
		since := r.Since.UTC()
		r.Since = &since
	}
	return r, nil
}

func (it Item) sanitize() (Item, error) {
	switch it.Kind {
	case KindQuestion, KindAction, KindNote, KindOption:
	default:
		return Item{}, errors.New("unknown kind")
	}
	switch {
	case it.N < 0 || it.N > maxN:
		return Item{}, errors.New("n out of range")
	case it.Actor != "" && it.Actor != ActorCameron && it.Actor != ActorClaude:
		return Item{}, errors.New("unknown actor")
	case it.Tag != "" && !letterTag.MatchString(it.Tag):
		return Item{}, errors.New("tag is not one letter")
	}
	switch it.Rec {
	case "", RecNow, RecLater, RecPark, RecForget:
	default:
		return Item{}, errors.New("unknown recommendation")
	}
	it.Text = Clean(it.Text, MaxText)
	return it, nil
}

func validSocket(s string) bool {
	return len(s) <= maxSocketBytes && filepath.IsAbs(s) && filepath.Clean(s) == s && Clean(s, 0) == s
}

func validCwd(s string) bool {
	return utf8.RuneCountInString(s) <= maxCwdRunes && s != "." && s != ".." &&
		!strings.ContainsAny(s, "/\x00") && Clean(s, 0) == s
}

// Validate checks r as Put would, without writing it.
func Validate(r Record) error {
	_, err := r.sanitize()
	return err
}
