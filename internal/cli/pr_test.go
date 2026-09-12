package cli

// Test plan for pr.go's `forgectl pr <ref>` RunE — the admission cap on the
// single-launch path (forgectl#472, Task 3 of the review-autonomy spine).
//
//   [x] Happy: below the cap, `pr <ref>` reserves then launches — the record
//       lands `active`, not merely `prepared` (positive control)
//   [x] Boundary: at the cap, `pr <ref>` refuses with exit 1 and the
//       three-line message naming max and live; no workspace, no record
//       beyond the pre-existing occupant
//   [x] Boundary: `pr <ref> --queue` at the cap writes a `queued` record with
//       no workspace and exits 0, printing the record path
//   [x] Invariant: `--queue` twice on one ref refuses, naming the first
//       record — no second record is written
//
// These share the ledger-backed tmux double from pr_outcome_test.go
// (remoteReviewCmd's sibling here builds the command directly so a cap seed
// can run before ExecuteContext).

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/config"
	netpkg "github.com/cameronsjo/forgectl/internal/net"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// remoteReviewCmdCapped is remoteReviewCmd (pr_outcome_test.go) with an
// explicit cfg, so a cap test can pass MaxConcurrent without threading a new
// parameter through the shared helper every other row in this package uses.
// It also returns the client, so a test can List() the records the command
// left behind.
func remoteReviewCmdCapped(t *testing.T, l *tmuxLedger, cfg config.Config) (*cobra.Command, *bytes.Buffer, *pr.Client) {
	t.Helper()
	fakeClaudeBin(t)
	reviewTempRoot(t)
	fake := l.runner()
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithFindingsDir(t.TempDir()),
		pr.WithTmuxSession(l.session),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
	cmd := newPrCmdForClient(cfg, client, netpkg.New(fake), filepath.Join(t.TempDir(), "reviewed.json"), theme.Theme{})
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	return cmd, out, client
}

func TestPrRefCmd_BelowCap_ReservesThenLaunchesActive(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	cmd, out, client := remoteReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 4}})
	cmd.SetArgs([]string{"cameronsjo/forgectl#42"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "prepared clean-room review of cameronsjo/forgectl#42") {
		t.Errorf("stdout = %q, want the success line", out.String())
	}

	summaries, unreadable, err := client.List(context.Background())
	if err != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v, want exactly one record", summaries)
	}
	if summaries[0].Phase() != pr.PhaseActive {
		t.Errorf("phase = %q, want %q — reserve (preparing) must complete through prepared to active", summaries[0].Phase(), pr.PhaseActive)
	}
}

func TestPrRefCmd_AtCap_RefusesWithThreeLineMessage(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	ledger.live = append(ledger.live, ledgerWindow{id: "@0", name: "pr-already-running"})
	cmd, out, client := remoteReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 1}})
	cmd.SetArgs([]string{"cameronsjo/forgectl#42"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("want an error at the cap, got nil")
	}
	for _, want := range []string{
		"review cap reached (max 1, 1 running) — nothing prepared.",
		"see them:   forgectl pr list",
		"queue it:   forgectl pr cameronsjo/forgectl#42 --queue   (then: forgectl pr drain --once)",
		"raise it:   [pr] max_concurrent in config.toml",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want line %q", err.Error(), want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed on refusal", out.String())
	}
	summaries, unreadable, listErr := client.List(context.Background())
	if listErr != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, listErr)
	}
	if len(summaries) != 0 {
		t.Errorf("summaries = %+v, want none — refusal must write no record (the pre-existing live window is not a breadcrumb)", summaries)
	}
}

func TestPrRefCmd_QueueAtCap_WritesQueuedRecordNoWorkspace(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	ledger.live = append(ledger.live, ledgerWindow{id: "@0", name: "pr-already-running"})
	cmd, out, client := remoteReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 1}})
	cmd.SetArgs([]string{"cameronsjo/forgectl#42", "--queue"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "queued cameronsjo/forgectl#42") {
		t.Errorf("stdout = %q, want the queued line", out.String())
	}
	if !strings.Contains(out.String(), "record:") || !strings.Contains(out.String(), "start it: forgectl pr drain --once") {
		t.Errorf("stdout = %q, want the record path and the drain hint", out.String())
	}

	summaries, unreadable, err := client.List(context.Background())
	if err != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v, want exactly one queued record", summaries)
	}
	if summaries[0].Phase() != pr.PhaseQueued {
		t.Errorf("phase = %q, want %q", summaries[0].Phase(), pr.PhaseQueued)
	}
	if !summaries[0].IsWorkspaceNone() {
		t.Errorf("a queued record must carry no workspace; summary = %+v", summaries[0])
	}
}

