package launch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

const testHome = "/home/u"

func resolveAt(lc config.LaunchConfig, cwd string) Profile {
	return resolve(lc, cwd, testHome)
}

func TestResolve_NoProjects_UsesBuiltinDefaults(t *testing.T) {
	got := resolveAt(config.LaunchConfig{}, "/home/u/somewhere")
	if got.Model != "opus" {
		t.Errorf("Model = %q, want %q", got.Model, "opus")
	}
	if got.PermissionMode != "plan" {
		t.Errorf("PermissionMode = %q, want %q", got.PermissionMode, "plan")
	}
	if got.AllowDanger != true {
		t.Errorf("AllowDanger = %v, want true", got.AllowDanger)
	}
	if got.Match != "" {
		t.Errorf("Match = %q, want empty", got.Match)
	}
}

// TestResolve_BuiltinCodexPostureIsNonWriting pins the harness-default half of
// the "launch always starts in plan" invariant. A user who sets only
// `harness = "codex"` must not get unattended write access to the checkout, so
// the built-in sandbox has to be read-only — workspace-write is an opt-up.
func TestResolve_BuiltinCodexPostureIsNonWriting(t *testing.T) {
	lc := config.LaunchConfig{Defaults: config.LaunchDefaults{Harness: "codex"}}
	got := resolveAt(lc, "/home/u/somewhere")
	if got.Harness != "codex" {
		t.Fatalf("Harness = %q, want %q", got.Harness, "codex")
	}
	if got.Sandbox != "read-only" {
		t.Errorf("Sandbox = %q, want %q — a bare codex profile must not be able to write", got.Sandbox, "read-only")
	}
	if got.ApprovalPolicy != "on-request" {
		t.Errorf("ApprovalPolicy = %q, want %q", got.ApprovalPolicy, "on-request")
	}
}

// TestDefaultsProfile_SandboxOptUp proves workspace-write is still reachable —
// read-only is the default, not a ceiling.
func TestDefaultsProfile_SandboxOptUp(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Harness: "codex", Sandbox: "workspace-write"},
	}
	if got := resolveAt(lc, "/home/u/somewhere"); got.Sandbox != "workspace-write" {
		t.Errorf("Sandbox = %q, want %q", got.Sandbox, "workspace-write")
	}
}

func TestResolve_DefaultsOverrideBuiltins(t *testing.T) {
	no := false
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "sonnet", PermissionMode: "acceptEdits", AllowDanger: &no},
	}
	got := resolveAt(lc, "/home/u/somewhere")
	if got.Model != "sonnet" {
		t.Errorf("Model = %q, want %q", got.Model, "sonnet")
	}
	if got.PermissionMode != "acceptEdits" {
		t.Errorf("PermissionMode = %q, want %q", got.PermissionMode, "acceptEdits")
	}
	if got.AllowDanger != false {
		t.Errorf("AllowDanger = %v, want false", got.AllowDanger)
	}
}

func TestResolve_LongestPrefixWins(t *testing.T) {
	lc := config.LaunchConfig{
		Projects: []config.LaunchProject{
			{Match: "~/Projects", Model: "sonnet"},
			{Match: "~/Projects/minute", Model: "haiku"},
		},
	}
	got := resolveAt(lc, "/home/u/Projects/minute/sub")
	if got.Model != "haiku" {
		t.Errorf("Model = %q, want %q", got.Model, "haiku")
	}
	if got.Match != "~/Projects/minute" {
		t.Errorf("Match = %q, want %q", got.Match, "~/Projects/minute")
	}
}

func TestResolve_ExactMatchCountsAsPrefix(t *testing.T) {
	lc := config.LaunchConfig{
		Projects: []config.LaunchProject{
			{Match: "~/Projects/minute", Model: "haiku"},
		},
	}
	got := resolveAt(lc, "/home/u/Projects/minute")
	if got.Model != "haiku" {
		t.Errorf("Model = %q, want %q", got.Model, "haiku")
	}
}

func TestResolve_ComponentBoundary_NoFalsePrefix(t *testing.T) {
	lc := config.LaunchConfig{
		Projects: []config.LaunchProject{
			{Match: "~/Projects/minute", Model: "haiku"},
		},
	}
	got := resolveAt(lc, "/home/u/Projects/minuteworld")
	if got.Match != "" {
		t.Errorf("Match = %q, want empty (no false prefix match)", got.Match)
	}
	if got.Model != "opus" {
		t.Errorf("Model = %q, want built-in default %q", got.Model, "opus")
	}
}

