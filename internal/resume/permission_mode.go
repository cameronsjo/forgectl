package resume

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// KeepPermissionModeFlag is the `forgectl resume` flag that resumes a session
// in the permission mode its transcript last recorded instead of the launch
// posture's. A restart run types it into every pane it relaunches, so an
// update restart puts each session back in the mode it was in
// (cameronsjo/forgectl#1332).
const KeepPermissionModeFlag = "--keep-permission-mode"

// permissionModeRecord is the subset of a transcript line LastPermissionMode
// reads. Claude Code writes the mode two ways (measured on 2.1.30x): a
// `{"type":"permission-mode","permissionMode":"…"}` record, and a top-level
// "permissionMode" field on each user prompt. Only the top level is decoded,
// so a "permissionMode" key nested in a message body or a tool's input is
// never taken for the session's own.
type permissionModeRecord struct {
	PermissionMode string `json:"permissionMode"`
	IsSidechain    bool   `json:"isSidechain"`
}

// LastPermissionMode returns the permission mode a session's transcript last
// recorded, or "" when the transcript cannot be found or read or records none.
//
// The value is transcript text, returned as found and NOT validated: a caller
// must check it against Claude Code's known modes (launch.CarryPermissionMode
// does) before it reaches an argv.
func LastPermissionMode(p Paths, id, cwd string) string {
	path := transcriptPath(p, id, cwd, nil)
	if path == "" {
		return ""
	}
	f, err := os.Open(path) // #nosec G304 -- resolved under the caller's own ~/.claude/projects
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	return lastPermissionMode(f)
}

// lastPermissionMode streams r and keeps the last non-empty top-level
// permissionMode. It reads the whole transcript, because the mode it wants is
// the latest one; the substring pre-filter keeps that to one JSON decode per
// candidate line, as readTranscript does. Sidechain records (a subagent's
// turns) are skipped: their mode is the subagent's, not the session's. A line
// over the read buffer is skipped by forEachLine; the dedicated
// permission-mode record is always short.
func lastPermissionMode(r io.Reader) string {
	var mode string
	forEachLine(r, func(line []byte) bool {
		if !bytes.Contains(line, []byte(`"permissionMode"`)) {
			return false
		}
		var rec permissionModeRecord
		if json.Unmarshal(line, &rec) != nil || rec.IsSidechain || rec.PermissionMode == "" {
			return false
		}
		mode = rec.PermissionMode
		return false
	})
	return mode
}
