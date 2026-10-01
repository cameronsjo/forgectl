// Package wire decodes herdr's JSON reply envelopes and checks the operands
// forgectl passes to herdr. It is the one reader of herdr's wire format, shared
// by internal/herdr (the client) and internal/surface/herdradapter (the
// launcher), so a change to herdr's envelope is one edit here and one test set
// (cameronsjo/forgectl#722).
//
// It takes raw bytes and imports only the standard library: the two callers
// read herdr through different runners (exec.Runner's string output and the
// sensitive runner's exec.BoundedOutput), and truncation, redaction and error
// presentation stay with each caller.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Refusal is herdr's structured refusal, {"error":{"code","message"}}. Code is
// herdr's own vocabulary (workspace_not_found, server_not_running, ...); match
// on it, never on Message. Message is herdr's raw text: a caller that keeps or
// renders it redacts it first.
type Refusal struct {
	Code    string
	Message string
}

// DecodeError returns the refusal in raw when raw is exactly one JSON object
// whose "error" member carries a non-empty string code. Anything else returns
// false: log lines before the JSON, a second object, an envelope with no code,
// or a result envelope. Surrounding whitespace is allowed.
func DecodeError(raw []byte) (Refusal, bool) {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Refusal{}, false
	}
	if env.Error == nil || env.Error.Code == "" {
		return Refusal{}, false
	}
	return Refusal{Code: env.Error.Code, Message: env.Error.Message}, true
}

// ErrNoResult is returned by DecodeResult for a reply that is readable JSON
// but has no "result" member, or a null one. A renamed envelope lands here, and
// must not read as an empty result.
var ErrNoResult = errors.New("reply has no result")

// DecodeResult decodes the "result" member of herdr's {"id","result"} reply
// into T. The envelope's id is ignored and unknown fields are tolerated: herdr
// adds fields freely, and a strict decoder would turn every upgrade into an
// outage. A missing or null result returns ErrNoResult. Members of T that must
// be present belong behind pointers, so the caller can tell absent from empty.
func DecodeResult[T any](raw []byte) (T, error) {
	var zero T
	var env struct {
		Result *T `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return zero, fmt.Errorf("decode reply: %w", err)
	}
	if env.Result == nil {
		return zero, ErrNoResult
	}
	return *env.Result, nil
}

// MaxOperandLen bounds an operand in bytes. Real herdr ids are a dozen bytes
// and session names are short; the bound stops a runaway value reaching an
// argv. It is the stricter of the two limits the client and adapter kept
// before they shared this check.
const MaxOperandLen = 64

// The reasons CheckOperand refuses a value. None of them carries the value: it
// can come from the environment or a config file, so the caller decides
// whether and how to show it.
var (
	ErrEmptyOperand   = errors.New("is empty")
	ErrFlagOperand    = errors.New("starts with '-'")
	ErrLongOperand    = fmt.Errorf("is over %d bytes", MaxOperandLen)
	ErrControlOperand = errors.New("has a control character")
)

// CheckOperand reports whether s is safe as one herdr operand: non-empty, not
// read as a flag (no leading '-'), at most MaxOperandLen bytes, and free of
// control characters. It is the floor every operand meets; a caller with a
// narrower shape (a session name's charset) checks that on top. It applies no
// charset itself, because ids carry ':' and labels carry spaces.
func CheckOperand(s string) error {
	switch {
	case s == "":
		return ErrEmptyOperand
	case strings.HasPrefix(s, "-"):
		return ErrFlagOperand
	case len(s) > MaxOperandLen:
		return ErrLongOperand
	case strings.IndexFunc(s, unicode.IsControl) >= 0:
		return ErrControlOperand
	}
	return nil
}