func TestResolve_ScalarMerge_ProjectWinsWhenSet_DefaultsOtherwise(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus", PermissionMode: "plan"},
		Projects: []config.LaunchProject{
			{Match: "~/p", Model: "sonnet"},
		},
	}
	got := resolveAt(lc, "/home/u/p")
	if got.Model != "sonnet" {
		t.Errorf("Model = %q, want %q", got.Model, "sonnet")
	}
	if got.PermissionMode != "plan" {
		t.Errorf("PermissionMode = %q, want %q (from defaults)", got.PermissionMode, "plan")
	}
}

func TestResolve_AllowDangerOverrideToFalse(t *testing.T) {
	yes := true
	no := false
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AllowDanger: &yes},
		Projects: []config.LaunchProject{
			{Match: "~/p", AllowDanger: &no},
		},
	}
	got := resolveAt(lc, "/home/u/p")
	if got.AllowDanger != false {
		t.Errorf("AllowDanger = %v, want false", got.AllowDanger)
	}
}

func TestResolve_CodexHarnessAndNativePosture(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{
			Harness:        "codex",
			Model:          "gpt-5",
			ApprovalPolicy: "on-request",
			Sandbox:        "workspace-write",
		},
		Projects: []config.LaunchProject{
			{
				Match:          "~/p",
				ApprovalPolicy: "never",
				Sandbox:        "read-only",
			},
		},
	}
	got := resolve(lc, "/home/me/p/repo", "/home/me")
	if got.Harness != "codex" || got.Model != "gpt-5" ||
		got.ApprovalPolicy != "never" || got.Sandbox != "read-only" {
		t.Errorf("resolved Codex posture = %+v", got)
	}
}

func TestResolve_CodexProjectDoesNotInheritClaudeDefaultModel(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus"},
		Projects: []config.LaunchProject{{
			Match:   "~/p",
			Harness: "codex",
		}},
	}
	got := resolve(lc, "/home/me/p/repo", "/home/me")
	if got.Model != "" {
		t.Fatalf("Codex project inherited Claude model: %q", got.Model)
	}
}

func TestResolve_PiHarnessProviderAndModel(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Harness: "pi", Provider: "google", Model: "gemini-2.5-pro"},
		Projects: []config.LaunchProject{{
			Match: "~/p", Provider: "lm-studio", Model: "qwen/qwen3-coder-next",
		}},
	}
	got := resolve(lc, "/home/me/p/repo", "/home/me")
	if got.Harness != "pi" || got.Provider != "lm-studio" || got.Model != "qwen/qwen3-coder-next" {
		t.Errorf("resolved Pi profile = %+v", got)
	}
}

func TestResolve_PiProjectDoesNotInheritClaudeDefaultModel(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus"},
		Projects: []config.LaunchProject{{Match: "~/p", Harness: "pi"}},
	}
	if got := resolveAt(lc, "/home/u/p"); got.Model != "" {
		t.Fatalf("Pi project inherited Claude model: %q", got.Model)
	}
}

// TestProfileValidate_UnsupportedHarness covers Validate's first branch, which
// had no test: an unknown harness must be rejected by name rather than falling
// through to the Claude path, which is what a typo would otherwise get.
func TestProfileValidate_UnsupportedHarness(t *testing.T) {
	for _, harness := range []string{"gemini", "Codex", "claude ", ""} {
		p := Profile{Harness: harness, Model: "gpt-5", ApprovalPolicy: "never", Sandbox: "read-only"}
		err := p.Validate()
		if err == nil {
			t.Errorf("Validate() accepted unsupported harness %q", harness)
			continue
		}
		if !strings.Contains(err.Error(), "want claude, codex, or pi") {
			t.Errorf("Validate() for %q = %v, want the message to name the supported harnesses", harness, err)
		}
	}

	for _, harness := range []string{"claude", "codex", "pi"} {
		p := Profile{Harness: harness, ApprovalPolicy: "never", Sandbox: "read-only"}
		if err := p.Validate(); err != nil {
			t.Errorf("Validate() rejected supported harness %q: %v", harness, err)
		}
	}
}

