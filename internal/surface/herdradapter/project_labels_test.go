package herdradapter

import (
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

func TestProjectLabelRoundTripsHumanNameAndMarker(test *testing.T) {
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	for _, human := range []string{"Codex + Pi", "Codex + Hermes", "项目"} {
		label, err := projectLabelFor(human, tag.OwnershipName())
		if err != nil {
			test.Fatalf("projectLabelFor(%q): %v", human, err)
		}
		parsed, err := parseProjectLabel(label)
		if err != nil {
			test.Fatalf("parseProjectLabel(%q): %v", label, err)
		}
		if parsed.human != human || parsed.marker != tag.OwnershipName() {
			test.Fatalf("parsed = %#v, want human %q marker %q", parsed, human, tag.OwnershipName())
		}
	}
}

func TestProjectLabelAcceptsBareLegacyMarker(test *testing.T) {
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	parsed, err := parseProjectLabel(tag.OwnershipName())
	if err != nil {
		test.Fatalf("parseProjectLabel: %v", err)
	}
	if parsed.human != "" || parsed.marker != tag.OwnershipName() {
		test.Fatalf("parsed = %#v, want bare marker", parsed)
	}
}

func TestProjectLabelRejectsMalformedSuffixesAndPrefixes(test *testing.T) {
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	marker := tag.OwnershipName()
	valid, err := projectLabelFor("Project", marker)
	if err != nil {
		test.Fatal(err)
	}
	otherTag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	cases := []string{
		"Project",
		"Project " + marker,
		"Project · " + marker + " · " + marker,
		"Project · " + marker + "-forged",
		"Project\x1b · " + marker,
		"Project\u202e · " + marker,
		" · " + marker,
		valid + " · " + marker,
		"Project · FC-surface-" + marker[len("fc-surface-"):],
	}
	for _, input := range cases {
		test.Run(strings.ReplaceAll(input, "\x1b", "esc"), func(test *testing.T) {
			if _, err := parseProjectLabel(input); !errors.Is(err, ErrProjectLabel) {
				test.Fatalf("parseProjectLabel(%q) err = %v, want ErrProjectLabel", input, err)
			}
		})
	}
	changed, err := projectLabelFor("Project", otherTag.OwnershipName())
	if err != nil {
		test.Fatal(err)
	}
	if projectLabelMatches(changed, marker) {
		test.Fatal("projectLabelMatches accepted a changed marker suffix")
	}
}

func TestProjectLabelMatchingRequiresExactMarker(test *testing.T) {
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	other, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	label, err := projectLabelFor("Project", tag.OwnershipName())
	if err != nil {
		test.Fatal(err)
	}
	if !projectLabelMatches(label, tag.OwnershipName()) || projectLabelMatches(label, other.OwnershipName()) {
		test.Fatal("project label matching did not require the exact marker")
	}
}

func TestProjectLabelForBoundsHumanPrefix(test *testing.T) {
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := projectLabelFor(strings.Repeat("x", MaxTokenValueLen+1), tag.OwnershipName()); !errors.Is(err, ErrBadToken) {
		test.Fatalf("projectLabelFor overlong human prefix err = %v, want ErrBadToken", err)
	}
}
