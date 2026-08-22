package proxy

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

func TestUseRendersFixedCasePairedBatch(t *testing.T) {
	got, err := Use(config.ProxyProfile{
		HTTPProxy:  "http://proxy.example:8080",
		HTTPSProxy: "https://proxy.example:8443",
		NoProxy:    "localhost,127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Use: %v", err)
	}
	want := "export HTTP_PROXY='http://proxy.example:8080' http_proxy='http://proxy.example:8080'; " +
		"export HTTPS_PROXY='https://proxy.example:8443' https_proxy='https://proxy.example:8443'; " +
		"unset ALL_PROXY all_proxy; " +
		"export NO_PROXY='localhost,127.0.0.1' no_proxy='localhost,127.0.0.1'"
	if got != want {
		t.Fatalf("script:\n got: %s\nwant: %s", got, want)
	}
}

func TestUseHostileValuesRoundTripWithoutExecution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "injected")
	values := []string{
		"http://user:p'ass@proxy.example/$HOME;$(touch " + marker + ")",
		"`touch " + marker + "`\nsecond-line",
		"socks5://proxy.example:1080 && touch " + marker,
		"localhost,*.example,!history\\literal",
	}
	script, err := Use(config.ProxyProfile{
		HTTPProxy: values[0], HTTPSProxy: values[1], AllProxy: values[2], NoProxy: values[3],
	})
	if err != nil {
		t.Fatalf("Use: %v", err)
	}

	// Evaluate the generated protocol in a real POSIX shell, comparing every
	// result against an argv-carried oracle. The marker asserts that command
	// substitutions, backticks, separators, and newlines stayed inert.
	check := script + `; ` +
		`[ "$HTTP_PROXY" = "$1" ] && [ "$http_proxy" = "$1" ] && ` +
		`[ "$HTTPS_PROXY" = "$2" ] && [ "$https_proxy" = "$2" ] && ` +
		`[ "$ALL_PROXY" = "$3" ] && [ "$all_proxy" = "$3" ] && ` +
		`[ "$NO_PROXY" = "$4" ] && [ "$no_proxy" = "$4" ] && ` +
		`[ ! -e "$5" ]`
	// #nosec G204 -- evaluating the generated shell source is the security property under test.
	cmd := exec.CommandContext(context.Background(), "sh", "-c", check, "sh", values[0], values[1], values[2], values[3], marker)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("evaluate generated script: %v\noutput: %s\nscript: %s", err, out, script)
	}
}

func TestUseHostileValuesRoundTripInZsh(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	marker := filepath.Join(t.TempDir(), "zsh-injected")
	value := "http://user:p'ass@proxy.example/!history;$(touch " + marker + ")\nline-two\\tail"
	script, err := Use(config.ProxyProfile{HTTPProxy: value})
	if err != nil {
		t.Fatalf("Use: %v", err)
	}
	check := script + `; [ "$HTTP_PROXY" = "$1" ] && [ "$http_proxy" = "$1" ] && [ ! -e "$2" ]`
	// #nosec G204 -- evaluating the generated shell source is the security property under test.
	cmd := exec.CommandContext(context.Background(), zsh, "-c", check, "zsh", value, marker)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("evaluate generated script in zsh: %v\noutput: %s\nscript: %s", err, out, script)
	}
}

func TestOffUnsetsEverySupportedSpelling(t *testing.T) {
	check := Off() + `; ` +
		`[ -z "${HTTP_PROXY+x}${http_proxy+x}${HTTPS_PROXY+x}${https_proxy+x}` +
		`${ALL_PROXY+x}${all_proxy+x}${NO_PROXY+x}${no_proxy+x}" ]`
	// #nosec G204 -- evaluating the generated fixed shell source is the property under test.
	cmd := exec.CommandContext(context.Background(), "sh", "-c", check)
	cmd.Env = append(cmd.Environ(),
		"HTTP_PROXY=x", "http_proxy=x", "HTTPS_PROXY=x", "https_proxy=x",
		"ALL_PROXY=x", "all_proxy=x", "NO_PROXY=x", "no_proxy=x",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("evaluate off script: %v\noutput: %s", err, out)
	}
}

func TestUseRefusesEmptyAndNULWithoutLeakingValue(t *testing.T) {
	if got, err := Use(config.ProxyProfile{}); got != "" || !errors.Is(err, ErrEmptyProfile) {
		t.Fatalf("empty profile = (%q, %v), want empty script + ErrEmptyProfile", got, err)
	}

	const opaqueValue = "do-not-leak"
	got, err := Use(config.ProxyProfile{HTTPProxy: opaqueValue + "\x00tail"})
	if got != "" || !errors.Is(err, ErrUnrepresentable) {
		t.Fatalf("NUL profile = (%q, %v), want empty script + ErrUnrepresentable", got, err)
	}
	if strings.Contains(err.Error(), opaqueValue) {
		t.Fatalf("error leaked proxy value: %q", err)
	}
}
