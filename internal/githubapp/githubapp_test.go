package githubapp

import (
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
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

const fakeTokenValue = "ghs_" + "testtokentesttokentesttokentest12"

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func b64PKCS1(t *testing.T) string {
	k := rsaKey(t)
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return base64.StdEncoding.EncodeToString(p)
}

func b64PKCS8(t *testing.T) string {
	k := rsaKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestParseKey(t *testing.T) {
	for name, in := range map[string]string{"pkcs1": b64PKCS1(t), "pkcs8": b64PKCS8(t)} {
		k, err := ParseKey(in)
		if err != nil || !k.Present() {
			t.Fatalf("%s: ParseKey = %v", name, err)
		}
	}
	raw := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey(t))}))
	for name, in := range map[string]string{
		"empty":       "",
		"not base64":  "!!!",
		"raw pem":     raw,
		"no pem":      base64.StdEncoding.EncodeToString([]byte("hello")),
		"not rsa der": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})),
	} {
		_, err := ParseKey(in)
		if err == nil {
			t.Fatalf("%s: ParseKey accepted it", name)
		}
		if in != "" && strings.Contains(err.Error(), in) {
			t.Fatalf("%s: error quotes the input", name)
		}
	}
}

func TestReadKey(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		gotName, gotArgs = name, args
		return b64PKCS1(t) + "\n", nil
	}}
	k, err := ReadKey(context.Background(), runner, "forgectl-worker-app")
	if err != nil || !k.Present() {
		t.Fatalf("ReadKey = %v", err)
	}
	if gotName != SecurityBinary || strings.Join(gotArgs, " ") != "find-generic-password -s forgectl-worker-app -w" {
		t.Fatalf("ran %s %v", gotName, gotArgs)
	}

	secret := b64PKCS1(t)
	failing := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return secret, errors.New("exit 44 " + secret)
	}}
	_, err = ReadKey(context.Background(), failing, "svc")
	if !errors.Is(err, ErrKeyNotFound) || strings.Contains(err.Error(), secret) {
		t.Fatalf("failed read: %v", err)
	}
}

func TestRedaction(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	tok := Token{reveal: func() string { return fakeTokenValue }, ExpiresAt: time.Unix(0, 0)}
	var logBuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	logger.Info("x", "key", k, "token", tok)
	j, _ := json.Marshal(map[string]any{"k": k, "t": tok})
	outs := []string{
		fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %q %s", k, k, k, k, k),
		fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %q %s", tok, tok, tok, tok, tok),
		string(j), logBuf.String(),
	}
	for _, o := range outs {
		if strings.Contains(o, fakeTokenValue) || strings.Contains(o, "PRIVATE") ||
			strings.Contains(o, k.k.D.String()) || strings.Contains(o, b64PKCS1(t)[40:80]) {
			t.Fatalf("leaked: %s", o)
		}
		if !strings.Contains(o, Redacted) {
			t.Fatalf("not redacted: %s", o)
		}
	}
}

func TestJWT(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	now := time.Unix(1_800_000_000, 0)
	c := &Client{AppID: 42, Key: k, Now: func() time.Time { return now }}
	jwt, err := c.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "42" || claims.Iat != now.Unix()-60 || claims.Exp != now.Add(9*time.Minute).Unix() {
		t.Fatalf("claims = %+v", claims)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&rsaKey(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}

	if _, err := (&Client{AppID: 1}).JWT(); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("no key: %v", err)
	}
}

func TestMint(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/77/access_tokens" {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, "no jwt", http.StatusUnauthorized)
			return
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":"2026-10-24T05:00:00Z","repositories":[{"full_name":"cameronsjo/forgectl"}]}`, fakeTokenValue)
	}))
	defer srv.Close()

	c := &Client{AppID: 1, Key: k, BaseURL: srv.URL}
	tok, err := c.Mint(context.Background(), 77, "cameronsjo/forgectl")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Reveal() != fakeTokenValue || !tok.ExpiresAt.Equal(time.Date(2026, 10, 24, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("token %v expires %v", tok.Present(), tok.ExpiresAt)
	}
	repos, _ := body["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "forgectl" {
		t.Fatalf("repositories = %v", body["repositories"])
	}
	perms, _ := body["permissions"].(map[string]any)
	want := map[string]string{"contents": "write", "pull_requests": "write", "issues": "read", "metadata": "read"}
	if len(perms) != len(want) {
		t.Fatalf("permissions = %v", perms)
	}
	for p, v := range want {
		if perms[p] != v {
			t.Fatalf("permissions[%s] = %v", p, perms[p])
		}
	}
}

