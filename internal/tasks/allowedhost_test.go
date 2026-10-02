package tasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestCheckAllowedHost is the host rule as a table: a keychain credential
// goes to the default host or to a host the user's config lists, compared
// lower-cased and whole, and to nothing else.
func TestCheckAllowedHost(t *testing.T) {
	listed := []string{"board.example", "Second.Example"}
	for _, tc := range []struct {
		name    string
		host    string
		allowed []string
		ok      bool
	}{
		{"the default host with no list", DefaultHost, nil, true},
		{"the default host with a list", DefaultHost, listed, true},
		{"the default host in uppercase", strings.ToUpper(DefaultHost), nil, true},
		{"a listed host", "board.example", listed, true},
		{"a listed host in another case", "BOARD.Example", listed, true},
		{"a host listed in another case", "second.example", listed, true},

		{"an unlisted host", "other.example", listed, false},
		{"an unlisted host with no list", "board.example", nil, false},
		{"an unlisted host with an empty list", "board.example", []string{}, false},
		{"empty", "", listed, false},
		{"the default host with a trailing dot", DefaultHost + ".", nil, false},
		{"a listed host with a trailing dot", "board.example.", listed, false},
		{"the default host with a port", DefaultHost + ":443", nil, false},
		{"a listed host with a port", "board.example:8443", listed, false},
		{"userinfo before the default host", "user@" + DefaultHost, nil, false},
		{"the default host as userinfo", DefaultHost + "@other.example", nil, false},
		{"a path after the default host", DefaultHost + "/api", nil, false},
		{"an IPv4 literal", "192.168.1.102", listed, false},
		{"a listed IPv4 literal", "192.168.1.102", []string{"192.168.1.102"}, false},
		{"an IPv6 literal", "fd00::1", listed, false},
		{"a listed IPv6 literal", "fd00::1", []string{"fd00::1"}, false},
		{"a bracketed IPv6 literal", "[fd00::1]", listed, false},
		{"the default host as a prefix", DefaultHost + ".other.example", nil, false},
		{"the default host as a suffix", "other" + DefaultHost, nil, false},
		{"a subdomain of the default host", "other." + DefaultHost, nil, false},
		{"the default host's parent domain", strings.SplitN(DefaultHost, ".", 2)[1], nil, false},
		{"a listed host as a suffix", "notboard.example", listed, false},
		{"a listed host as a prefix", "board.example.other.example", listed, false},
		{"a host that matches a listed entry carrying a port", "board.example:8443", []string{"board.example:8443"}, false},
		{"a line break", DefaultHost + "\n", nil, false},
	} {
		err := CheckAllowedHost(tc.host, tc.allowed)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: CheckAllowedHost(%q) = %v, want nil", tc.name, tc.host, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: CheckAllowedHost(%q) = nil, want a refusal", tc.name, tc.host)
			continue
		}
		if !IsHostRefused(err) {
			t.Errorf("%s: the refusal does not satisfy IsHostRefused: %v", tc.name, err)
		}
		if !strings.Contains(err.Error(), AllowedHostsConfigKey) {
			t.Errorf("%s: the refusal does not name the config key %q: %v", tc.name, AllowedHostsConfigKey, err)
		}
		if strings.ContainsAny(err.Error(), "\n\x1b") {
			t.Errorf("%s: the refusal carries a raw control character: %q", tc.name, err.Error())
		}
	}
}

func TestAllowedHostsConfigKey_NamesTheSectionAndTheKey(t *testing.T) {
	if !strings.Contains(AllowedHostsConfigKey, "[tasks]") || !strings.Contains(AllowedHostsConfigKey, "allowed_hosts") {
		t.Errorf("AllowedHostsConfigKey = %q, want it to name [tasks] and allowed_hosts", AllowedHostsConfigKey)
	}
}