func TestProfileValidate_CodexEnums(t *testing.T) {
	valid := Profile{
		Harness:        "codex",
		ApprovalPolicy: "never",
		Sandbox:        "read-only",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	valid.ApprovalPolicy = "ask-sometimes"
	if err := valid.Validate(); err == nil {
		t.Fatal("invalid approval policy accepted")
	}
	valid.ApprovalPolicy = "never"
	valid.Sandbox = "host-everything"
	if err := valid.Validate(); err == nil {
		t.Fatal("invalid sandbox accepted")
	}
	valid.Sandbox = "read-only"
	valid.Model = "opus"
	if err := valid.Validate(); err == nil {
		t.Fatal("Claude model accepted for Codex")
	}
}

func TestResolve_EnvMerge_ProjectWins(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Env: map[string]string{"A": "1", "B": "2"}},
		Projects: []config.LaunchProject{
			{Match: "~/p", Env: map[string]string{"B": "3", "C": "4"}},
		},
	}
	got := resolveAt(lc, "/home/u/p")
	want := map[string]string{"A": "1", "B": "3", "C": "4"}
	if !reflect.DeepEqual(got.Env, want) {
		t.Errorf("Env = %v, want %v", got.Env, want)
	}
}

func TestResolve_AddDir_ConcatExpandDedupe(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AddDir: []string{"~/a", "~/shared"}},
		Projects: []config.LaunchProject{
			{Match: "~/p", AddDir: []string{"~/shared", "~/b"}},
		},
	}
	got := resolveAt(lc, "/home/u/p")
	want := []string{"/home/u/a", "/home/u/shared", "/home/u/b"}
	if !reflect.DeepEqual(got.AddDir, want) {
		t.Errorf("AddDir = %v, want %v", got.AddDir, want)
	}
}

func TestResolve_DefaultsOnly_NoMatch_StillExpandsAddDir(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{AddDir: []string{"~/g"}},
	}
	got := resolveAt(lc, "/home/u/elsewhere")
	want := []string{"/home/u/g"}
	if !reflect.DeepEqual(got.AddDir, want) {
		t.Errorf("AddDir = %v, want %v", got.AddDir, want)
	}
}

func TestExpandTilde(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"~", testHome},
		{"~/Projects", "/home/u/Projects"},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"~notme/x", "~notme/x"},
	}
	for _, tc := range cases {
		got := expandTilde(tc.in, testHome)
		if got != tc.want {
			t.Errorf("expandTilde(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── effort ──────────────────────────────────────────────────────────────────

func TestEffortForModel_MappedAliases(t *testing.T) {
	cases := map[string]string{
		"sonnet": "high",
		"opus":   "medium",
		"fable":  "medium",
		// The "[1m]" suffix picks the 1M-token context window, not a different
		// model, so effort carries over. Claude Code 2.1.221 accepts these.
		"sonnet[1m]": "high",
		"opus[1m]":   "medium",
		"fable[1m]":  "medium",
	}
	for model, want := range cases {
		if got := EffortForModel(model); got != want {
			t.Errorf("EffortForModel(%q) = %q, want %q", model, got, want)
		}
	}
}

// TestEffortForModel_UnmappedYieldsNoLevel is the deliberate half of the
// mapping. Emitting no flag leaves settings.json's effortLevel in charge —
// the pre-existing behavior — whereas guessing a level for a model whose
// effort semantics were never measured silently changes how it runs.
//
// opusplan is the pointed case: it runs opus for planning and sonnet for
// execution, and this mapping puts those at DIFFERENT levels, so there is no
// single honest answer to give it.
func TestEffortForModel_UnmappedYieldsNoLevel(t *testing.T) {
	for _, model := range []string{
		"", "haiku", "opusplan", "opusplan[1m]", "claude-opus-5", "gpt-5", "opus[2m]",
	} {
		if got := EffortForModel(model); got != "" {
			t.Errorf("EffortForModel(%q) = %q, want \"\" (unmapped models must emit no --effort)", model, got)
		}
	}
}

func TestResolve_EffortDerivedFromBuiltinModel(t *testing.T) {
	got := resolveAt(config.LaunchConfig{}, "/home/u/somewhere")
	if got.Effort != "medium" {
		t.Errorf("Effort = %q, want %q (derived from the built-in opus)", got.Effort, "medium")
	}
}

func TestResolve_EffortScalarMerge_ProjectWinsWhenSet_DefaultsOtherwise(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus", Effort: "low"},
		Projects: []config.LaunchProject{
			{Match: "~/p", Effort: "max"},
			{Match: "~/q"},
		},
	}
	if got := resolveAt(lc, "/home/u/p"); got.Effort != "max" {
		t.Errorf("Effort = %q, want %q (project override)", got.Effort, "max")
	}
	if got := resolveAt(lc, "/home/u/q"); got.Effort != "low" {
		t.Errorf("Effort = %q, want %q (from defaults)", got.Effort, "low")
	}
}

// TestResolve_ExplicitEffortBeatsDerivation pins the precedence at both
// layers: a configured level outranks whatever the model would derive.
func TestResolve_ExplicitEffortBeatsDerivation(t *testing.T) {
	defaultsOnly := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "sonnet", Effort: "low"}, // sonnet derives "high"
	}
	if got := resolveAt(defaultsOnly, "/home/u/x"); got.Effort != "low" {
		t.Errorf("Effort = %q, want %q — [launch.defaults] effort must beat derivation", got.Effort, "low")
	}

	projectOnly := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus"},
		Projects: []config.LaunchProject{{Match: "~/p", Model: "sonnet", Effort: "low"}},
	}
	if got := resolveAt(projectOnly, "/home/u/p"); got.Effort != "low" {
		t.Errorf("Effort = %q, want %q — a project effort must beat its own model's derivation", got.Effort, "low")
	}
}

