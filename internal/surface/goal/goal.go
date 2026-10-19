package goal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/surface/goalcontainer"
)

const Version = 1

type Phase string

const (
	PhasePreparing     Phase = "preparing"
	PhaseStarting      Phase = "starting"
	PhaseImplementing  Phase = "implementing"
	PhaseReviewing     Phase = "reviewing"
	PhaseNeedsOperator Phase = "needs_operator"
	PhaseVerifying     Phase = "verifying"
	PhaseStopping      Phase = "stopping"
	PhaseReady         Phase = "ready"
	PhaseFailed        Phase = "failed"
	PhaseCanceled      Phase = "canceled"
	PhaseInterrupted   Phase = "interrupted"
	PhaseUnknown       Phase = "unknown"
)

type Spec struct {
	Version      int            `json:"version"`
	Title        string         `json:"title"`
	Objective    string         `json:"objective"`
	Repository   string         `json:"repository"`
	BaseRevision string         `json:"base_revision"`
	AllowedPaths []string       `json:"allowed_paths"`
	Acceptance   []string       `json:"acceptance"`
	Checks       [][]string     `json:"checks"`
	Model        string         `json:"model"`
	LimitMinutes int            `json:"limit_minutes"`
	Team         *TeamSpec      `json:"team,omitempty"`
	Isolation    *IsolationSpec `json:"isolation,omitempty"`
}

type IsolationSpec = goalcontainer.Spec

type Prepared struct {
	Spec         Spec     `json:"spec"`
	SourceBranch string   `json:"source_branch"`
	SourceDirty  bool     `json:"source_dirty"`
	Warnings     []string `json:"warnings"`
}

type Role struct {
	Name            string `json:"name"`
	ThreadID        string `json:"thread_id"`
	RunID           string `json:"run_id"`
	NativeSessionID string `json:"native_session_id"`
	Socket          string `json:"socket"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	Harness         string `json:"harness,omitempty"`
}

type Question struct {
	ID           string     `json:"id"`
	Role         string     `json:"role"`
	NativeTurnID string     `json:"native_turn_id"`
	Text         string     `json:"text"`
	CreatedAt    time.Time  `json:"created_at"`
	Answer       string     `json:"answer,omitempty"`
	AnswerSHA256 string     `json:"answer_sha256,omitempty"`
	AnsweredAt   *time.Time `json:"answered_at,omitempty"`
}

type CheckResult struct {
	Argv         []string  `json:"argv"`
	ArgvSHA256   string    `json:"argv_sha256"`
	ExitCode     int       `json:"exit_code"`
	Passed       bool      `json:"passed"`
	OutputPath   string    `json:"output_path"`
	OutputSHA256 string    `json:"output_sha256"`
	PatchBefore  string    `json:"patch_before"`
	PatchAfter   string    `json:"patch_after"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
}

type Result struct {
	PatchPath       string   `json:"patch_path"`
	PatchSHA256     string   `json:"patch_sha256"`
	ChangedFiles    []string `json:"changed_files"`
	ReviewTurnID    string   `json:"review_turn_id"`
	ReviewText      string   `json:"review_text"`
	ReviewPassed    bool     `json:"review_passed"`
	ChecksPassed    bool     `json:"checks_passed"`
	SourceUnchanged bool     `json:"source_unchanged"`
	HumanAccepted   bool     `json:"human_accepted"`
}