func TestCheckKeychainService(t *testing.T) {
	for _, name := range []string{
		DefaultKeychainService, DefaultWriteKeychainService, "a", "A.b_c-9", strings.Repeat("x", 64),
	} {
		if err := CheckKeychainService(name); err != nil {
			t.Errorf("CheckKeychainService(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{
		"", strings.Repeat("x", 65), "two words", "a;b", "a/b", "a$b", "a\nb", "a\x1b[2J", "naïve", "a:b", "a'b", `a"b`,
	} {
		err := CheckKeychainService(name)
		if err == nil {
			t.Errorf("CheckKeychainService(%q) = nil, want a refusal", name)
			continue
		}
		if strings.ContainsAny(err.Error(), "\n\x1b") {
			t.Errorf("CheckKeychainService(%q) error carries a raw control character: %q", name, err.Error())
		}
	}
}

func TestDefaultWriteKeychainService_IsNotTheReadEntry(t *testing.T) {
	if DefaultWriteKeychainService != "vikunja-write" {
		t.Errorf("DefaultWriteKeychainService = %q, want vikunja-write", DefaultWriteKeychainService)
	}
	if DefaultWriteKeychainService == DefaultKeychainService {
		t.Error("the write entry and the read entry have the same name")
	}
}

func keychainRunner(calls *[]string) *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, _ []string) (string, error) {
			if calls != nil {
				*calls = append(*calls, name)
			}
			if name == SecurityBinary {
				return fakeToken + "\n", nil
			}
			if name == routeBinary {
				return "gateway: " + HomelabGateway + "\n", nil
			}
			return "", nil
		},
	}
}

// TestReadToken_RemembersWhereItMayGo: the token carries the host set it was
// read under, so the rule travels with the credential and not with the caller.
func TestReadToken_RemembersWhereItMayGo(t *testing.T) {
	ctx := context.Background()

	defaultOnly, err := ReadToken(ctx, keychainRunner(nil), DefaultKeychainService, nil)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if err := defaultOnly.checkHost(DefaultHost); err != nil {
		t.Errorf("a keychain token read with no list may not go to the default host: %v", err)
	}
	if err := defaultOnly.checkHost("board.example"); !IsHostRefused(err) {
		t.Errorf("a keychain token read with no list may go to board.example: %v", err)
	}

	hosts := []string{"board.example"}
	listed, err := ReadToken(ctx, keychainRunner(nil), DefaultKeychainService, hosts)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	if err := listed.checkHost("board.example"); err != nil {
		t.Errorf("a keychain token may not go to a host it was read for: %v", err)
	}
	if err := listed.checkHost(DefaultHost); err != nil {
		t.Errorf("a list must not remove the default host: %v", err)
	}
	if err := listed.checkHost("other.example"); !IsHostRefused(err) {
		t.Errorf("a keychain token may go to an unlisted host: %v", err)
	}

	// The list is the caller's slice. Editing it after the read must not
	// change where an already-issued token may go.
	hosts[0] = "other.example"
	if err := listed.checkHost("other.example"); !IsHostRefused(err) {
		t.Errorf("editing the caller's slice after ReadToken widened the token: %v", err)
	}
	if err := listed.checkHost("board.example"); err != nil {
		t.Errorf("editing the caller's slice after ReadToken narrowed the token: %v", err)
	}
}

// The constructor tests below use "localhost" as the host on purpose. It
// resolves from the hosts file with no network call, and the pin then refuses
// the loopback address — so a client is never built, and which refusal comes
// back says how far the constructor got. The host rule's refusal names the
// config key and runs nothing; the pin's refusal says "loopback" and has
// already asked for the default gateway.

