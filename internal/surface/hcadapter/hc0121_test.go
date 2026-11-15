// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package hcadapter

import (
	"context"
	"slices"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// get0121 spells a `hc get --json` record the way hc 0.1.21 prints it: the
// 0.1.15 fields the adapter reads plus the additive fields measured on 0.1.21
// (reported, status_ms, screen, identity, foreground, parent, keep, reason,
// signal, title, and the where, herdr.seen and herdr.tab labels). The
// adapter must ignore every one of them.
func get0121(owner, state, status, finished, reason, signal string) string {
	return `{"address":"t1","argv":["sh","-c","sleep 120"],"cols":200,"created_ms":1793409108596,` +
		`"cwd":"/repo","die_with_parent":false,"exit_code":null,"finished_ms":` + finished + `,` +
		`"foreground":{"argv":["sleep","120"],"cwd":"/repo","name":"sleep","pid":4243},"identity":"ghostty","keep":false,` +
		`"labels":{"forgectl.surface":"` + owner + `","herdr.seen":"1","herdr.space":"sp0123456789abcdef0123456789abcdef",` +
		`"herdr.tab":"tb0123456789abcdef","where":"Books / repo"},` +
		`"name":"x","notify":false,"parent":"t0","pid":4242,"reason":` + reason + `,"reported":false,` +
		`"rm":false,"rows":120,"screen":"exact","signal":` + signal + `,"state":"` + state + `",` +
		`"status":` + status + `,"status_ms":1793409109000,"template":null,"title":"","turn":1}`
}

// TestHc0121GetRecordWithUnknownFieldsParses pins that the additive fields
// hc 0.1.21 prints do not break the parsers: Probe and Close still read the
// state, the ownership label and the creation time.
func TestHc0121GetRecordWithUnknownFieldsParses(t *testing.T) {
	f := &fakeHC{coreID: "core1", terminals: map[string]string{}}
	a := newTestAdapter(t, f, "")
	ref := startWorker(t, f, a)
	owner := ref.OwnershipName()

	live := []string{
		`{"app":"claude-code","kind":null,"msg":"","progress":null,"state":"working","title":"T"}`,
		`{"app":"claude-code","kind":"permission","msg":"approve?","progress":0.5,"state":"blocked","title":"T"}`,
		`{"app":"claude-code","kind":"question","msg":"which?","progress":null,"state":"blocked","title":""}`,
		`{"app":"claude-code","kind":"auth","msg":"login","progress":null,"state":"blocked","title":""}`,
	}
	for _, status := range live {
		f.terminals["t1"] = get0121(owner, "live", status, "null", "null", "null")
		if res := a.Probe(context.Background(), ref); res.State() != backend.ProbePresent {
			t.Fatalf("live Probe with status %s = %v, want present", status, res)
		}
	}

	f.terminals["t1"] = get0121(owner, "finished", "null", "1793409999999", `"killed"`, "1")
	if res := a.Probe(context.Background(), ref); res.State() != backend.ProbeGone {
		t.Fatalf("finished Probe = %v, want gone", res)
	}

	f.terminals["t1"] = get0121(owner, "live", live[0], "null", "null", "null")
	f.verbs = nil
	if res := a.Close(context.Background(), ref); res.State() != backend.CloseClosed {
		t.Fatalf("Close = %v, want closed", res)
	}
	if want := []string{"server", "get", "kill", "rm"}; !slices.Equal(f.verbs, want) {
		t.Fatalf("Close verbs = %v, want %v", f.verbs, want)
	}

	// The ownership label must still be the one compared: a foreign value
	// next to the new labels is refused.
	f.terminals["t1"] = get0121("someoneelse", "live", live[0], "null", "null", "null")
	if res := a.Close(context.Background(), ref); res.State() != backend.CloseIdentityMismatch {
		t.Fatalf("Close of a foreign terminal = %v, want identity-mismatch", res)
	}
}
