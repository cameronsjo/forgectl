package pr

import (
	"regexp"
	"strings"
	"testing"
)

// reEncodedName is the grammar of the whole namespace, asserted independently
// of the code that builds it.
var reEncodedName = regexp.MustCompile(`^pr-v2-[lr]-[a-z0-9-]{1,55}-[a-z2-7]{32}$`)

func TestSanitizeSessionLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already clean", "cameronsjo-forgectl-218", "cameronsjo-forgectl-218"},
		{"uppercase folds", "CameronSjo-ForgeCtl", "cameronsjo-forgectl"},
		{"dots become hyphens", "owner-repo.name-1", "owner-repo-name-1"},
		{"runs collapse", "a...___   b", "a-b"},
		{"trims edges", "--owner--", "owner"},
		{"empty becomes x", "", "x"},
		{"all separators become x", "...", "x"},
		{"controls become a separator", "a\x00\x1fb", "a-b"},
		{"non-ascii becomes a separator", "häx", "h-x"},
		{"truncates to 55 bytes", strings.Repeat("a", 80), strings.Repeat("a", 55)},
		{
			"truncation never leaves a trailing hyphen",
			strings.Repeat("a", 54) + "-bbbb",
			strings.Repeat("a", 54),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeSessionLabel(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeSessionLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeSessionLabelAlwaysProducesALegalLabel(t *testing.T) {
	reLabel := regexp.MustCompile(`^[a-z0-9-]{1,55}$`)
	inputs := []string{
		"", "-", "----", "...", "\x00", "\t\n ", strings.Repeat("-a", 60),
		strings.Repeat("ü", 40), "local-0123456-74565", string([]byte{0xff, 0xfe}),
		strings.Repeat("a", 54) + "-" + strings.Repeat("b", 10),
	}
	for _, in := range inputs {
		got := sanitizeSessionLabel(in)
		if !reLabel.MatchString(got) {
			t.Errorf("sanitizeSessionLabel(%q) = %q, outside the label grammar", in, got)
		}
	}
}

func TestEncodedNameGrammarAndBound(t *testing.T) {
	local, err := localSessionKey("0123456")
	if err != nil {
		t.Fatalf("localSessionKey: %v", err)
	}
	remote, err := remoteSessionKey("cameronsjo", "forgectl", 218)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	for _, key := range []prSessionKey{local, remote} {
		for _, role := range []nameRole{roleReview, roleShell} {
			name, err := key.encodedName(strings.Repeat("label-", 20), role)
			if err != nil {
				t.Fatalf("encodedName: %v", err)
			}
			if len(name) > maxPRSessionNameBytes {
				t.Fatalf("name %q is %d bytes, over the %d bound", name, len(name), maxPRSessionNameBytes)
			}
			if !reEncodedName.MatchString(name) {
				t.Fatalf("name %q is outside the grammar", name)
			}
			if !strings.HasPrefix(name, reviewWindowPrefix) {
				t.Fatalf("name %q left the %q namespace the admission gate counts", name, reviewWindowPrefix)
			}
		}
	}
}

func TestEncodedNameReachesTheDeclaredMaximum(t *testing.T) {
	key, err := remoteSessionKey("cameronsjo", "forgectl", 218)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	name, err := key.encodedName(strings.Repeat("a", 80), roleReview)
	if err != nil {
		t.Fatalf("encodedName: %v", err)
	}
	if len(name) != maxPRSessionNameBytes {
		t.Fatalf("longest name is %d bytes, want exactly %d", len(name), maxPRSessionNameBytes)
	}
}

// The digest is what carries identity, so a different key or a different role
// must move it even when the cosmetic label is identical.
func TestEncodedNameDigestSeparatesKeysAndRoles(t *testing.T) {
	local, err := localSessionKey("0123456")
	if err != nil {
		t.Fatalf("localSessionKey: %v", err)
	}
	remote, err := remoteSessionKey("local", "0123456", 0x012345)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	const label = "same-label"
	names := map[string]string{}
	for _, tc := range []struct {
		what string
		key  prSessionKey
		role nameRole
	}{
		{"local-review", local, roleReview},
		{"local-shell", local, roleShell},
		{"remote-review", remote, roleReview},
		{"remote-shell", remote, roleShell},
	} {
		name, err := tc.key.encodedName(label, tc.role)
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		if prior, seen := names[name]; seen {
			t.Fatalf("%s produced the same name as %s: %q", tc.what, prior, name)
		}
		names[name] = tc.what
	}
}

func TestEncodedNameIsDeterministic(t *testing.T) {
	key, err := remoteSessionKey("owner", "repo", 9)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	first, err := key.encodedName("owner-repo-9", roleReview)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := key.encodedName("owner-repo-9", roleReview)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != second {
		t.Fatalf("encodedName is not deterministic: %q vs %q", first, second)
	}
	// Mixed-case spellings of one repository are one key, so one window.
	upper, err := remoteSessionKey("Owner", "Repo", 9)
	if err != nil {
		t.Fatalf("upper: %v", err)
	}
	upperName, err := upper.encodedName("owner-repo-9", roleReview)
	if err != nil {
		t.Fatalf("upper name: %v", err)
	}
	if upperName != first {
		t.Fatalf("mixed-case spelling produced a different window name: %q vs %q", upperName, first)
	}
}

func TestEncodedNameRejectsAnUninitializedKey(t *testing.T) {
	var zero prSessionKey
	if _, err := zero.encodedName("label", roleReview); err == nil {
		t.Fatal("a zero key produced a window name")
	}
}

func TestEncodedNameRejectsInvalidUTF8Label(t *testing.T) {
	key, err := remoteSessionKey("owner", "repo", 1)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	if _, err := key.encodedName(string([]byte{0xff, 0xfe}), roleReview); err == nil {
		t.Fatal("an invalid-UTF-8 label was accepted")
	}
}

// tmux splits a target string on ":" and ".", and treats "$", "@", "%", "="
// as identity or match sigils. None may appear in a generated name.
func TestEncodedNameCarriesNoTmuxTargetGrammar(t *testing.T) {
	key, err := remoteSessionKey("owner", "repo-name", 1)
	if err != nil {
		t.Fatalf("remoteSessionKey: %v", err)
	}
	name, err := key.encodedName("owner-repo.name-1", roleReview)
	if err != nil {
		t.Fatalf("encodedName: %v", err)
	}
	if strings.ContainsAny(name, ":.%$@=") {
		t.Fatalf("name %q carries tmux target grammar", name)
	}
	for i := range len(name) {
		if name[i] < 0x20 || name[i] == 0x7f {
			t.Fatalf("name %q carries a control byte at %d", name, i)
		}
	}
}