func TestNewClient_RefusesAKeychainTokenForAnUnlistedHost(t *testing.T) {
	ctx := context.Background()
	var calls []string
	runner := keychainRunner(&calls)
	token, err := ReadToken(ctx, runner, DefaultKeychainService, nil)
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	calls = nil

	for name, build := range map[string]func() (*Client, error){
		"NewClient":         func() (*Client, error) { return NewClient(ctx, runner, "localhost", token) },
		"NewClientWithPins": func() (*Client, error) { return NewClientWithPins(ctx, runner, "localhost", token, nil) },
	} {
		client, err := build()
		if client != nil {
			t.Fatalf("%s built a client for a host the token may not go to", name)
		}
		if !errors.Is(err, ErrHostRefused) {
			t.Fatalf("%s = %v, want ErrHostRefused", name, err)
		}
		if !strings.Contains(err.Error(), AllowedHostsConfigKey) {
			t.Errorf("%s was refused by something other than the host rule: %v", name, err)
		}
		if strings.Contains(err.Error(), fakeToken) {
			t.Errorf("%s: the refusal carries the token", name)
		}
	}
	if len(calls) != 0 {
		t.Errorf("the constructor ran %v before refusing; the host rule must come before any lookup", calls)
	}
}

func TestNewClient_AcceptsAKeychainTokenForAListedHost(t *testing.T) {
	ctx := context.Background()
	var calls []string
	runner := keychainRunner(&calls)
	token, err := ReadToken(ctx, runner, DefaultKeychainService, []string{"localhost"})
	if err != nil {
		t.Fatalf("ReadToken: %v", err)
	}
	calls = nil

	_, err = NewClient(ctx, runner, "localhost", token)
	if err == nil {
		t.Fatal("NewClient built a client for a loopback address; the pin should have refused it")
	}
	if strings.Contains(err.Error(), AllowedHostsConfigKey) {
		t.Fatalf("the host rule refused a listed host: %v", err)
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("NewClient = %v, want the pin's loopback refusal, which is only reached past the host rule", err)
	}
	if len(calls) != 1 || calls[0] != routeBinary {
		t.Errorf("runner calls = %v, want the one gateway lookup the pin makes", calls)
	}
}

// TestNewClient_ATokenFileTokenHasNoHostRule: the container transport's token
// comes from a mounted file and is governed by the required pin list, not by
// the user's config file.
func TestNewClient_ATokenFileTokenHasNoHostRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := ReadTokenFile(path)
	if err != nil {
		t.Fatalf("ReadTokenFile: %v", err)
	}
	for _, host := range []string{"localhost", "other.example", "192.168.1.102"} {
		if err := token.checkHost(host); err != nil {
			t.Errorf("a token-file token is restricted from %s: %v", host, err)
		}
	}

	var calls []string
	_, err = NewClient(context.Background(), keychainRunner(&calls), "localhost", token)
	if err == nil || strings.Contains(err.Error(), AllowedHostsConfigKey) || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("NewClient = %v, want the pin's loopback refusal and not the host rule's", err)
	}
}

func TestValidateEvidence_IsTheTrailersOwnCheck(t *testing.T) {
	if err := ValidateEvidence("merged owner/repo#12"); err != nil {
		t.Errorf("ValidateEvidence(plain text) = %v, want nil", err)
	}
	for name, evidence := range map[string]string{
		"blank":           "   ",
		"a line break":    "one\ntwo",
		"a control":       "one\x1b[2Jtwo",
		"over the limit":  strings.Repeat("x", maxEvidenceRunes+1),
		"a token":         "see " + fakeToken,
		"invalid UTF-8":   "one\xfftwo",
		"a zero-width":    "one" + string(rune(0x200b)) + "two",
		"a bidi override": "one" + string(rune(0x202e)) + "two",
	} {
		err := ValidateEvidence(evidence)
		if err == nil {
			t.Errorf("ValidateEvidence(%s) = nil, want a refusal", name)
			continue
		}
		_, want := sanitizeEvidence(evidence)
		if want == nil || err.Error() != want.Error() {
			t.Errorf("ValidateEvidence(%s) = %v, want the trailer sanitizer's own refusal %v", name, err, want)
		}
		if strings.Contains(err.Error(), fakeToken) {
			t.Errorf("ValidateEvidence(%s) echoes the token", name)
		}
	}
}
