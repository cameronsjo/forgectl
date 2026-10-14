package herdradapter

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// MetadataSource is the --source forgectl reports herdr metadata under. herdr
// keeps each source's tokens apart, so a token forgectl sets never replaces
// one another reporter set.
const MetadataSource = "forgectl-surface"

// WorkerNameToken is the workspace token that carries a worker's name. A
// herdr sidebar shows it where the operator's [ui.sidebar.spaces] rows name
// "$worker" (forgectl#1047). The workspace label stays the ownership marker.
const WorkerNameToken = "worker"

// herdr's own limits on a metadata report (src/app/api_helpers.rs). A key
// outside them is refused by herdr; checking here gives the refusal before
// any call and names the key.
const (
	MaxTokensPerReport = 16
	maxTokenKeyLen     = 32
	// MaxTokenValueLen is in characters. herdr truncates a longer value;
	// forgectl refuses it so a caller never stamps a value it did not send.
	MaxTokenValueLen = 80
)

// ErrMetadataFailed reports a metadata report herdr did not confirm.
var ErrMetadataFailed = errors.New("herdr: the metadata report failed")

// ErrBadToken reports a token key or value herdr would refuse or alter.
var ErrBadToken = errors.New("herdr: invalid metadata token")

// CheckTokenKey reports whether key is a token name herdr accepts: 1 to 32
// ASCII letters, digits, '_' or '-', not starting with '-'. herdr itself
// takes a leading '-', but the key is an argv operand (--clear-token k), and
// the sensitive exec seam refuses a dash-leading operand, so such a key is
// refused here, before any herdr call.
func CheckTokenKey(key string) error {
	if key == "" || len(key) > maxTokenKeyLen {
		return fmt.Errorf("%w: key %q must be 1-%d characters", ErrBadToken, key, maxTokenKeyLen)
	}
	if key[0] == '-' {
		return fmt.Errorf("%w: key %q must not start with '-'", ErrBadToken, key)
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return fmt.Errorf("%w: key %q may hold only ASCII letters, digits, '_' and '-'", ErrBadToken, key)
		}
	}
	return nil
}

// CheckTokenValue reports whether value is a token value herdr would store
// unchanged: non-empty, at most 80 characters, valid UTF-8, and free of
// control, bidi and invisible characters and surrounding space. herdr strips
// such characters and truncates, so a value it would alter is refused here
// instead.
func CheckTokenValue(key, value string) error {
	switch {
	case strings.TrimSpace(value) == "":
		return fmt.Errorf("%w: %s has an empty value; clear a token with --clear-token", ErrBadToken, key)
	case strings.TrimSpace(value) != value:
		return fmt.Errorf("%w: %s's value has leading or trailing space", ErrBadToken, key)
	case !utf8.ValidString(value):
		return fmt.Errorf("%w: %s's value is not valid UTF-8", ErrBadToken, key)
	case utf8.RuneCountInString(value) > MaxTokenValueLen:
		return fmt.Errorf("%w: %s's value is longer than %d characters", ErrBadToken, key, MaxTokenValueLen)
	}
	for _, r := range value {
		if termsafe.IsUnsafeTerminalRune(r) || termsafe.IsInvisibleRune(r) {
			return fmt.Errorf("%w: %s's value holds a control, bidi or invisible character", ErrBadToken, key)
		}
	}
	return nil
}

// ShowWorkerName sets the worker-name token on the workspace ref names, after
// the ownership check every write makes. It is cosmetic: the caller treats a
// failure as a note, never as a failed launch.
func (a *Adapter) ShowWorkerName(ctx context.Context, ref backend.Ref, name string) error {
	if err := CheckTokenValue(WorkerNameToken, name); err != nil {
		return err
	}
	_, ws, err := a.ownedPane(ctx, ref)
	if err != nil {
		return err
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrMetadata,
		exec.MustFixed("workspace"),
		exec.MustFixed("report-metadata"),
		exec.Opaque(ws),
		exec.MustFixed("--source"),
		exec.MustFixed(MetadataSource),
		exec.MustFixed("--token"),
		exec.Opaque(WorkerNameToken+"="+name),
	))
	if runErr != nil {
		return fmt.Errorf("%w: workspace report-metadata: %v", ErrMetadataFailed, a.classifyRunError(runErr, res))
	}
	return nil
}

