//go:build unix

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/surface/project"
)

func prepareProjectEvidence(test *testing.T) (project.Store, projectReport, mail.Worker) {
	test.Helper()
	store := project.Store{Root: filepath.Join(test.TempDir(), "registry")}
	created := createCLIProject(test, store.Root, test.TempDir(), "Evidence")
	member := mail.Worker{Name: "lead", Harness: mail.HarnessCodex, Coordinator: true, Worktree: created.Project.Worktree, RunID: "evidence-run", ThreadID: "native-session"}
	registerProjectCLIMember(test, created.Ledger, member, false)
	bindProjectCLIThread(test, store.Root, created.Project.ID, created.Project.CoordinatorThreadID, member.Name, member.RunID, "none")
	return store, created, member
}

func projectEvidenceCommand(test *testing.T, store project.Store, created projectReport, runID, action string, flags ...string) (projectEvidenceReport, string, int) {
	test.Helper()
	arguments := []string{"project", "--root", store.Root, "thread", "--project", created.Project.ID, action, created.Project.CoordinatorThreadID, "--run-id", runID, "--json"}
	arguments = append(arguments, flags...)
	output, stderr, code := runSurface(test, "", arguments...)
	var report projectEvidenceReport
	if output != "" {
		if err := json.Unmarshal([]byte(output), &report); err != nil {
			test.Fatalf("invalid evidence output: %v: %s", err, output)
		}
	}
	return report, stderr, code
}

func installProjectObserver(test *testing.T, observer func(context.Context, mail.Worker) (projectNativeObservation, error)) {
	test.Helper()
	previous := projectObserveNative
	projectObserveNative = observer
	test.Cleanup(func() { projectObserveNative = previous })
}

func TestSurfaceProjectEvidenceMetadataDoesNotBecomeCompletion(test *testing.T) {
	store, created, member := prepareProjectEvidence(test)
	before, err := store.Snapshot(test.Context())
	if err != nil {
		test.Fatal(err)
	}
	installProjectObserver(test, func(_ context.Context, observed mail.Worker) (projectNativeObservation, error) {
		if observed.Name != member.Name || observed.ThreadID != member.ThreadID || observed.RunID != member.RunID || !observed.Coordinator {
			test.Fatalf("observer targeted wrong enrollment: %#v", observed)
		}
		return projectNativeObservation{Source: project.EvidenceSourceCodexThreadRead, NativeSessionID: member.ThreadID, Provider: "openai", Model: "selected-model"}, nil
	})
	report, stderr, code := projectEvidenceCommand(test, store, created, member.RunID, "observe")
	if code != 0 || report.Evidence != nil || report.Native == nil || report.Native.Model != "selected-model" || report.ProcessLiveness != project.ProcessLivenessUnknown || report.Revision != before.Revision {
		test.Fatalf("metadata overclaimed completion: exit=%d stderr=%s report=%#v", code, stderr, report)
	}
	after, err := store.Snapshot(test.Context())
	if err != nil || len(after.Projects[0].Evidence) != 0 || after.Revision != before.Revision {
		test.Fatalf("metadata mutated durable evidence: %v %#v", err, after)
	}
}

func TestSurfaceProjectEvidenceRecordsTurnWithoutInventingProducer(test *testing.T) {
	store, created, member := prepareProjectEvidence(test)
	observedAt := time.Now().UTC()
	installProjectObserver(test, func(context.Context, mail.Worker) (projectNativeObservation, error) {
		return projectNativeObservation{
			Source: project.EvidenceSourceCodexThreadRead, NativeSessionID: member.ThreadID,
			Provider: "openai", Model: "currently-selected-model",
			Turn: &projectNativeTurn{ID: "native-turn", Status: project.EvidenceCompleted, ObservedAt: observedAt},
		}, nil
	})
	report, stderr, code := projectEvidenceCommand(test, store, created, member.RunID, "observe")
	if code != 0 || report.Evidence == nil || report.Evidence.TurnID != "native-turn" || report.Evidence.Status != project.EvidenceCompleted || report.Evidence.Model != "" || report.Evidence.Provider != "" || report.ProcessLiveness != project.ProcessLivenessUnknown {
		test.Fatalf("native result lost or model producer invented: exit=%d stderr=%s report=%#v", code, stderr, report)
	}
	repeated, stderr, code := projectEvidenceCommand(test, store, created, member.RunID, "observe")
	if code != 0 || repeated.Revision != report.Revision || repeated.Evidence == nil || !repeated.Evidence.ObservedAt.Equal(observedAt) {
		test.Fatalf("repeat mutated evidence: exit=%d stderr=%s report=%#v", code, stderr, repeated)
	}
	output, stderr, code := runSurface(test, "", "project", "--root", store.Root, "timeline", created.Project.ID, "--json")
	if code != 0 || !strings.Contains(output, "NATIVE-TURN-COMPLETED") || !strings.Contains(output, "native-turn") || strings.Contains(output, "currently-selected-model") {
		test.Fatalf("timeline lost native evidence or invented producer: exit=%d stderr=%s output=%s", code, stderr, output)
	}
	output, stderr, code = runSurface(test, "", "project", "--root", store.Root, "timeline", created.Project.ID)
	if code != 0 || !strings.Contains(output, "NATIVE-TURN-COMPLETED") || !strings.Contains(output, "unknown") {
		test.Fatalf("text timeline lost completed-turn or unknown liveness: exit=%d stderr=%s output=%s", code, stderr, output)
	}
}