type State struct {
	Version          int           `json:"version"`
	ID               string        `json:"id"`
	ProjectID        string        `json:"project_id,omitempty"`
	Spec             Spec          `json:"spec"`
	GoalSHA256       string        `json:"goal_sha256"`
	Phase            Phase         `json:"phase"`
	Summary          string        `json:"summary"`
	Root             string        `json:"root"`
	RunDir           string        `json:"run_dir"`
	Worktree         string        `json:"worktree"`
	Branch           string        `json:"branch"`
	Ledger           string        `json:"ledger,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
	HeartbeatAt      time.Time     `json:"heartbeat_at"`
	Deadline         time.Time     `json:"deadline"`
	SupervisorActive bool          `json:"supervisor_active"`
	CleanupComplete  bool          `json:"cleanup_complete"`
	CancelRequested  bool          `json:"cancel_requested"`
	LastControl      uint64        `json:"last_control"`
	Roles            []Role        `json:"roles"`
	Question         *Question     `json:"question,omitempty"`
	Questions        []Question    `json:"questions"`
	Checks           []CheckResult `json:"checks"`
	Result           *Result       `json:"result,omitempty"`
	Error            string        `json:"error,omitempty"`
}

type Control struct {
	Version    int       `json:"version"`
	GoalID     string    `json:"goal_id"`
	Sequence   uint64    `json:"sequence"`
	Action     string    `json:"action"`
	QuestionID string    `json:"question_id,omitempty"`
	Answer     string    `json:"answer,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Event struct {
	Time    time.Time       `json:"time"`
	Kind    string          `json:"kind"`
	Role    string          `json:"role,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

var idPattern = regexp.MustCompile(`^goal-[a-f0-9]{24}$`)
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

func (phase Phase) Terminal() bool {
	return phase == PhaseReady || phase == PhaseFailed || phase == PhaseCanceled || phase == PhaseInterrupted
}

func (spec Spec) Validate() error {
	if spec.Version != Version {
		return errors.New("goal version must be 1")
	}
	if !safeText(spec.Title, 128, false) {
		return errors.New("title must be a nonempty single line of at most 128 bytes")
	}
	if !safeText(spec.Objective, 12*1024, true) {
		return errors.New("objective must be nonempty UTF-8 text of at most 12 KiB")
	}
	if !filepath.IsAbs(spec.Repository) || filepath.Clean(spec.Repository) != spec.Repository || !safeText(spec.Repository, 4096, false) {
		return errors.New("repository must be an absolute clean local path")
	}
	if spec.BaseRevision != "" && !revisionPattern.MatchString(spec.BaseRevision) {
		return errors.New("base_revision must be an exact Git object ID")
	}
	if spec.Team == nil && config.CheckModelName(spec.Model) != nil {
		return errors.New("an exact OpenAI model ID is required")
	}
	if spec.Team != nil {
		if err := spec.Team.Validate(); err != nil {
			return err
		}
		if spec.Model != "" && config.CheckModelName(spec.Model) != nil {
			return errors.New("legacy model must be empty or a plain model ID")
		}
	}
	if spec.Isolation != nil {
		if spec.Team == nil {
			return errors.New("container isolation requires an explicit native team so every tool uses the approved boundary")
		}
		if err := spec.Isolation.Validate(); err != nil {
			return err
		}
	}
	if spec.LimitMinutes < 1 || spec.LimitMinutes > 30 {
		return errors.New("limit_minutes must be between 1 and 30 (not a spending cap)")
	}
	if len(spec.AllowedPaths) < 1 || len(spec.AllowedPaths) > 32 {
		return errors.New("provide 1-32 repository-relative allowed paths; . explicitly allows repository-wide changes")
	}
	seen := map[string]bool{}
	for _, allowed := range spec.AllowedPaths {
		if !validScope(allowed) || seen[allowed] {
			return fmt.Errorf("invalid or duplicate allowed path %q; use literal files or directories, not globs", allowed)
		}
		seen[allowed] = true
	}
	if len(spec.Acceptance) < 1 || len(spec.Acceptance) > 16 {
		return errors.New("provide 1-16 acceptance criteria")
	}
	for _, criterion := range spec.Acceptance {
		if !safeText(criterion, 4096, true) {
			return errors.New("acceptance criteria must be bounded nonempty text")
		}
	}
	if len(spec.Checks) < 1 || len(spec.Checks) > 8 {
		return errors.New("provide 1-8 explicit verification argument arrays")
	}
	for _, argv := range spec.Checks {
		if len(argv) < 1 || len(argv) > 64 || strings.HasPrefix(argv[0], "-") {
			return errors.New("a verification command needs an executable and at most 63 arguments")
		}
		for _, argument := range argv {
			if !safeArgument(argument) {
				return errors.New("verification arguments must be bounded UTF-8 without control characters")
			}
		}
		if argv[0] == "" {
			return errors.New("verification executable must not be empty")
		}
	}
	// termsafe:allow-raw-json bounded goal digest input, never terminal output
	encoded, err := json.Marshal(spec)
	if err != nil || len(encoded) > 32*1024 {
		return errors.New("goal exceeds 32 KiB")
	}
	return nil
}

func validScope(value string) bool {
	if value == "." {
		return true
	}
	if value == "" || len(value) > 4096 || path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\\*?[]{}") || !safeText(value, 4096, false) {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." || component == "." || component == "" || protectedComponent(component) {
			return false
		}
	}
	return true
}

func protectedComponent(component string) bool {
	lower := strings.ToLower(component)
	return lower == ".git" || lower == ".codex" || lower == ".agents" || lower == ".claude" || lower == ".env" || strings.HasPrefix(lower, ".env.")
}

func Allowed(file string, scopes []string) bool {
	if !validScope(file) || file == "." {
		return false
	}
	for _, scope := range scopes {
		if scope == "." || file == scope || strings.HasPrefix(file, scope+"/") {
			return true
		}
	}
	return false
}

func safeText(value string, maximum int, multiline bool) bool {
	if strings.TrimSpace(value) == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && (!multiline || (character != '\n' && character != '\t')) {
			return false
		}
	}
	return true
}

func safeArgument(value string) bool {
	if len(value) > 4096 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func Digest(bytes []byte) string { sum := sha256.Sum256(bytes); return hex.EncodeToString(sum[:]) }

func newID() (string, error) {
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "goal-" + hex.EncodeToString(bytes), nil
}
