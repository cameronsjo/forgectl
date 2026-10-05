// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package desk

import "strings"

// Event line prefixes, the batch event vocabulary. Only the desk writes
// done/<name>.events; step output never reaches it, so a step cannot print a
// line that ends the run for a watcher.
const (
	EventRunStart  = "RUN-START"
	EventStepStart = "STEP-START"
	EventStepEnd   = "STEP-END"
	EventStepSkip  = "STEP-SKIP"
	EventStepWarn  = "STEP-WARN"
	EventRunEnd    = "RUN-END"
	EventRunLost   = "RUN-LOST"
)

// eventMsgKey is the one field whose value may hold spaces: it runs to the
// end of the line, so it is always written last.
const eventMsgKey = "msg"

// ParsedEvent is one event line split into its key and fields. Values are
// raw file text: render them through termsafe before they reach a terminal.
type ParsedEvent struct {
	Key    string
	Fields map[string]string
}

// ParseEvent splits an event line, "KEY k=v k=v ... [msg=free text]". Fields
// are separated by single spaces, except that msg= takes the rest of the
// line. For a repeated key the first one wins. ok is false for an unknown
// event key or a field that is not k=v.
func ParseEvent(line string) (ParsedEvent, bool) {
	key, rest, _ := strings.Cut(line, " ")
	switch key {
	case EventRunStart, EventStepStart, EventStepEnd, EventStepSkip, EventStepWarn, EventRunEnd, EventRunLost:
	default:
		return ParsedEvent{}, false
	}
	ev := ParsedEvent{Key: key, Fields: map[string]string{}}
	for rest != "" {
		var tok string
		if strings.HasPrefix(rest, eventMsgKey+"=") {
			tok, rest = rest, ""
		} else {
			tok, rest, _ = strings.Cut(rest, " ")
		}
		k, v, found := strings.Cut(tok, "=")
		if !found || k == "" {
			return ParsedEvent{}, false
		}
		if _, dup := ev.Fields[k]; !dup {
			ev.Fields[k] = v
		}
	}
	return ev, true
}
