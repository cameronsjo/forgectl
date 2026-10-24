// Package githubapp mints and revokes GitHub App installation tokens for
// drain workers (openspec/changes/worker-github-app).
//
// The App's private key is read from the login keychain, where it is stored
// base64-encoded (`security find-generic-password -w` prints a multi-line
// secret as hex). A token is scoped to one repository and a fixed permission
// set, and expires in an hour.
//
// Every type that holds key or token material renders "[redacted]" from
// String, GoString, Format, LogValue and the marshalers. Errors name the step
// that failed and GitHub's status and message, never a credential.
//
// This is not a boundary: a worker runs as the operator's uid and can read
// the same keychain entry (ADR-0010, 2026-10-20 amendment).
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Redacted is what every key- or token-carrying type renders as.
const Redacted = "[redacted]"

// DefaultBaseURL is the GitHub REST API root.
const DefaultBaseURL = "https://api.github.com"

// SecurityBinary is the keychain tool by absolute path, so a PATH entry
// cannot supply a different key.
const SecurityBinary = "/usr/bin/security"

// keychainTimeout bounds the keychain read: a locked keychain or an ACL
// prompt with nobody at the console never returns on its own.
const keychainTimeout = 15 * time.Second

// requestTimeout bounds each GitHub call.
const requestTimeout = 15 * time.Second

// maxBody caps how much of a GitHub response is read.
const maxBody = 64 << 10

// Permissions is the fixed set every worker token asks for. Asking for less
// than the installation holds is allowed, so a permission later added to the
// App by hand does not widen workers.
var Permissions = map[string]string{
	"contents":      "write",
	"pull_requests": "write",
	"issues":        "read",
	"metadata":      "read",
}

// Errors, one per step, so a caller and a doctor line can say which step broke.
var (
	ErrKeyNotFound  = errors.New("githubapp: no App key in the login keychain")
	ErrKeyMalformed = errors.New("githubapp: keychain entry is not a base64-encoded RSA private key")
	ErrMint         = errors.New("githubapp: minting an installation token failed")
	ErrRevoke       = errors.New("githubapp: revoking an installation token failed")
	ErrBadRepo      = errors.New("githubapp: repository must be owner/name")
	ErrInstallation = errors.New("githubapp: reading the installation as the App failed")
	// ErrIdentityMismatch: GitHub's App and bot do not match the config.
	ErrIdentityMismatch = errors.New("githubapp: the App does not match [surface.identity]")
	// ErrUnreachable: no answer from GitHub (network, proxy, timeout).
	ErrUnreachable = errors.New("GitHub could not be reached")
)

// Key is the App's RSA private key.
type Key struct{ k *rsa.PrivateKey }

func (Key) String() string                { return Redacted }
func (Key) GoString() string              { return Redacted }
func (Key) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (Key) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (Key) MarshalJSON() ([]byte, error)  { return []byte(strconv.Quote(Redacted)), nil }
func (Key) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

// Present reports whether a key was loaded.
func (k Key) Present() bool { return k.k != nil }

// ReadKey reads the base64-encoded PEM key from the keychain service.
func ReadKey(ctx context.Context, runner exec.Runner, service string) (Key, error) {
	ctx, cancel := context.WithTimeout(ctx, keychainTimeout)
	defer cancel()
	out, err := runner.Run(ctx, SecurityBinary, "find-generic-password", "-s", service, "-w")
	if err != nil {
		// err is dropped, not wrapped: exec.CommandError keeps the child's
		// stdout, and this child's stdout is the key.
		return Key{}, fmt.Errorf("%w: service %s", ErrKeyNotFound, termsafe.QuoteArgMax(service, 0))
	}
	key, err := ParseKey(strings.TrimSpace(out))
	if err != nil {
		return Key{}, fmt.Errorf("%w: service %s", err, termsafe.QuoteArgMax(service, 0))
	}
	return key, nil
}

// ParseKey decodes a base64-encoded PEM block holding a PKCS#1 or PKCS#8 RSA
// key. The returned error never quotes the input.
func ParseKey(b64 string) (Key, error) {
	if b64 == "" {
		return Key{}, ErrKeyNotFound
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Key{}, ErrKeyMalformed
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return Key{}, ErrKeyMalformed
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return Key{k: k}, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return Key{}, ErrKeyMalformed
	}
	k, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return Key{}, ErrKeyMalformed
	}
	return Key{k: k}, nil
}

// Token is an installation token. Reveal is the one place its value leaves.
type Token struct {
	reveal    func() string
	ExpiresAt time.Time
}