func TestPrRefCmd_QueueSkipsDispatchCapabilityFloor(t *testing.T) {
	fake := oldTmuxRunner()
	client := pr.New(fake, pr.WithSessionsDir(t.TempDir()))
	netClient := netpkg.New(fake)
	cmd := newPrCmdForClient(config.Config{}, client, netClient, filepath.Join(t.TempDir(), "reviewed.json"), theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"cameronsjo/forgectl#42", "--queue"})

	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("--queue on a pre-2.2 tmux: unexpected error: %v", err)
	}
	summaries, _, err := client.List(context.Background())
	if err != nil || len(summaries) != 1 || summaries[0].Phase() != pr.PhaseQueued {
		t.Fatalf("List: %+v, %v — want exactly one queued record", summaries, err)
	}
}

func TestPrRefCmd_QueueTwice_RefusesNamingFirstRecord(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	cmd, _, client := remoteReviewCmdCapped(t, ledger, config.Config{})
	cmd.SetArgs([]string{"cameronsjo/forgectl#42", "--queue"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("first queue: unexpected error: %v", err)
	}
	summaries, _, err := client.List(context.Background())
	if err != nil || len(summaries) != 1 {
		t.Fatalf("List after first queue: %+v, %v", summaries, err)
	}
	firstPath := summaries[0].Path()

	// A fresh cobra.Command over the SAME client, so the second --queue sees
	// the record the first one wrote — a cobra.Command's flag state is not
	// reusable across ExecuteContext calls, but the client is.
	cmd2 := newPrCmdForClient(config.Config{}, client, netpkg.New(ledger.runner()), filepath.Join(t.TempDir(), "reviewed.json"), theme.Theme{})
	cmd2.SetOut(new(bytes.Buffer))
	cmd2.SetErr(new(bytes.Buffer))
	cmd2.SilenceUsage = true
	cmd2.SetArgs([]string{"cameronsjo/forgectl#42", "--queue"})
	err = cmd2.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("second --queue on the same ref: want an error, got nil")
	}
	if !strings.Contains(err.Error(), firstPath) {
		t.Errorf("error = %q, want it to name the first record %q", err.Error(), firstPath)
	}

	summaries, _, err = client.List(context.Background())
	if err != nil || len(summaries) != 1 {
		t.Fatalf("List after duplicate queue: %+v, %v — want still exactly one record", summaries, err)
	}
}

// --- Fix round on the security review's three Important findings ---
//
//   [x] A pure policy refusal (the agent gate) runs BEFORE Reserve, so it
//       leaves zero records — the promise this command's Long text makes
//   [x] A failure AFTER Reserve (gh errors) parks the reservation in
//       needs-repair with the reason: the slot frees and a retry is admitted
//   [x] --queue applies the same agent gate the immediate path applies, so a
//       queued record can never store a pairing the launch path refuses
//   [x] A duplicate refusal naming a `queued` record advises `pr teardown`
//       only — `pr repair` does not list queued records

// TestPrRefCmd_CodexOnRemoteRef_RefusesBeforeReserving states the ordering the
// docs promise: a refusal that needs no I/O to decide happens before the slot
// is claimed, so nothing is left on disk to repair.
func TestPrRefCmd_CodexOnRemoteRef_RefusesBeforeReserving(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	cmd, out, client := remoteReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 4}})
	cmd.SetArgs([]string{"cameronsjo/forgectl#42", "--agent", "codex"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("want a refusal for --agent codex on a remote ref, got nil")
	}
	if !strings.Contains(err.Error(), "codex") {
		t.Errorf("error = %q, want the Codex confinement refusal", err.Error())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed on refusal", out.String())
	}
	summaries, unreadable, listErr := client.List(context.Background())
	if listErr != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, listErr)
	}
	if len(summaries) != 0 {
		t.Errorf("summaries = %+v, want none — a pure policy refusal must reserve nothing", summaries)
	}
}

// TestPrRefCmd_QueueAppliesTheAgentGate closes the one admission surface that
// could persist an agent/provenance pairing the immediate path refuses. The
// refusal must be the SAME one, so the operator meets it here rather than the
// drainer meeting it hours later on someone else's command.
func TestPrRefCmd_QueueAppliesTheAgentGate(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	immediate, _, _ := remoteReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 4}})
	immediate.SetArgs([]string{"cameronsjo/forgectl#43", "--agent", "codex"})
	immediateErr := immediate.ExecuteContext(context.Background())
	if immediateErr == nil {
		t.Fatal("immediate path: want a refusal for --agent codex, got nil")
	}

	queued, out, client := remoteReviewCmdCapped(t, ledger, config.Config{Pr: config.PrConfig{MaxConcurrent: 4}})
	queued.SetArgs([]string{"cameronsjo/forgectl#43", "--agent", "codex", "--queue"})
	queuedErr := queued.ExecuteContext(context.Background())
	if queuedErr == nil {
		t.Fatal("--queue: want the same refusal for --agent codex, got nil")
	}
	if queuedErr.Error() != immediateErr.Error() {
		t.Errorf("--queue refusal = %q, want it identical to the immediate path's %q",
			queuedErr.Error(), immediateErr.Error())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing printed on refusal", out.String())
	}
	summaries, unreadable, listErr := client.List(context.Background())
	if listErr != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, listErr)
	}
	if len(summaries) != 0 {
		t.Errorf("summaries = %+v, want none — a refused queue must write no record", summaries)
	}
}