func TestSurfaceProjectEvidenceRefusesReplacementDuringObservation(test *testing.T) {
	for _, withTurn := range []bool{false, true} {
		test.Run(map[bool]string{false: "metadata", true: "settled-turn"}[withTurn], func(test *testing.T) {
			store, created, member := prepareProjectEvidence(test)
			before, err := store.Snapshot(test.Context())
			if err != nil {
				test.Fatal(err)
			}
			installProjectObserver(test, func(context.Context, mail.Worker) (projectNativeObservation, error) {
				replacement := member
				replacement.RunID, replacement.ThreadID = "replacement-run", "replacement-session"
				registerProjectCLIMember(test, created.Ledger, replacement, true)
				observation := projectNativeObservation{Source: project.EvidenceSourceCodexThreadRead, NativeSessionID: member.ThreadID, Provider: "openai", Model: "model"}
				if withTurn {
					observation.Turn = &projectNativeTurn{ID: "old-turn", Status: project.EvidenceCompleted, ObservedAt: time.Now().UTC()}
				}
				return observation, nil
			})
			if report, stderr, code := projectEvidenceCommand(test, store, created, member.RunID, "observe"); code != exitFailed || report.Action != "" || stderr == "" {
				test.Fatalf("replacement accepted stale observation: exit=%d stderr=%s report=%#v", code, stderr, report)
			}
			after, err := store.Snapshot(test.Context())
			if err != nil || after.Revision != before.Revision || len(after.Projects[0].Evidence) != 0 {
				test.Fatalf("stale observation changed state: %v %#v", err, after)
			}
		})
	}
}

func TestSurfaceProjectEvidenceRejectsWrongIdentityBeforeNativeCall(test *testing.T) {
	store, created, member := prepareProjectEvidence(test)
	calls := 0
	installProjectObserver(test, func(context.Context, mail.Worker) (projectNativeObservation, error) {
		calls++
		return projectNativeObservation{NativeSessionID: "foreign-native-session"}, nil
	})
	if _, _, code := projectEvidenceCommand(test, store, created, "old-run", "observe"); code != exitFailed || calls != 0 {
		test.Fatalf("wrong run reached native endpoint: exit=%d calls=%d", code, calls)
	}
	if _, _, code := projectEvidenceCommand(test, store, created, member.RunID, "observe"); code != exitFailed || calls != 1 {
		test.Fatalf("wrong native response accepted: exit=%d calls=%d", code, calls)
	}
}

func TestSurfaceProjectEvidenceArtifactMatchIsSeparateFromTurn(test *testing.T) {
	store, created, member := prepareProjectEvidence(test)
	contents := []byte("checked artifact bytes\n")
	if err := os.WriteFile(filepath.Join(member.Worktree, "result.txt"), contents, 0o600); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	expected := hex.EncodeToString(digest[:])
	report, stderr, code := projectEvidenceCommand(test, store, created, member.RunID, "verify-artifact", "--path", "result.txt", "--sha256", expected)
	if code != 0 || report.Evidence == nil || report.Evidence.Artifact == nil || report.Evidence.Artifact.SHA256 != expected || report.Evidence.Source != project.EvidenceSourceArtifactDigest || report.Evidence.TurnID != "" || report.Native != nil || report.ProcessLiveness != project.ProcessLivenessUnknown {
		test.Fatalf("artifact evidence overclaims or missing: exit=%d stderr=%s report=%#v", code, stderr, report)
	}
	for _, flags := range [][]string{
		{"--path", "../result.txt", "--sha256", expected},
		{"--path", "result.txt", "--sha256", strings.Repeat("0", 64)},
	} {
		if _, _, code := projectEvidenceCommand(test, store, created, member.RunID, "verify-artifact", flags...); code == 0 {
			test.Fatalf("unsafe/mismatched artifact accepted: %v", flags)
		}
	}
	after, err := store.Snapshot(test.Context())
	if err != nil || after.Revision != report.Revision || len(after.Projects[0].Evidence) != 1 {
		test.Fatalf("rejected artifact changed state: %v %#v", err, after)
	}
	output, stderr, code := runSurface(test, "", "project", "--root", store.Root, "timeline", created.Project.ID, "--json")
	if code != 0 || !strings.Contains(output, "ARTIFACT-BYTES-MATCHED") || strings.Contains(output, "NATIVE-TURN-COMPLETED") {
		test.Fatalf("artifact became native completion: exit=%d stderr=%s output=%s", code, stderr, output)
	}
	output, stderr, code = runSurface(test, "", "project", "--root", store.Root, "thread", "--project", created.Project.ID, "verify-artifact", created.Project.CoordinatorThreadID, "--run-id", member.RunID, "--path", "result.txt", "--sha256", expected)
	if code != 0 || !strings.Contains(output, "matched SHA-256") || !strings.Contains(output, "correctness, authorship, and continued freshness not evaluated") {
		test.Fatalf("text artifact claim is missing or too broad: exit=%d stderr=%s output=%s", code, stderr, output)
	}
}

func TestSurfaceProjectEvidenceJSONUsageErrors(test *testing.T) {
	isolateJSONContractEnv(test)
	for _, action := range []string{"observe", "verify-artifact"} {
		output, stderr, err := runJSONThroughFang(test, productionJSONRoot(&exec.FakeRunner{}), "surface", "project", "thread", action, "thread-id", "--json")
		assertProjectJSONError(test, output, stderr, err, jsonCodeUsage, exitUsage)
	}
}
