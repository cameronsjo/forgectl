// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package rexadapter

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// The parsers below read the JSON `rex` printed on 0.1.0-preview.19 (API
// revision 14); testdata holds the recorded replies. Each returns an error
// for anything it cannot positively read, and a caller treats that error as
// unreadable, never as absence.

var errUnreadable = errors.New("rex reply is missing a field this build needs")

// server is what `rex whoami --json` reports that the adapter uses.
type server struct {
	Endpoint   string
	HostID     string
	InstanceID string
	Version    string
	Revision   int
	// TailnetOpen is true unless the server reports Tailscale sharing as
	// disabled and tailnet members as not allowed.
	TailnetOpen bool
}

// parseWhoami reads `rex whoami --json`. Every field is required; the
// revision is returned as reported, and the caller refuses any but
// APIRevision.
func parseWhoami(raw []byte) (server, error) {
	var w struct {
		Endpoint    *string `json:"endpoint"`
		HostID      *string `json:"host_id"`
		InstanceID  *string `json:"instance_id"`
		RexVersion  *string `json:"rex_version"`
		APIRevision *int    `json:"api_revision"`
		Tailscale   *struct {
			State               *string `json:"state"`
			AllowTailnetMembers *bool   `json:"allow_tailnet_members"`
		} `json:"tailscale"`
	}
	if err := decodeOne(raw, &w); err != nil {
		return server{}, err
	}
	if w.Endpoint == nil || w.HostID == nil || w.InstanceID == nil || w.RexVersion == nil || w.APIRevision == nil ||
		w.Tailscale == nil || w.Tailscale.State == nil || w.Tailscale.AllowTailnetMembers == nil {
		return server{}, errUnreadable
	}
	s := server{Endpoint: *w.Endpoint, HostID: *w.HostID, InstanceID: *w.InstanceID, Version: *w.RexVersion, Revision: *w.APIRevision,
		TailnetOpen: *w.Tailscale.State != "disabled" || *w.Tailscale.AllowTailnetMembers}
	if s.Endpoint == "" || len(s.Endpoint) > maxEndpointLen || !printable(s.Endpoint) ||
		!serverIDShape(s.HostID) || !serverIDShape(s.InstanceID) || s.Version == "" || !printable(s.Version) {
		return server{}, errUnreadable
	}
	return s, nil
}

// serverIDShape accepts a bounded token of letters, digits and hyphens: the
// UUIDs Rex reports for host_id and instance_id, without claiming more about
// their format than was measured.
func serverIDShape(s string) bool {
	if s == "" || len(s) > maxServerIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' {
			return false
		}
	}
	return true
}

// printable reports whether s is printable ASCII.
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// created is what `rex new --json` reports: the new session and the one block
// its one window holds.
type created struct {
	Session string
	Block   string
}

// parseNew reads `rex new --json`. It requires a valid session id and
// exactly one window holding exactly one valid block id: a create that
// reports anything else did not create the one block Start asked for.
func parseNew(raw []byte) (created, error) {
	var n struct {
		SessionID      *string `json:"session_id"`
		InitialWindows []struct {
			BlockIDs []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if err := decodeOne(raw, &n); err != nil {
		return created{}, err
	}
	if n.SessionID == nil || len(n.InitialWindows) != 1 || len(n.InitialWindows[0].BlockIDs) != 1 {
		return created{}, errUnreadable
	}
	c := created{Session: *n.SessionID, Block: n.InitialWindows[0].BlockIDs[0]}
	if _, err := backend.NewRexIdentity(c.Session, c.Block); err != nil {
		return created{}, errUnreadable
	}
	return c, nil
}

// session is one row of `rex ls --json`.
type session struct {
	ID    string
	Label string
}

// parseSessions reads `rex ls --json`. The sessions array is required (an
// empty one is `[]`), and every row needs a valid id and a label.
func parseSessions(raw []byte) ([]session, error) {
	var l struct {
		Sessions *[]struct {
			SessionID *string `json:"session_id"`
			Label     *string `json:"label"`
		} `json:"sessions"`
	}
	if err := decodeOne(raw, &l); err != nil {
		return nil, err
	}
	if l.Sessions == nil {
		return nil, errUnreadable
	}
	out := make([]session, 0, len(*l.Sessions))
	for _, s := range *l.Sessions {
		if s.SessionID == nil || s.Label == nil {
			return nil, errUnreadable
		}
		if _, ok := backend.RexSessionCreatedMS(*s.SessionID); !ok {
			return nil, errUnreadable
		}
		out = append(out, session{ID: *s.SessionID, Label: *s.Label})
	}
	return out, nil
}

// block is one row of `rex block ls --json`.
type block struct {
	ID    string
	Label string
}

// parseBlocks reads `rex block ls -s S --json`. The blocks array is required,
// and every row needs a valid id and a label. A row this build cannot read
// fails the whole listing: skipping it could skip the very block a reference
// names and report it gone.
func parseBlocks(raw []byte) ([]block, error) {
	var l struct {
		Blocks *[]struct {
			BlockID *string `json:"block_id"`
			Label   *string `json:"label"`
		} `json:"blocks"`
	}
	if err := decodeOne(raw, &l); err != nil {
		return nil, err
	}
	if l.Blocks == nil {
		return nil, errUnreadable
	}
	out := make([]block, 0, len(*l.Blocks))
	for _, b := range *l.Blocks {
		if b.BlockID == nil || b.Label == nil || len(*b.Label) > maxRexLabelLen {
			return nil, errUnreadable
		}
		if _, ok := backend.RexBlockCreatedMS(*b.BlockID); !ok {
			return nil, errUnreadable
		}
		out = append(out, block{ID: *b.BlockID, Label: *b.Label})
	}
	return out, nil
}

// process is a block's program state from `rex block call ... process`.
type process struct {
	Running  bool
	ExitCode int
}

// parseProcess reads `rex block call -s S -b B process`. last_exit is
// required: null while the program runs, and {exit_code, error?} once it has
// exited. A reply without the key, or with an exit object that has no integer
// exit_code, is unreadable, never "running".
func parseProcess(raw []byte) (process, error) {
	var p map[string]json.RawMessage
	if err := decodeOne(raw, &p); err != nil {
		return process{}, err
	}
	last, ok := p["last_exit"]
	if !ok {
		return process{}, errUnreadable
	}
	if strings.TrimSpace(string(last)) == "null" {
		return process{Running: true}, nil
	}
	var exit struct {
		ExitCode *int `json:"exit_code"`
	}
	if err := json.Unmarshal(last, &exit); err != nil || exit.ExitCode == nil {
		return process{}, errUnreadable
	}
	return process{ExitCode: *exit.ExitCode}, nil
}
