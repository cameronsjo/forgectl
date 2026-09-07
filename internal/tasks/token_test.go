package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

const fakeToken = "tk_" + "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

func TestReadToken_NeverAppearsInArgv(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return fakeToken + "\n", nil
		},
	}
	tok, err := ReadToken(context.Background(), runner, DefaultKeychainService)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if !tok.Present() {
		t.Fatal("ReadToken: token not present")
	}
	for _, call := range runner.Calls {
		for _, a := range call.Args {
			if strings.Contains(a, fakeToken) {
				t.Fatalf("token literal appeared in argv: %v", call.Args)
			}
		}
	}
}

func TestReadToken_RejectsMalformedValue(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "not-a-real-token", nil },
	}
	_, err := ReadToken(context.Background(), runner, DefaultKeychainService)
	if !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("ReadToken(malformed) = %v, want errors.Is(ErrTokenMalformed)", err)
	}
}

func TestReadToken_NotFound(t *testing.T) {
	runner := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return "", fmt.Errorf("security: item not found")
		},
	}
	_, err := ReadToken(context.Background(), runner, DefaultKeychainService)
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("ReadToken(not found) = %v, want errors.Is(ErrTokenNotFound)", err)
	}
}

// TestToken_RedactsUnderEveryRenderingPath is the load-bearing redaction
// test: every rendering surface a Token could reach must produce the fixed
// Redacted string, never the payload.
func TestToken_RedactsUnderEveryRenderingPath(t *testing.T) {
	tok := newToken(fakeToken)

	checks := map[string]string{
		"String":      tok.String(),
		"GoString":    tok.GoString(),
		"Sprintf %v":  fmt.Sprintf("%v", tok),
		"Sprintf %+v": fmt.Sprintf("%+v", tok),
		"Sprintf %#v": fmt.Sprintf("%#v", tok),
	}
	for name, rendered := range checks {
		if strings.Contains(rendered, fakeToken) {
			t.Errorf("%s leaked the token: %q", name, rendered)
		}
		if rendered != Redacted && !strings.Contains(rendered, Redacted) {
			t.Errorf("%s = %q, want it to render %q", name, rendered, Redacted)
		}
	}

	jsonBytes, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("json.Marshal(Token): %v", err)
	}
	if strings.Contains(string(jsonBytes), fakeToken) {
		t.Fatalf("json.Marshal(Token) leaked the token: %s", jsonBytes)
	}

	logValue := tok.LogValue()
	if strings.Contains(logValue.String(), fakeToken) {
		t.Fatalf("LogValue() leaked the token: %s", logValue.String())
	}

	// The one sanctioned reveal point must still carry the real value.
	if want := "Bearer " + fakeToken; tok.Header() != want {
		t.Fatalf("Header() = %q, want %q", tok.Header(), want)
	}
}
