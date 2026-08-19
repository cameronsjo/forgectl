package tmux

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// IdentityFormat is the -F spec for a tmux server generation identity: server
// PID, server start time, and native window id. It is the leading prefix of
// windowFormat, so a `new-window -P -F IdentityFormat` result and a
// list-windows row describe the same window in the same field order.
//
// Exported because internal/pr passes it to `new-window -P -F` and then matches
// the captured identity against ListWindows rows. If the two sides ever drifted,
// VerifyDispatched would match nothing and report every healthy review gone —
// TestWindowFormatCarriesIdentityPrefix pins them together without needing tmux.
const IdentityFormat = "#{pid}" + FieldSep + "#{start_time}" + FieldSep + "#{window_id}"

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
	versionOut, err := c.run.Run(ctx, c.tmuxBin, c.tmuxArgs("-V")...)
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

	args := c.tmuxArgs("display-message", "-p", IdentityFormat)
	out, err := c.run.Run(ctx, c.tmuxBin, args...)
	if err != nil {
		failure := c.classifyServerFailure(ctx, args, err)
		if failure.Kind == serverAbsent {
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

// identityTriple is a parsed "#{pid}<sep>#{start_time}<sep>#{<native id>}"
// result — the shape both IdentityFormat (window id) and sessionIdentityFormat
// (session id) emit.
type identityTriple struct {
	PID       string
	StartTime string
	ID        string
}

// parseIdentityTriple validates one generation-plus-native-id triple. kind
// names the object for diagnostics; validateID is the native-id validator for
// that object, so the same parser cannot accept a window id where a session id
// was asked for.
func parseIdentityTriple(value, kind string, validateID func(string) error) (identityTriple, error) {
	fields := SplitFields(value)
	// One field means no separator survived at all, not a malformed identity —
	// and a caller may wrap this in a "tmux 2.2 or newer is required" message
	// that would otherwise blame a perfectly modern tmux for the operator's
	// locale. Name the real cause (see ErrUnreadableFields).
	if len(fields) == 1 {
		return identityTriple{}, fmt.Errorf(
			"%w: tmux returned %q as a single field; "+
				"the most likely cause is a non-UTF-8 locale, in which tmux renders the separator lossily — check LANG/LC_ALL, set a UTF-8 locale, and retry",
			ErrUnreadableFields, value)
	}
	if len(fields) != 3 {
		return identityTriple{}, fmt.Errorf("tmux %s identity has %d fields, want 3 (raw %q)", kind, len(fields), value)
	}
	pid, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || pid == 0 {
		return identityTriple{}, fmt.Errorf("invalid tmux server pid %q", fields[0])
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return identityTriple{}, fmt.Errorf("invalid tmux server start time %q", fields[1])
	}
	if err := validateID(fields[2]); err != nil {
		return identityTriple{}, err
	}
	return identityTriple{PID: fields[0], StartTime: fields[1], ID: fields[2]}, nil
}

func parseGenerationIdentity(value string) (string, error) {
	triple, err := parseIdentityTriple(value, "dispatch", ValidateWindowID)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{triple.PID, triple.StartTime, triple.ID}, FieldSep), nil
}