// TestResolve_ProjectModelOverride_RederivesWhenNoExplicitEffort is the case
// the "derive LAST, against the FINAL model" ordering exists for. The fixture
// deliberately sets NO defaults-effort — with one set, the explicit value wins
// and no re-derivation happens (that is the converse test below).
func TestResolve_ProjectModelOverride_RederivesWhenNoExplicitEffort(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus"}, // would derive "medium"
		Projects: []config.LaunchProject{{Match: "~/p", Model: "sonnet"}},
	}
	if got := resolveAt(lc, "/home/u/p"); got.Effort != "high" {
		t.Errorf("Effort = %q, want %q — a project overriding only `model` must re-derive", got.Effort, "high")
	}
}

// TestResolve_DefaultsEffortSurvivesProjectModelOverride is the converse: an
// explicit [launch.defaults] effort is a deliberate global floor, so a project
// changing only its model inherits that level rather than re-deriving.
func TestResolve_DefaultsEffortSurvivesProjectModelOverride(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus", Effort: "xhigh"},
		Projects: []config.LaunchProject{{Match: "~/p", Model: "sonnet"}},
	}
	if got := resolveAt(lc, "/home/u/p"); got.Effort != "xhigh" {
		t.Errorf("Effort = %q, want %q — an explicit defaults effort must survive a project model override", got.Effort, "xhigh")
	}
}

// TestResolve_ProjectModelOverrideToUnmappedClearsDerivedEffort: switching to
// an unmapped model must CLEAR the level the defaults' model derived, not
// carry it across. Reading p.Effort back would silently keep "medium" here.
func TestResolve_ProjectModelOverrideToUnmappedClearsDerivedEffort(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus"},
		Projects: []config.LaunchProject{{Match: "~/p", Model: "haiku"}},
	}
	if got := resolveAt(lc, "/home/u/p"); got.Effort != "" {
		t.Errorf("Effort = %q, want \"\" — an unmapped project model must not inherit the defaults' derived level", got.Effort)
	}
}

func TestDefaultsProfile_DerivesEffortFromDefaultsModel(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "sonnet"},
		// A project block must not influence DefaultsProfile at all.
		Projects: []config.LaunchProject{{Match: "~/", Model: "haiku"}},
	}
	if got := DefaultsProfile(lc); got.Effort != "high" {
		t.Errorf("Effort = %q, want %q", got.Effort, "high")
	}
}

