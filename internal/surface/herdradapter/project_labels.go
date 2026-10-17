package herdradapter

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

const (
	projectLabelSeparator  = " · "
	projectOwnershipPrefix = "fc-surface-"
)

var ErrProjectLabel = errors.New("herdradapter: invalid project label")

type projectLabel struct {
	human  string
	marker string
}

func projectLabelFor(human, marker string) (string, error) {
	if err := CheckTokenValue("project label", human); err != nil {
		return "", err
	}
	if strings.Contains(human, projectOwnershipPrefix) || strings.Contains(human, projectLabelSeparator) {
		return "", fmt.Errorf("%w: human prefix contains ownership syntax", ErrProjectLabel)
	}
	parsed, err := parseProjectLabel(marker)
	if err != nil || parsed.marker != marker {
		return "", fmt.Errorf("%w: marker is not exact", ErrProjectLabel)
	}
	return human + projectLabelSeparator + marker, nil
}

func parseProjectLabel(label string) (projectLabel, error) {
	if label == "" {
		return projectLabel{}, fmt.Errorf("%w: label is empty", ErrProjectLabel)
	}
	if strings.HasPrefix(label, projectOwnershipPrefix) {
		marker, err := projectMarker(label)
		if err != nil || marker != label {
			return projectLabel{}, fmt.Errorf("%w: malformed legacy marker", ErrProjectLabel)
		}
		return projectLabel{marker: marker}, nil
	}
	if strings.Count(label, projectLabelSeparator) != 1 {
		return projectLabel{}, fmt.Errorf("%w: label must have one exact separator", ErrProjectLabel)
	}
	human, marker, _ := strings.Cut(label, projectLabelSeparator)
	if err := CheckTokenValue("project label", human); err != nil {
		return projectLabel{}, fmt.Errorf("%w: human prefix: %v", ErrProjectLabel, err)
	}
	if strings.Contains(human, projectOwnershipPrefix) {
		return projectLabel{}, fmt.Errorf("%w: human prefix contains ownership syntax", ErrProjectLabel)
	}
	parsedMarker, err := projectMarker(marker)
	if err != nil || parsedMarker != marker {
		return projectLabel{}, fmt.Errorf("%w: malformed marker suffix", ErrProjectLabel)
	}
	return projectLabel{human: human, marker: marker}, nil
}

func projectMarker(label string) (string, error) {
	if !strings.HasPrefix(label, projectOwnershipPrefix) {
		return "", fmt.Errorf("%w: missing marker prefix", ErrProjectLabel)
	}
	tag, err := backend.ParseRecoveryTag(strings.TrimPrefix(label, projectOwnershipPrefix))
	if err != nil {
		return "", fmt.Errorf("%w: marker suffix is not exact", ErrProjectLabel)
	}
	return tag.OwnershipName(), nil
}

func projectLabelMatches(label, marker string) bool {
	parsed, err := parseProjectLabelFor(label, marker)
	return err == nil && parsed.marker == marker
}

func parseProjectLabelFor(label, marker string) (projectLabel, error) {
	parsed, err := parseProjectLabel(label)
	if err != nil {
		return projectLabel{}, err
	}
	if parsed.marker != marker {
		return projectLabel{}, fmt.Errorf("%w: marker suffix changed", ErrProjectLabel)
	}
	return parsed, nil
}
