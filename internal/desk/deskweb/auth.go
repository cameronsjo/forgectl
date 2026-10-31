//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package deskweb

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk/ask"
)

// TokenName is the per-install token file inside the asks directory.
const TokenName = "web.token"

// tokenHexLen is the token's length: 32 random bytes, hex-encoded.
const tokenHexLen = 64

// sessionTTL is how long a session cookie lasts. Browsers send a cookie to
// every port on its host, so other servers on 127.0.0.1 (or the same ts.net
// name) receive it too; a week bounds how long a copy stays useful.
const sessionTTL = 7 * 24 * time.Hour

// cookiePrefix starts the session cookie's name; the listen port follows, so
// desks on two ports keep separate cookies.
const cookiePrefix = "desk_session_"

// LoadToken reads <deskDir>/asks/web.token, creating it with a fresh random
// token when it is missing or when rotate is set. The file is 0600 and never
// followed through a symlink.
func LoadToken(deskDir string, rotate bool) (string, error) {
	p := filepath.Join(deskDir, ask.SubDir, TokenName)
	if !rotate {
		tok, err := readToken(p)
		if err == nil {
			return tok, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return writeToken(p)
}

func readToken(p string) (string, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: fixed name inside the desk's asks directory
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("deskweb: %s is not a regular file", TokenName)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("deskweb: %s is readable by others (mode %o); rerun with --rotate-token", TokenName, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if !validToken(tok) {
		return "", fmt.Errorf("deskweb: %s does not hold a token; rerun with --rotate-token", TokenName)
	}
	return tok, nil
}

func validToken(tok string) bool {
	if len(tok) != tokenHexLen {
		return false
	}
	_, err := hex.DecodeString(tok)
	return err == nil
}

// writeToken writes a new token to a temp file and renames it into place, so
// a reader never sees a half-written token and a symlink at p is replaced,
// not followed.
func writeToken(p string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	tmp, err := os.CreateTemp(filepath.Dir(p), ".web.token-*")
	if err != nil {
		return "", fmt.Errorf("deskweb: create the token file: %w", err)
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Rename(name, p); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("deskweb: install the token file: %w", err)
	}
	return tok, nil
}

// keys holds the secrets derived from the install token. Rotating the token
// changes both, which ends every session and invalidates every form.
type keys struct {
	token   [32]byte // sha256 of the token, for constant-time comparison
	session []byte
	csrf    []byte
	// port is the listen port a session is bound to: a cookie signed by a
	// desk on another port does not open this one.
	port string
}

func deriveKeys(token string) keys {
	mac := func(label string) []byte {
		h := hmac.New(sha256.New, []byte(token))
		h.Write([]byte(label))
		return h.Sum(nil)
	}
	return keys{token: sha256.Sum256([]byte(token)), session: mac("desk-session-v1"), csrf: mac("desk-csrf-v1")}
}

// tokenOK compares a presented token with the install token in constant
// time (both hashed first, so length leaks nothing either).
func (k keys) tokenOK(presented string) bool {
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], k.token[:]) == 1
}

// newSession returns a signed cookie value: v2.<port>.<expiry>.<random>.<mac>.
// It is stateless, so a session survives a server restart on the same port
// and ends when it expires, the token rotates, or the port changes.
func (k keys) newSession(now time.Time) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	body := "v2." + k.port + "." + strconv.FormatInt(now.Add(sessionTTL).Unix(), 10) + "." + hex.EncodeToString(b[:])
	return body + "." + k.sign(k.session, body), nil
}

func (k keys) sign(key []byte, body string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// sessionOK reports whether v is a cookie this server signed that has not
// expired.
func (k keys) sessionOK(v string, now time.Time) bool {
	i := strings.LastIndexByte(v, '.')
	if i < 0 || len(v) > 256 {
		return false
	}
	body, mac := v[:i], v[i+1:]
	want := k.sign(k.session, body)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) != 1 {
		return false
	}
	parts := strings.Split(body, ".")
	if len(parts) != 4 || parts[0] != "v2" || parts[1] != k.port {
		return false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	return err == nil && now.Unix() < exp
}

// csrfFor is the form token bound to one session cookie.
func (k keys) csrfFor(session string) string { return k.sign(k.csrf, session) }

// csrfOK compares a submitted form token with the session's.
func (k keys) csrfOK(session, presented string) bool {
	want := k.csrfFor(session)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}
