package activity

import (
	"errors"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	Version  = 1
	MaxSpans = 64
)

type Source string

const (
	SourceCodex  Source = "codex-turn-history"
	SourcePi     Source = "pi-agent-events"
	SourceHermes Source = "hermes-prompt"
)

type Coverage string

const (
	CoverageNativeHistory  Coverage = "native-history"
	CoverageCurrentProcess Coverage = "current-process"
)

type State string

const (
	Running    State = "running"
	Finished   State = "finished"
	Failed     State = "failed"
	Incomplete State = "incomplete"
	Unknown    State = "unknown"
)

type Span struct {
	ID        string     `json:"id"`
	TurnID    string     `json:"turn_id,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	State     State      `json:"state"`
	Provider  string     `json:"provider,omitempty"`
	Model     string     `json:"model,omitempty"`
}

type Snapshot struct {
	Version    int        `json:"version"`
	Source     Source     `json:"source"`
	Coverage   Coverage   `json:"coverage"`
	ObservedAt time.Time  `json:"observed_at"`
	Since      *time.Time `json:"since,omitempty"`
	Spans      []Span     `json:"spans"`
	Partial    bool       `json:"partial,omitempty"`
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func Validate(snapshot Snapshot) error {
	if snapshot.Version != Version {
		return errors.New("activity snapshot has an unsupported version")
	}
	switch snapshot.Source {
	case SourceCodex:
		if snapshot.Coverage != CoverageNativeHistory {
			return errors.New("codex activity requires native-history coverage")
		}
	case SourcePi, SourceHermes:
		if snapshot.Coverage != CoverageCurrentProcess {
			return errors.New("process activity requires current-process coverage")
		}
	default:
		return errors.New("activity snapshot has an unsupported source")
	}
	if !validTime(snapshot.ObservedAt) {
		return errors.New("activity snapshot has an invalid observation time")
	}
	if snapshot.Since != nil && (!validTime(*snapshot.Since) || snapshot.Since.After(snapshot.ObservedAt)) {
		return errors.New("activity snapshot has an invalid coverage start")
	}
	if snapshot.Coverage == CoverageCurrentProcess && snapshot.Since == nil {
		return errors.New("process activity lacks a coverage start")
	}
	if snapshot.Spans == nil || len(snapshot.Spans) > MaxSpans {
		return errors.New("activity snapshot has an invalid span count")
	}
	seen := make(map[string]struct{}, len(snapshot.Spans))
	running := 0
	for _, span := range snapshot.Spans {
		if err := ValidateSpan(span); err != nil {
			return err
		}
		if _, duplicate := seen[span.ID]; duplicate {
			return errors.New("activity snapshot repeats a span identity")
		}
		seen[span.ID] = struct{}{}
		if span.StartedAt.After(snapshot.ObservedAt.Add(time.Second)) || (span.EndedAt != nil && span.EndedAt.After(snapshot.ObservedAt.Add(time.Second))) {
			return errors.New("activity interval is later than its observation")
		}
		if snapshot.Since != nil && span.StartedAt.Before(*snapshot.Since) {
			return errors.New("activity interval precedes process coverage")
		}
		if span.State == Running {
			running++
		}
	}
	if running > 1 {
		return errors.New("activity snapshot contains multiple active intervals")
	}
	return nil
}

func ValidateSpan(span Span) error {
	if !identifier.MatchString(span.ID) || (span.TurnID != "" && !identifier.MatchString(span.TurnID)) {
		return errors.New("activity interval has an invalid identity")
	}
	if !validTime(span.StartedAt) {
		return errors.New("activity interval lacks a valid start")
	}
	if span.EndedAt != nil && (!validTime(*span.EndedAt) || span.EndedAt.Before(span.StartedAt)) {
		return errors.New("activity interval has an invalid end")
	}
	switch span.State {
	case Running:
		if span.EndedAt != nil {
			return errors.New("running activity already has an end")
		}
	case Finished, Failed, Incomplete:
		if span.EndedAt == nil {
			return errors.New("settled activity lacks an end")
		}
	case Unknown:
	default:
		return errors.New("activity interval has an unsupported state")
	}
	if (span.Provider == "") != (span.Model == "") || !validMetadata(span.Provider) || !validMetadata(span.Model) {
		return errors.New("activity interval has invalid model metadata")
	}
	return nil
}

func Clone(snapshot Snapshot) Snapshot {
	cloned := snapshot
	if snapshot.Since != nil {
		since := *snapshot.Since
		cloned.Since = &since
	}
	if snapshot.Spans != nil {
		cloned.Spans = make([]Span, len(snapshot.Spans))
	}
	for index, span := range snapshot.Spans {
		cloned.Spans[index] = span
		if span.EndedAt != nil {
			endedAt := *span.EndedAt
			cloned.Spans[index].EndedAt = &endedAt
		}
	}
	return cloned
}

func validTime(value time.Time) bool {
	return !value.IsZero() && value.Year() >= 1970 && value.Year() <= 9999
}

func validMetadata(value string) bool {
	if len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) || unicode.In(character, unicode.Cf) || termsafe.IsUnsafeTerminalRune(character) || termsafe.IsInvisibleRune(character) {
			return false
		}
	}
	return true
}
