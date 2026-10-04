package resume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// This file finds each outdated session's herdr pane by its session id rather
// than by the HERDR_PANE_ID its process carries. The environment value is
// fixed when the process starts, and a herdr server restart renumbers every
// pane: after one, a long-running session's claim names a pane that no longer
// exists (herdr answers pane_not_found, measured 2026-10-04), while herdr's
// pane list shows the session under its new id. Resolution only chooses which
// pane the four checks look at; it never replaces any of them. Check 3 still
// wants herdr's label and the pid in the pane's foreground processes, because
// a nested session's hooks can put its id on its parent's pane.

// HerdrPane is one row of `herdr pane list`: the pane id and the agent session
// herdr's hooks last reported in it.
type HerdrPane struct {
	ID string
	// Agent and Session are agent_session.agent and agent_session.value, the
	// same fields check 3 compares; both are empty when herdr has no session
	// for the pane.
	Agent   string
	Session string
}

// PaneResolution is the pane a restart run uses for one session, and why.
type PaneResolution struct {
	// Pane is the pane to use: the one herdr reports holding the session, or
	// the environment's claim when herdr names none or could not be asked.
	Pane string
	// Note, when set, says where Pane came from for the operator: the
	// environment's differing claim, or the pane-list failure that forced the
	// fallback. It is display text and never decides anything.
	Note string
	// Ambiguous lists every pane herdr reports holding the session when there
	// is more than one. A run refuses such a session: which pane it runs in is
	// unknown.
	Ambiguous []string
}

// ResolvePane decides one session's pane from its environment claim and
// herdr's pane list (listErr set when the list could not be read):
//
//   - exactly one claude pane names the session: use it;
//   - more than one does: ambiguous (a nested session relabelling its
//     parent's pane looks like this);
//   - none does, or the list failed: fall back to the environment's claim,
//     today's behavior, and let the per-session checks decide.
func ResolvePane(sessionID, envPane string, panes []HerdrPane, listErr error) PaneResolution {
	r := PaneResolution{Pane: envPane}
	if listErr != nil {
		r.Note = "from its environment; herdr pane list failed (" + clipDetail(listErr.Error()) + ")"
		return r
	}
	var claims []string
	for _, p := range panes {
		if p.Agent == "claude" && p.Session == sessionID {
			claims = append(claims, p.ID)
		}
	}
	switch len(claims) {
	case 0:
	case 1:
		r.Pane = claims[0]
		switch {
		case envPane == "":
			r.Note = "found by session; none in its environment"
		case envPane != claims[0]:
			r.Note = "found by session; env said " + envPane
		}
	default:
		r.Ambiguous = claims
	}
	return r
}

// resolvePanes applies ResolvePane to every session in list, setting Pane and
// PaneNote, and returns the ids herdr reports in more than one pane.
func resolvePanes(list []OutdatedSession, panes []HerdrPane, listErr error) ([]OutdatedSession, map[string][]string) {
	out := make([]OutdatedSession, len(list))
	ambiguous := map[string][]string{}
	for i, s := range list {
		r := ResolvePane(s.SessionID, s.Pane, panes, listErr)
		s.Pane, s.PaneNote = r.Pane, r.Note
		if len(r.Ambiguous) > 0 {
			ambiguous[s.SessionID] = r.Ambiguous
		}
		out[i] = s
	}
	return out, ambiguous
}

// SkipAmbiguousPanes re-plans as a skip every session herdr reports in more
// than one pane. A session already skipped keeps its own reason.
func SkipAmbiguousPanes(plan []RestartPlanItem, ambiguous map[string][]string) []RestartPlanItem {
	out := make([]RestartPlanItem, len(plan))
	for i, item := range plan {
		if panes := ambiguous[item.SessionID]; len(panes) > 0 && item.Action != ActionSkip {
			item.Action = ActionSkip
			item.Reason = fmt.Sprintf("herdr reports it in %d panes (%s), so which one it runs in is unknown (nested session?); quit it, then run %s",
				len(panes), strings.Join(panes, ", "), ManualResume(item.SessionID))
		}
		out[i] = item
	}
	return out
}

// paneLabel is a session's pane for a progress or preview line, with the
// resolution note when there is one.
func paneLabel(s OutdatedSession) string {
	if s.PaneNote == "" {
		return s.Pane
	}
	return s.Pane + " (" + s.PaneNote + ")"
}

// ListPanes reads herdr's pane list, one read-only call.
func (e SystemRestartEnv) ListPanes(ctx context.Context) ([]HerdrPane, error) {
	if e.runner == nil {
		return nil, errors.New("no command runner")
	}
	out, err := e.runHerdr(ctx, "pane", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr pane list: %w", err)
	}
	return parsePaneList(out)
}

// parsePaneList reads `herdr pane list` JSON: .result.panes[] with pane_id and
// agent_session.{agent,value} (measured on herdr 0.9.1; a pane with no agent
// has no agent_session key). A pane id checkPaneArg would refuse fails the
// whole list rather than being dropped: a dropped row could be the second
// claim that makes a session ambiguous.
func parsePaneList(out string) ([]HerdrPane, error) {
	var reply struct {
		Result struct {
			Panes *[]struct {
				PaneID       string `json:"pane_id"`
				AgentSession *struct {
					Agent string `json:"agent"`
					Value string `json:"value"`
				} `json:"agent_session"`
			} `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return nil, errors.New("herdr pane list: reply is not JSON")
	}
	if reply.Result.Panes == nil {
		return nil, errors.New("herdr pane list: reply has no panes")
	}
	panes := make([]HerdrPane, 0, len(*reply.Result.Panes))
	for _, p := range *reply.Result.Panes {
		if checkPaneArg(p.PaneID) != nil {
			return nil, errors.New("herdr pane list: reply has a pane id that is not a plain operand")
		}
		hp := HerdrPane{ID: p.PaneID}
		if as := p.AgentSession; as != nil {
			hp.Agent, hp.Session = as.Agent, as.Value
		}
		panes = append(panes, hp)
	}
	return panes, nil
}