// CheckTag checks one tag report before anything reaches herdr: set and
// unset are not both empty, at most MaxTokensPerReport keys in all, every key
// and value passes CheckTokenKey and CheckTokenValue, and no key is named
// twice. It returns the set keys and the cleared keys, each sorted.
func CheckTag(set map[string]string, unset []string) ([]string, []string, error) {
	if len(set) == 0 && len(unset) == 0 {
		return nil, nil, fmt.Errorf("%w: no token to set or clear", ErrBadToken)
	}
	if len(set)+len(unset) > MaxTokensPerReport {
		return nil, nil, fmt.Errorf("%w: at most %d tokens per report", ErrBadToken, MaxTokensPerReport)
	}
	keys := make([]string, 0, len(set))
	for k, v := range set {
		if err := CheckTokenKey(k); err != nil {
			return nil, nil, err
		}
		if err := CheckTokenValue(k, v); err != nil {
			return nil, nil, err
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	cleared := slices.Sorted(slices.Values(unset))
	for i, k := range cleared {
		if err := CheckTokenKey(k); err != nil {
			return nil, nil, err
		}
		if _, ok := set[k]; ok || (i > 0 && cleared[i-1] == k) {
			return nil, nil, fmt.Errorf("%w: %s is named twice", ErrBadToken, k)
		}
	}
	return keys, cleared, nil
}

// TagPane sets and clears display tokens on a worker's root pane under
// MetadataSource, after the checks a write makes (writablePane). It returns
// the pane it tagged. The tokens must pass CheckTag.
func (a *Adapter) TagPane(ctx context.Context, ref backend.Ref, set map[string]string, unset []string) (string, error) {
	keys, cleared, err := CheckTag(set, unset)
	if err != nil {
		return "", err
	}
	pane, err := a.writablePane(ctx, ref)
	if err != nil {
		return "", err
	}
	args := []exec.Arg{
		exec.MustFixed("pane"),
		exec.MustFixed("report-metadata"),
		exec.Opaque(pane),
		exec.MustFixed("--source"),
		exec.MustFixed(MetadataSource),
	}
	for _, k := range keys {
		args = append(args, exec.MustFixed("--token"), exec.Opaque(k+"="+set[k]))
	}
	for _, k := range cleared {
		args = append(args, exec.MustFixed("--clear-token"), exec.Opaque(k))
	}
	res, runErr := a.run.RunSensitive(ctx, a.command(exec.KindHerdrMetadata, args...))
	if runErr != nil {
		return "", fmt.Errorf("%w: pane report-metadata: %v", ErrMetadataFailed, a.classifyRunError(runErr, res))
	}
	return pane, nil
}

// TerminalID reads the herdr terminal_id of the root pane ref names. Unlike a
// pane id, a terminal_id survives the pane moving between tabs. It is a read
// of one pane record, checked to be the pane and workspace the reference
// names; it does not re-run the ownership check, so a caller that needs one
// probes first.
func (a *Adapter) TerminalID(ctx context.Context, ref backend.Ref) (string, error) {
	identity, err := ref.HerdrIdentity()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrScreenUnreadable, err)
	}
	ws, pane := identity.Workspace(), identity.Pane()
	if pane == "" || !strings.HasPrefix(pane, ws+":") {
		return "", fmt.Errorf("%w: the reference names pane %q, which is not in workspace %q", ErrScreenUnreadable, pane, ws)
	}
	p, err := a.paneRecord(ctx, pane, ws)
	if err != nil {
		return "", err
	}
	if p.TerminalID == "" {
		return "", fmt.Errorf("%w: pane get reply names no terminal_id", ErrScreenUnreadable)
	}
	return p.TerminalID, nil
}