// TestPrRefCmd_PrepareFailureParksTheReservation is the leak the bulk path
// already avoided: a `gh` failure after the slot is claimed used to leave a
// `preparing` record holding that slot forever, so four transient failures
// would exhaust a default cap of four.
func TestPrRefCmd_PrepareFailureParksTheReservation(t *testing.T) {
	fakeClaudeBin(t)
	reviewTempRoot(t)
	ledger := newTmuxLedger("forgectl")
	ghCalls := 0
	fake := ledger.runnerWith(func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			ghCalls++
			if ghCalls == 1 {
				return "", errors.New("boom: gh could not reach the forge")
			}
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		}
		return "", nil
	})
	client := pr.New(fake,
		pr.WithSessionsDir(t.TempDir()),
		pr.WithFindingsDir(t.TempDir()),
		pr.WithTmuxSession(ledger.session),
		pr.WithDispatchWait(func(context.Context) error { return nil }),
	)
	cfg := config.Config{Pr: config.PrConfig{MaxConcurrent: 1}}
	newCmd := func() *cobra.Command {
		c := newPrCmdForClient(cfg, client, netpkg.New(fake), filepath.Join(t.TempDir(), "reviewed.json"), theme.Theme{})
		c.SetOut(new(bytes.Buffer))
		c.SetErr(new(bytes.Buffer))
		c.SilenceUsage = true
		return c
	}

	first := newCmd()
	first.SetArgs([]string{"cameronsjo/forgectl#42"})
	err := first.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("want the gh failure to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "boom: gh could not reach the forge") {
		t.Errorf("error = %q, want the original gh failure, not a bookkeeping error", err.Error())
	}

	summaries, unreadable, listErr := client.List(context.Background())
	if listErr != nil || unreadable != 0 {
		t.Fatalf("List: %+v, %d, %v", summaries, unreadable, listErr)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v, want exactly one parked record", summaries)
	}
	if summaries[0].Phase() != pr.PhaseNeedsRepair {
		t.Fatalf("phase = %q, want %q — a failed reservation must be parked, not left preparing",
			summaries[0].Phase(), pr.PhaseNeedsRepair)
	}
	if !strings.Contains(summaries[0].RepairReason(), "boom: gh could not reach the forge") {
		t.Errorf("reason = %q, want it to carry why the prepare failed", summaries[0].RepairReason())
	}

	// The slot is free again...
	if _, live, free, ok := client.Admit(context.Background(), cfg.Pr.MaxConcurrent); !ok || free != 1 || live != 0 {
		t.Errorf("Admit = live %d, free %d, ok %v; want live 0, free 1 — a parked record holds no slot", live, free, ok)
	}
	// ...and the same ref is admitted on retry, now that gh answers.
	retry := newCmd()
	retry.SetArgs([]string{"cameronsjo/forgectl#42", "--no-verify"})
	if err := retry.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("retry of the same ref: unexpected error: %v", err)
	}
}

// TestPrRefCmd_DuplicateQueueRefusalAdvisesTeardownOnly keeps the remedy
// reachable: `pr repair` does not list queued records, so naming it here would
// send the operator to a command that prints nothing.
func TestPrRefCmd_DuplicateQueueRefusalAdvisesTeardownOnly(t *testing.T) {
	ledger := newTmuxLedger("forgectl")
	cmd, _, client := remoteReviewCmdCapped(t, ledger, config.Config{})
	cmd.SetArgs([]string{"cameronsjo/forgectl#42", "--queue"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("first queue: unexpected error: %v", err)
	}
	summaries, _, err := client.List(context.Background())
	if err != nil || len(summaries) != 1 {
		t.Fatalf("List after first queue: %+v, %v", summaries, err)
	}
	firstPath := summaries[0].Path()

	second := newPrCmdForClient(config.Config{}, client, netpkg.New(ledger.runner()), filepath.Join(t.TempDir(), "reviewed.json"), theme.Theme{})
	second.SetOut(new(bytes.Buffer))
	second.SetErr(new(bytes.Buffer))
	second.SilenceUsage = true
	second.SetArgs([]string{"cameronsjo/forgectl#42", "--queue"})
	err = second.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("second --queue on the same ref: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "forgectl pr teardown "+firstPath) {
		t.Errorf("error = %q, want it to advise 'forgectl pr teardown %s'", err.Error(), firstPath)
	}
	if strings.Contains(err.Error(), "forgectl pr repair") {
		t.Errorf("error = %q, must not advise 'pr repair' for a queued record — repair does not list them", err.Error())
	}
}