func (Token) String() string                { return Redacted }
func (Token) GoString() string              { return Redacted }
func (Token) Format(f fmt.State, verb rune) { writeRedacted(f, verb) }
func (Token) LogValue() slog.Value          { return slog.StringValue(Redacted) }
func (Token) MarshalJSON() ([]byte, error)  { return []byte(strconv.Quote(Redacted)), nil }
func (Token) MarshalText() ([]byte, error)  { return []byte(Redacted), nil }

// Present reports whether the token holds a value.
func (t Token) Present() bool { return t.reveal != nil && t.reveal() != "" }

// Reveal returns the raw token. Its only callers write it to the worker's
// private gh config file or set it on an Authorization header.
func (t Token) Reveal() string {
	if t.reveal == nil {
		return ""
	}
	return t.reveal()
}

func writeRedacted(f fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprint(f, strconv.Quote(Redacted))
		return
	}
	_, _ = fmt.Fprint(f, Redacted)
}

// Client talks to GitHub as the App.
type Client struct {
	AppID   int64
	Key     Key
	BaseURL string           // DefaultBaseURL when empty; only tests set another
	HTTP    *http.Client     // nil refuses redirects; only tests set one
	Now     func() time.Time // time.Now when nil
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// A redirect would resend the Authorization header somewhere else.
	return &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// JWT returns the App's signed JSON Web Token: RS256, issued 60 s in the
// past to absorb clock skew, expiring 9 minutes out (GitHub's cap is 10).
func (c *Client) JWT() (string, error) {
	if !c.Key.Present() {
		return "", ErrKeyNotFound
	}
	now := c.now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	// termsafe:allow-raw-json JWT claims sent to GitHub, never command output
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(c.AppID, 10),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.Key.k, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("githubapp: signing the App JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Mint creates an installation token for one repository ("owner/name") with
// Permissions. GitHub resolves the name inside the installation's account,
// so Mint checks the repository GitHub scoped the token to and revokes a
// token scoped anywhere else, or one with no readable expiry.
func (c *Client) Mint(ctx context.Context, installationID int64, repo string) (Token, error) {
	_, name, ok := strings.Cut(repo, "/")
	if !ok || name == "" || strings.Contains(name, "/") || strings.HasPrefix(repo, "/") {
		return Token{}, fmt.Errorf("%w: %s", ErrBadRepo, termsafe.QuoteArgMax(repo, 0))
	}
	jwt, err := c.JWT()
	if err != nil {
		return Token{}, fmt.Errorf("%w: %w", ErrMint, err)
	}
	// termsafe:allow-raw-json request body sent to GitHub, never command output
	body, err := json.Marshal(map[string]any{
		"repositories": []string{name},
		"permissions":  Permissions,
	})
	if err != nil {
		return Token{}, fmt.Errorf("%w: %w", ErrMint, err)
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.base(), installationID)
	data, err := c.do(ctx, http.MethodPost, url, "Bearer "+jwt, body, http.StatusCreated)
	if err != nil {
		return Token{}, fmt.Errorf("%w: %w", ErrMint, err)
	}
	var out struct {
		Token        string `json:"token"`
		ExpiresAt    string `json:"expires_at"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		return Token{}, fmt.Errorf("%w: GitHub's response held no token", ErrMint)
	}
	v := out.Token
	tok := Token{reveal: func() string { return v }}
	expires, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		c.revokeQuietly(ctx, tok)
		return Token{}, fmt.Errorf("%w: GitHub's response held no readable expiry; the token was revoked", ErrMint)
	}
	tok.ExpiresAt = expires
	if len(out.Repositories) != 1 || !strings.EqualFold(out.Repositories[0].FullName, repo) {
		got := "no repository"
		if len(out.Repositories) > 0 {
			got = termsafe.QuoteArgMax(out.Repositories[0].FullName, 0)
		}
		c.revokeQuietly(ctx, tok)
		return Token{}, fmt.Errorf("%w: GitHub scoped the token to %s, not %s; the token was revoked (is the repository in this installation's account?)",
			ErrMint, got, termsafe.QuoteArgMax(repo, 0))
	}
	return tok, nil
}

// revokeQuietly revokes a token Mint will not hand out. A failure is logged;
// the token dies within the hour anyway.
func (c *Client) revokeQuietly(ctx context.Context, t Token) {
	if err := c.Revoke(ctx, t); err != nil {
		slog.Warn("githubapp: could not revoke a refused token", "err", err)
	}
}

// Installation is what CheckInstallation reads: the App's slug and the
// account the installation belongs to.
type Installation struct {
	AppSlug string
	Account string
}

// BotLogin is the login GitHub gives the App's bot user.
func (i Installation) BotLogin() string { return i.AppSlug + "[bot]" }

// CheckInstallation proves the key signs for the App and the installation
// exists, by reading the installation as the App. It mints no token.
func (c *Client) CheckInstallation(ctx context.Context, installationID int64) (Installation, error) {
	jwt, err := c.JWT()
	if err != nil {
		return Installation{}, fmt.Errorf("%w: %w", ErrInstallation, err)
	}
	url := fmt.Sprintf("%s/app/installations/%d", c.base(), installationID)
	data, err := c.do(ctx, http.MethodGet, url, "Bearer "+jwt, nil, http.StatusOK)
	if err != nil {
		return Installation{}, fmt.Errorf("%w: %w", ErrInstallation, err)
	}
	var out struct {
		AppSlug string `json:"app_slug"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.AppSlug == "" {
		return Installation{}, fmt.Errorf("%w: GitHub's response named no App", ErrInstallation)
	}
	return Installation{AppSlug: out.AppSlug, Account: out.Account.Login}, nil
}

// BotUserID reads the user id of the App's bot ("<slug>[bot]"). The users
// endpoint is public, so the call carries no credential.
func (c *Client) BotUserID(ctx context.Context, slug string) (int64, error) {
	if !validSlug(slug) {
		return 0, fmt.Errorf("%w: %s is not an App slug", ErrInstallation, termsafe.QuoteArgMax(slug, 0))
	}
	data, err := c.do(ctx, http.MethodGet, c.base()+"/users/"+slug+"%5Bbot%5D", "", nil, http.StatusOK)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInstallation, err)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.ID <= 0 {
		return 0, fmt.Errorf("%w: GitHub's response named no user id", ErrInstallation)
	}
	return out.ID, nil
}

// VerifyIdentity checks that the key, the App, the installation and the
// configured bot login and user id all agree. It mints no token.
func (c *Client) VerifyIdentity(ctx context.Context, installationID int64, botLogin string, botUserID int64) (Installation, error) {
	inst, err := c.CheckInstallation(ctx, installationID)
	if err != nil {
		return Installation{}, err
	}
	if !strings.EqualFold(inst.BotLogin(), botLogin) {
		return inst, fmt.Errorf("%w: the App's bot is %s, config says %s", ErrIdentityMismatch,
			termsafe.QuoteArgMax(inst.BotLogin(), 0), termsafe.QuoteArgMax(botLogin, 0))
	}
	id, err := c.BotUserID(ctx, inst.AppSlug)
	if err != nil {
		return inst, err
	}
	if id != botUserID {
		return inst, fmt.Errorf("%w: %s has user id %d, config says %d", ErrIdentityMismatch,
			termsafe.QuoteArgMax(inst.BotLogin(), 0), id, botUserID)
	}
	return inst, nil
}

func validSlug(s string) bool {
	if s == "" || len(s) > 100 || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// Revoke deletes an installation token using the token itself. A 401 means
// the token is already dead, which is what revoking wants.
func (c *Client) Revoke(ctx context.Context, t Token) error {
	if !t.Present() {
		return nil
	}
	_, err := c.do(ctx, http.MethodDelete, c.base()+"/installation/token", "token "+t.Reveal(), nil, http.StatusNoContent)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusUnauthorized {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRevoke, err)
	}
	return nil
}

// StatusError is a GitHub answer other than the one a call wanted.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub answered %d", e.Code)
	}
	return fmt.Sprintf("GitHub answered %d: %s", e.Code, termsafe.QuoteArgMax(e.Message, 200))
}

// do sends one request and returns the body when the status is want. auth
// is the Authorization value, or empty for none. A transport failure wraps
// ErrUnreachable; any other status is a *StatusError.
func (c *Client) do(ctx context.Context, method, url, auth string, body []byte, want int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		slog.Debug("githubapp: request failed", "method", method, "err", requestErr(err))
		return nil, ErrUnreachable
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != want {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &msg)
		return nil, &StatusError{Code: resp.StatusCode, Message: msg.Message}
	}
	if readErr != nil {
		slog.Debug("githubapp: reading the response failed", "method", method, "err", readErr)
		return nil, ErrUnreachable
	}
	return data, nil
}

// requestErr is a transport failure's inner error, for the debug log only.
func requestErr(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap().Error()
	}
	return err.Error()
}
