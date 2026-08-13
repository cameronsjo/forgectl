package cli

import (
	"strings"
	"testing"

	docspkg "github.com/cameronsjo/forgectl/internal/docs"
)

// TestResolveDocsToken_OutputIsPublishable pins a coupling that is invisible at
// the call site and expensive to get wrong.
//
// An off-loopback bind generates a token, and the discovery record then has to
// carry it. NewServerInfo revalidates that token against ITS OWN copy of RFC
// 6750's b64token grammar, and a validation failure there is fatal — the
// command exits rather than serving undiscoverable, because a server that
// cannot mint a generation has no discovery story at all.
//
// So if either grammar drifts, or GenerateToken's encoding changes, every
// exposed `docs serve` stops starting. Nothing else in the suite crosses that
// seam: the token-policy tests stop at resolveDocsToken, and the discovery
// tests supply their own fixtures.
func TestResolveDocsToken_OutputIsPublishable(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:3590", "192.168.1.10:3590", ":3590"} {
		t.Run(addr, func(t *testing.T) {
			resolved, err := resolveDocsToken("", addr)
			if err != nil {
				t.Fatalf("resolveDocsToken(\"\", %q): %v", addr, err)
			}
			if resolved.value == "" {
				t.Fatalf("resolveDocsToken(\"\", %q) produced no token", addr)
			}
			if _, err := docspkg.NewServerInfo("127.0.0.1:3590", resolved.value); err != nil {
				t.Fatalf("a generated token was refused by the discovery record validator: %v — an exposed `docs serve` would fail to start", err)
			}
		})
	}
}

// TestTokenFileValues_ArePublishable covers the other source of a token: the
// operator's --token-file. Anything acquireDocsTokenFile accepts must also
// survive being published, for the same reason.
func TestTokenFileValues_ArePublishable(t *testing.T) {
	// The punctuation-and-padding value the token-file suite already treats as
	// the hardest legal case, plus the boundary length.
	for _, token := range []string{
		"Az09-._~+/===",
		"a",
		strings.Repeat("A", 4096),
	} {
		if _, err := docspkg.NewServerInfo("127.0.0.1:3590", token); err != nil {
			t.Errorf("NewServerInfo refused a token --token-file accepts (%d bytes): %v", len(token), err)
		}
	}
}
