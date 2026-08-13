package tmux

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const identityFormat = "#{pid}" + fieldSep + "#{start_time}" + fieldSep + "#{window_id}"

var (
	tmuxVersionPattern = regexp.MustCompile(`^tmux ([0-9]+)\.([0-9]+)(?:[a-z])?(?:[-+~][A-Za-z0-9._+~:-]+)?$`)
	windowIDPattern    = regexp.MustCompile(`^@[0-9]+$`)
)

type GenerationCapability struct {
	Version string
}

func parseTmuxVersion(value string) (major int, minor int, normalized string, err error) {
	match := tmuxVersionPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, 0, "", fmt.Errorf("malformed tmux version %q", value)
	}
	major, err = strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, "", fmt.Errorf("parse tmux major version: %w", err)
	}
	minor, err = strconv.Atoi(match[2])
	if err != nil {
		return 0, 0, "", fmt.Errorf("parse tmux minor version: %w", err)
	}
	return major, minor, value, nil
}

func (c *Client) CheckGenerationCapability(ctx context.Context) (GenerationCapability, error) {
	const unknownVersion = "unknown"
	versionOut, err := c.run.Run(ctx, c.tmuxBin, "-V")
	if err != nil {
		return GenerationCapability{}, generationCapabilityError(unknownVersion, err)
	}
	major, minor, normalized, err := parseTmuxVersion(versionOut)
	if err != nil {
		return GenerationCapability{}, generationCapabilityError(unknownVersion, err)
	}
	if major < 2 || (major == 2 && minor < 2) {
		return GenerationCapability{}, generationCapabilityError(normalized, fmt.Errorf("unsupported tmux version"))
	}

	args := []string{"display-message", "-p", identityFormat}
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		failure := c.classifyServerFailure(ctx, args, err)
		if failure.Kind == serverAbsentDefault {
			return GenerationCapability{Version: normalized}, nil
		}
		cause := failure.Cause
		if cause == nil {
			cause = err
		}
		return GenerationCapability{}, generationCapabilityError(normalized, cause)
	}
	if _, err := parseGenerationIdentity(out); err != nil {
		return GenerationCapability{}, generationCapabilityError(normalized, err)
	}
	return GenerationCapability{Version: normalized}, nil
}

func generationCapabilityError(found string, cause error) error {
	return fmt.Errorf("tmux 2.2 or newer is required to launch PR reviews with exact dispatch identity (found %q); upgrade tmux and retry: %w", found, cause)
}

func parseGenerationIdentity(value string) (string, error) {
	fields := strings.Split(value, fieldSep)
	if len(fields) != 3 {
		return "", fmt.Errorf("tmux dispatch identity has %d fields, want 3", len(fields))
	}
	pid, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || pid == 0 {
		return "", fmt.Errorf("invalid tmux server pid %q", fields[0])
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return "", fmt.Errorf("invalid tmux server start time %q", fields[1])
	}
	if !windowIDPattern.MatchString(fields[2]) {
		return "", fmt.Errorf("invalid tmux window id %q", fields[2])
	}
	return strings.Join(fields, fieldSep), nil
}