// TestProfileValidate_EffortEnum pins the five levels `claude --help` documents
// (2.1.221: "low, medium, high, xhigh, max"). Empty is valid and means "emit no
// flag" — an unmapped model resolves to exactly that, so rejecting it would
// make every haiku profile unlaunchable.
func TestProfileValidate_EffortEnum(t *testing.T) {
	for _, level := range EffortLevels {
		p := Profile{Harness: "claude", Effort: level}
		if err := p.Validate(); err != nil {
			t.Errorf("effort %q must be accepted: %v", level, err)
		}
	}
	if err := (Profile{Harness: "claude"}).Validate(); err != nil {
		t.Errorf("an empty effort means \"emit no flag\" and must be accepted: %v", err)
	}
	for _, bad := range []string{"hihg", "HIGH", "maximum", "1", "veryhigh"} {
		err := (Profile{Harness: "claude", Effort: bad}).Validate()
		if err == nil {
			t.Errorf("effort %q must be rejected", bad)
			continue
		}
		if !strings.Contains(err.Error(), "xhigh") {
			t.Errorf("the error for %q must name the accepted set, got %q", bad, err)
		}
	}
}

// TestProfileValidate_EffortCheckedOnCodexToo: effort is inert under Codex
// (no --effort flag, and the Codex builders never emit one), so a typo there
// is silent until the profile flips harness — or until the clean-room review
// forces harness to claude. Validate it in both postures.
func TestProfileValidate_EffortCheckedOnCodexToo(t *testing.T) {
	p := Profile{Harness: "codex", ApprovalPolicy: "never", Sandbox: "read-only", Effort: "hihg"}
	if err := p.Validate(); err == nil {
		t.Error("a bad effort must be rejected on a Codex profile too, where it is otherwise inert")
	}
}

// TestEffortForModel_OnlyYieldsValidLevels closes the loop between the two
// halves of this feature: every level the derivation can produce must survive
// the validator. A mapping entry added without a matching enum entry would
// make an untouched config fail at launch.
func TestEffortForModel_OnlyYieldsValidLevels(t *testing.T) {
	for _, model := range []string{"sonnet", "opus", "fable", "sonnet[1m]", "opus[1m]", "fable[1m]", "haiku", ""} {
		derived := EffortForModel(model)
		if err := (Profile{Harness: "claude", Effort: derived}).Validate(); err != nil {
			t.Errorf("EffortForModel(%q) = %q, which Validate rejects: %v", model, derived, err)
		}
	}
}

// TestResolve_ExplicitDefaultsEffortSurvivesHarnessOnlyOverride closes the
// fixture gap the review flagged: a project block overriding only `harness`
// clears the Model (the harness switch re-derives it), so the effort path has
// to be checked separately from the model-override cases. An explicit
// defaults-effort is a global floor and survives; it is inert under Codex,
// which is why Validate checks it there too.
func TestResolve_ExplicitDefaultsEffortSurvivesHarnessOnlyOverride(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "opus", Effort: "xhigh"},
		Projects: []config.LaunchProject{{Match: "~/p", Harness: "codex"}},
	}
	got := resolveAt(lc, "/home/u/p")
	if got.Harness != "codex" {
		t.Fatalf("Harness = %q, want codex", got.Harness)
	}
	if got.Model != "" {
		t.Errorf("Model = %q, want \"\" (the harness switch re-derives it)", got.Model)
	}
	if got.Effort != "xhigh" {
		t.Errorf("Effort = %q, want %q — an explicit defaults effort is a floor, not a per-model value", got.Effort, "xhigh")
	}
}

// TestResolve_HarnessOnlyOverride_NoExplicitEffort_ClearsDerived is the
// converse: with no explicit effort, a harness-only switch clears the Model,
// so the derived level must clear with it rather than stay pinned to the
// Claude model it came from.
func TestResolve_HarnessOnlyOverride_NoExplicitEffort_ClearsDerived(t *testing.T) {
	lc := config.LaunchConfig{
		Defaults: config.LaunchDefaults{Model: "sonnet"}, // derives "high"
		Projects: []config.LaunchProject{{Match: "~/p", Harness: "codex"}},
	}
	if got := resolveAt(lc, "/home/u/p"); got.Effort != "" {
		t.Errorf("Effort = %q, want \"\" — a cleared model must clear its derived level", got.Effort)
	}
}