func TestMintRefusals(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"There is at least one repository that does not exist"}`))
	}))
	defer srv.Close()
	c := &Client{AppID: 1, Key: k, BaseURL: srv.URL}
	_, err := c.Mint(context.Background(), 1, "cameronsjo/nope")
	if !errors.Is(err, ErrMint) || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range []string{"forgectl", "a/b/c", "/x", "owner/"} {
		if _, err := c.Mint(context.Background(), 1, bad); !errors.Is(err, ErrBadRepo) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestMintRefusesRedirect(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := &Client{AppID: 1, Key: k, BaseURL: srv.URL}
	if _, err := c.Mint(context.Background(), 1, "o/r"); !errors.Is(err, ErrMint) {
		t.Fatalf("err = %v", err)
	}
	if followed {
		t.Fatal("followed a redirect with the App JWT")
	}
}

func TestRevoke(t *testing.T) {
	var auth string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/installation/token" {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		auth = r.Header.Get("Authorization")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	tok := Token{reveal: func() string { return fakeTokenValue }}
	if err := c.Revoke(context.Background(), tok); err != nil || auth != "token "+fakeTokenValue {
		t.Fatalf("revoke: %v, auth %q", err, auth)
	}
	status = http.StatusUnauthorized
	if err := c.Revoke(context.Background(), tok); err != nil {
		t.Fatalf("already-dead token: %v", err)
	}
	status = http.StatusInternalServerError
	err := c.Revoke(context.Background(), tok)
	if !errors.Is(err, ErrRevoke) || strings.Contains(err.Error(), fakeTokenValue) {
		t.Fatalf("500: %v", err)
	}
	if err := c.Revoke(context.Background(), Token{}); err != nil {
		t.Fatalf("empty token: %v", err)
	}
}

func TestCheckInstallation(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations/5" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			_, _ = w.Write([]byte(`{"id":5,"app_slug":"forgectl-worker","account":{"login":"cameronsjo"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()
	c := &Client{AppID: 1, Key: k, BaseURL: srv.URL}
	inst, err := c.CheckInstallation(context.Background(), 5)
	if err != nil || inst.BotLogin() != "forgectl-worker[bot]" || inst.Account != "cameronsjo" {
		t.Fatalf("%+v, %v", inst, err)
	}
	_, err = c.CheckInstallation(context.Background(), 6)
	var se *StatusError
	if !errors.Is(err, ErrInstallation) || !errors.As(err, &se) || se.Code != 404 || errors.Is(err, ErrMint) {
		t.Fatalf("missing installation: %v", err)
	}
}

// identityServer answers the installation and the bot user lookups.
func identityServer(t *testing.T, slug string, botID int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/5":
			_, _ = fmt.Fprintf(w, `{"app_slug":%q,"account":{"login":"cameronsjo"}}`, slug)
		case r.URL.RawPath == "/users/"+slug+"%5Bbot%5D" || r.URL.Path == "/users/"+slug+"[bot]":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("the public users call carried a credential")
			}
			_, _ = fmt.Fprintf(w, `{"id":%d}`, botID)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestVerifyIdentity(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	srv := identityServer(t, "forgectl-worker", 99)
	defer srv.Close()
	c := &Client{AppID: 1, Key: k, BaseURL: srv.URL}
	ctx := context.Background()
	if _, err := c.VerifyIdentity(ctx, 5, "forgectl-worker[bot]", 99); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VerifyIdentity(ctx, 5, "other[bot]", 99); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong login: %v", err)
	}
	if _, err := c.VerifyIdentity(ctx, 5, "forgectl-worker[bot]", 100); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong id: %v", err)
	}
	if _, err := c.BotUserID(ctx, "../x"); !errors.Is(err, ErrInstallation) {
		t.Fatalf("bad slug: %v", err)
	}
}

func TestUnreachable(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{AppID: 1, Key: k, BaseURL: url}
	if _, err := c.CheckInstallation(context.Background(), 5); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("closed server: %v", err)
	}
}

// mintServer answers a mint with body and records whether a revoke followed.
func mintServer(body string, revoked *bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/installation/token" {
			*revoked = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(body))
	}))
}

func TestMintRevokesWrongScope(t *testing.T) {
	k, _ := ParseKey(b64PKCS1(t))
	for name, body := range map[string]string{
		"other owner": `{"token":"` + fakeTokenValue + `","expires_at":"2026-10-24T05:00:00Z","repositories":[{"full_name":"someoneelse/forgectl"}]}`,
		"no repos":    `{"token":"` + fakeTokenValue + `","expires_at":"2026-10-24T05:00:00Z"}`,
		"two repos":   `{"token":"` + fakeTokenValue + `","expires_at":"2026-10-24T05:00:00Z","repositories":[{"full_name":"cameronsjo/forgectl"},{"full_name":"cameronsjo/cadence"}]}`,
		"no expiry":   `{"token":"` + fakeTokenValue + `","repositories":[{"full_name":"cameronsjo/forgectl"}]}`,
		"bad expiry":  `{"token":"` + fakeTokenValue + `","expires_at":"soon","repositories":[{"full_name":"cameronsjo/forgectl"}]}`,
	} {
		var revoked bool
		srv := mintServer(body, &revoked)
		c := &Client{AppID: 1, Key: k, BaseURL: srv.URL}
		tok, err := c.Mint(context.Background(), 1, "cameronsjo/forgectl")
		srv.Close()
		if !errors.Is(err, ErrMint) || tok.Present() {
			t.Errorf("%s: handed out a token: %v", name, err)
		}
		if !revoked {
			t.Errorf("%s: the refused token was not revoked", name)
		}
		if err != nil && strings.Contains(err.Error(), fakeTokenValue) {
			t.Errorf("%s: error carries the token", name)
		}
	}
}
