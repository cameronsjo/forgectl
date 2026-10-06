package launch

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// BinarySource names the configuration layer that selected a harness binary.
//
// The path alone is not enough for every caller. `forgectl surface` starts the
// harness inside a terminal manager the operator is not watching, so it treats
// an env var or a config key as a deliberate assertion ("run this wrapper") and
// a bare PATH hit as ambient — accepting the first two and requiring an opt-in
// for the third. That distinction has to survive resolution to be actionable,
// which is why this travels beside Path rather than being re-derived later.
//
// It is provenance, not authenticity: an explicitly selected wrapper is allowed
// precisely because the operator asked for it, and nothing here proves the file
// is an official harness build.
type BinarySource string

const (
	BinaryClaudeEnv    BinarySource = "claude-env"
	BinaryClaudeConfig BinarySource = "claude-config"
	BinaryCodexEnv     BinarySource = "codex-env"
	BinaryCodexConfig  BinarySource = "codex-config"
	BinaryPiEnv        BinarySource = "pi-env"
	BinaryPiConfig     BinarySource = "pi-config"
	BinaryPATH         BinarySource = "path"
)

// ResolvedBinary is a harness binary plus the layer that chose it.
type ResolvedBinary struct {
	Path   string
	Source BinarySource
}

// Invocation is everything needed to start one harness process: which harness,
// which binary, the full argv after that binary, the complete environment, and
// the directory to run in.
//
// BuildInvocation clones every slice it is handed and every slice it returns,
// so a built Invocation shares no backing array with its caller's inputs. That
// bounds the aliasing that matters here — the surface coordinator holds an
// Invocation across a handshake and a cancellation window, and an argv that
// changed underneath it would be undetectable. It is not deep immutability: the
// fields are exported and a holder can still write to them.
type Invocation struct {
	Harness string
	Binary  ResolvedBinary
	Args    []string
	Env     []string
	CWD     string
}

// Posture names the argv shape BuildInvocation selected. It is returned rather
// than acted on because the two consumers want different things from it:
// `forgectl launch` banners the posture to stderr, and `forgectl surface`
// deliberately banners nothing, because its stderr belongs to whatever terminal
// manager is hosting the session.
type Posture string

const (
	PostureClaudeSession     Posture = "claude-session"
	PostureClaudeBuilder     Posture = "claude-builder"
	PostureClaudeAgents      Posture = "claude-agents"
	PostureAgentsPassthrough Posture = "agents-passthrough"
	PostureClaudePassthrough Posture = "claude-passthrough" //nolint:gosec // G101: a posture name ("passthrough"), not a credential
	PostureClaudePrint       Posture = "claude-print"
	PostureCodexSession      Posture = "codex-session"
	PostureCodexExec         Posture = "codex-exec"
	PosturePiSession         Posture = "pi-session"
	PosturePiArgs            Posture = "pi-args"
)

// BinaryResolver maps a harness name and its config defaults to a binary. The
// production implementation is ResolveBinary; the surface module wraps that one
// in its own policy, which is why the builder takes the function rather than
// calling ResolveBinary directly.
type BinaryResolver func(harness string, defaults config.LaunchDefaults) (ResolvedBinary, error)

// InvocationRequest is BuildInvocation's input: the effective post-migration
// launch config, the target directory, the operator's passthrough args, an
// environment snapshot, any injected environment to sit beneath the profile's,
// and the resolver to use.
type InvocationRequest struct {
	Config config.LaunchConfig
	// CWD is the directory whose profile applies AND the directory the harness
	// runs in. There is deliberately no second caller-set cwd: two of them would
	// permit resolving one project's posture and running it in another.
	CWD         string
	Args        []string
	BaseEnv     []string
	InjectedEnv map[string]string
	// UnsetEnv names variables to REMOVE from BaseEnv rather than override.
	// InjectedEnv cannot express removal, and setting a variable empty is not
	// the same as not setting it — see StripEnv. The profile's own Env still
	// wins over a removal, because it is the operator naming a value explicitly.
	UnsetEnv []string
	Resolve  BinaryResolver
	// Harness, when set, replaces the harness of the profile matched by CWD.
	// Only claude and codex are accepted: pi has no permission or sandbox flag
	// forgectl can pass, so an override to it would start an agent with no
	// posture at all. The override changes no posture field: the matched
	// profile's permission mode, allow_danger, approval policy and sandbox all
	// stay. Those fields are per harness, so a repo block that set only claude
	// fields gives a codex override the codex values from [launch.defaults];
	// the worker profile (T5) is what compares the two.
	Harness string
	// Worker marks a coordinator's worker launch. It applies a floor under
	// the resolved posture; see applyWorkerFloor.
	Worker bool
	// Prompt is a worker's first brief. It goes last in the argv, after a
	// `--`, so the harness starts its first turn with it and no keystroke is
	// typed into its TUI. Only a worker takes one; the caller validates its
	// text (worker.CheckBrief).
	Prompt string
	// StdoutTerminal reports whether the harness's stdout (forgectl's own,
	// since launch execs it) is a terminal. It decides whether
	// `--output-format` alone selects the print posture (IsClaudePrintMode,
	// forgectl#795). The zero value, not a terminal, keeps it print mode.
	// Off a terminal the session, agents, and builder postures also withhold
	// --allow-dangerously-skip-permissions (forgectl#812, #899).
	StdoutTerminal bool
}

var (
	// ErrHarnessOverride reports a --harness value outside the overridable set.
	ErrHarnessOverride = errors.New("launch: harness override must be claude or codex")
	// ErrWorkerPosture reports a resolved posture a worker may not start with.
	ErrWorkerPosture = errors.New("launch: this posture is not allowed for a worker")
)

// Worker posture allowlists. They are the plan's v1 worker posture and
// everything stricter: a claude worker whose shell commands still prompt, and
// a codex worker that can write only its workspace and asks before anything
// else. Anything not listed is refused, so a mode Claude Code or Codex adds
// later is refused until someone decides it is safe for an unattended worker.
var (
	workerPermissionModes = []string{"plan", "default", "acceptEdits"}
	workerSandboxes       = []string{"read-only", "workspace-write"}
	workerApprovals       = []string{"untrusted", "on-request"}
)

// applyWorkerFloor is the worker posture until the worker profile (T5) lands.
//
// Workers run unattended in panes the operator is not watching. pi is refused
// whichever way it was chosen, because forgectl can pass it no permission or
// sandbox flag. A posture outside the allowlists above is refused rather than
// quietly narrowed, so the operator sees the conflict. allow_danger is turned
// off rather than refused: it is on by default, and refusing it would refuse
// every worker on a default config.
func applyWorkerFloor(p Profile) (Profile, error) {
	switch p.Harness {
	case "claude":
		if !oneOf(p.PermissionMode, workerPermissionModes...) {
			return Profile{}, fmt.Errorf("%w: permission_mode %q (workers allow %s)",
				ErrWorkerPosture, p.PermissionMode, strings.Join(workerPermissionModes, ", "))
		}
	case "codex":
		if !oneOf(p.Sandbox, workerSandboxes...) {
			return Profile{}, fmt.Errorf("%w: sandbox %q (workers allow %s)",
				ErrWorkerPosture, p.Sandbox, strings.Join(workerSandboxes, ", "))
		}
		if !oneOf(p.ApprovalPolicy, workerApprovals...) {
			return Profile{}, fmt.Errorf("%w: approval_policy %q (workers allow %s)",
				ErrWorkerPosture, p.ApprovalPolicy, strings.Join(workerApprovals, ", "))
		}
	default:
		return Profile{}, fmt.Errorf("%w: %s has no permission or sandbox flag forgectl can pass", ErrWorkerPosture, p.Harness)
	}
	p.AllowDanger = false
	// A worker edits its own worktree. Extra directories from the repo profile
	// would let acceptEdits or workspace-write reach past it, so they are dropped
	// until the worker profile (T5) decides otherwise.
	p.AddDir = nil
	return p, nil
}

// workerClaudeSettings is the inline settings JSON every claude worker gets.
//
// useAutoModeDuringPlan defaults to true: when auto mode is available, a
// plan-mode session sends shell commands to the auto-mode classifier instead
// of prompting. A worker runs where nobody watches the prompt, so the
// classifier would be the only check on its shell (forgectl#1060). Off, a
// plan-mode worker's commands prompt, and the prompt is a blocking screen the
// coordinator reports.
//
// This is the only --settings a worker gets. Claude Code's handling of a
// repeated --settings flag is unverified, so a second source (T5's worker
// settings file) must merge its keys into this one value, not add a flag.
const workerClaudeSettings = `{"useAutoModeDuringPlan":false}`

// withWorkerSettings inserts `--settings <workerClaudeSettings>` right after
// the posture's leading --permission-mode pair.
//
// A worker takes no harness args (BuildInvocation refuses them), so its
// posture is always the session posture, which starts with that pair. The
// anchor is therefore index 0 and nowhere else: matching the first
// --permission-mode anywhere could anchor on a user token in a passthrough
// argv. Any other shape means a posture builder changed, and the launch is
// refused rather than started without the setting.
func withWorkerSettings(args []string) ([]string, error) {
	if len(args) < 2 || args[0] != "--permission-mode" {
		return nil, fmt.Errorf("%w: worker argv %q does not start with --permission-mode, so --settings has no anchor",
			ErrWorkerPosture, args)
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args[:2]...)
	out = append(out, "--settings", workerClaudeSettings)
	return append(out, args[2:]...), nil
}

// applyHarnessOverride switches p to harness while keeping every posture field.
//
// The model is the one field that does not carry across: a model chosen for
// the profile's own harness means nothing (or the wrong thing) to another, so a
// switch takes the target harness's built-in model and re-derives effort from
// it. An override naming the profile's own harness changes nothing at all.
func applyHarnessOverride(p Profile, harness string) (Profile, error) {
	if harness == "" {
		return p, nil
	}
	if harness != "claude" && harness != "codex" {
		return Profile{}, ErrHarnessOverride
	}
	if harness == p.Harness {
		return p, nil
	}
	p.Harness = harness
	p.Model = builtinModelForHarness(harness)
	p.Effort = EffortForModel(p.Model)
	return p, nil
}

// BuiltInvocation is the invocation plus the two things the caller needs to
// finish the job: the profile it resolved from, and the posture it chose.
type BuiltInvocation struct {
	Invocation Invocation
	Profile    Profile
	Posture    Posture
}

// ErrNoBinaryResolver reports a request with no resolver. Refusing beats
// defaulting to ResolveBinary: a surface launch that silently lost its
// policy-wrapped resolver would run the harness anyway, and a launch that
// ignored the policy is indistinguishable from one that honored it.
var ErrNoBinaryResolver = errors.New("launch: invocation request has no binary resolver")

// BuildInvocation reduces the launch config against req.CWD, chooses the argv
// posture, resolves the binary, and merges the environment — returning data.
// It starts no process, prints nothing, walks no project tree, and touches no
// terminal surface; those belong to its callers. It does read the filesystem:
// resolving the profile follows symlinks on req.CWD, and the default resolver
// stats the binary it selects.
//
// Every refusal runs before the resolver, so a rejected invocation never
// reports a binary-resolution problem it was not going to reach.
func BuildInvocation(req InvocationRequest) (BuiltInvocation, error) {
	if req.Resolve == nil {
		return BuiltInvocation{}, ErrNoBinaryResolver
	}

	profile, err := Resolve(req.Config, req.CWD)
	if err != nil {
		return BuiltInvocation{}, err
	}
	if profile, err = applyHarnessOverride(profile, req.Harness); err != nil {
		return BuiltInvocation{}, err
	}
	if err := profile.Validate(); err != nil {
		return BuiltInvocation{}, err
	}
	if req.Prompt != "" && !req.Worker {
		return BuiltInvocation{}, errors.New("launch: only a worker launch takes a prompt")
	}
	if req.Worker {
		// A user arg lands after the posture, where Claude Code's last-flag-wins
		// parsing would let `--permission-mode` or `--settings` undo the floor.
		// The coordinator starts workers with no args, so refuse any.
		if len(req.Args) > 0 {
			return BuiltInvocation{}, fmt.Errorf("%w: workers take no harness args, got %q", ErrWorkerPosture, req.Args)
		}
		if profile, err = applyWorkerFloor(profile); err != nil {
			return BuiltInvocation{}, err
		}
	}

	args := cloneStrings(req.Args)
	posture, harnessArgs, err := selectPosture(profile, args, req.StdoutTerminal)
	if err != nil {
		return BuiltInvocation{}, err
	}
	if req.Worker && profile.Harness == "claude" {
		if harnessArgs, err = withWorkerSettings(harnessArgs); err != nil {
			return BuiltInvocation{}, err
		}
	}
	if req.Prompt != "" {
		// The `--` ends option parsing in both harnesses, so a prompt that
		// starts with '-' or names a subcommand (`mcp`, `update`) stays the
		// prompt. Measured: Claude Code 2.1.289 answered `-- mcp` as a prompt,
		// and Codex 0.160.0 took `-- --help` as one.
		harnessArgs = append(harnessArgs, "--", req.Prompt)
	}

	binary, err := req.Resolve(profile.Harness, req.Config.Defaults)
	if err != nil {
		return BuiltInvocation{}, err
	}

	// One merge, one place: the injected defaults sit under the profile's env,
	// and that single result overlays the process snapshot. Layering it anywhere
	// else too would let an injected value beat the profile value that exists to
	// override it.
	extra := MergeMaps(req.InjectedEnv, profile.Env)
	// Removals apply to the inherited snapshot only, so a profile Env entry
	// naming the same variable still lands — the operator's explicit value
	// outranks an injected default's removal, exactly as it outranks its set.
	base := StripEnv(cloneStrings(req.BaseEnv), req.UnsetEnv)

	return BuiltInvocation{
		Invocation: Invocation{
			Harness: profile.Harness,
			Binary:  binary,
			Args:    harnessArgs,
			Env:     MergeEnv(base, extra),
			CWD:     req.CWD,
		},
		Profile: profile,
		Posture: posture,
	}, nil
}

// selectPosture routes args to the builder that owns them and reports which one
// ran. args is already a private copy, so the passthrough branch can return it
// without aliasing the caller. stdoutTerminal is InvocationRequest's.
func selectPosture(p Profile, args []string, stdoutTerminal bool) (Posture, []string, error) {
	if p.Harness == "pi" {
		if len(args) > 0 && args[0] == "agents" {
			return "", nil, fmt.Errorf(
				"`launch agents` is Claude-only and has no Pi adapter; invoke Pi directly or switch this launch profile to Claude",
			)
		}
		if len(args) == 0 {
			return PosturePiSession, PiArgs(p, nil), nil
		}
		return PosturePiArgs, PiArgs(p, args), nil
	}
	if p.Harness == "codex" {
		if len(args) > 0 && args[0] == "agents" {
			return "", nil, fmt.Errorf(
				"`launch agents` is Claude-only and has no Codex adapter; invoke Codex directly or switch this launch profile to Claude",
			)
		}
		if len(args) == 0 {
			return PostureCodexSession, CodexSessionArgs(p), nil
		}
		return PostureCodexExec, CodexExecArgs(p, args), nil
	}

	// Off a terminal claude runs as if given --print, with no prompt argument
	// too: a bare `claude < /dev/null | cat` exits "Input must be provided
	// either through stdin or as a prompt argument when using --print"
	// (Claude Code 2.1.285), and a piped stdin becomes the prompt. So every
	// Claude posture forgectl injects into (session, agents, builder)
	// withholds the one flag PrintArgs withholds for safety, and allow_danger
	// never makes bypass reachable in an unattended run. The permission mode, model,
	// effort, and add-dirs all stay (forgectl#812, #899). A flag the user types
	// into args is theirs and still passes. p is a copy, so
	// BuiltInvocation.Profile still reports the profile as resolved.
	if !stdoutTerminal {
		p.AllowDanger = false
	}
	switch {
	case len(args) == 0:
		return PostureClaudeSession, SessionArgs(p), nil
	case args[0] == "agents":
		if IsAgentsPassthrough(args) {
			return PostureAgentsPassthrough, args, nil
		}
		return PostureClaudeAgents, AgentsArgs(p, args), nil
	// A subcommand in the first slot can never be a flag's value, so it goes
	// first. Print mode goes before help/version as defence in depth. Help and
	// version only count at args[0], so a later help token cannot reach the
	// passthrough on its own. The order still means that if that check ever
	// widens to scan argv, `-p x --help` keeps its permission mode. A run that
	// selects both, such as `-v -p hi`, gets the print posture.
	case IsClaudeSubcommandCall(args):
		return PostureClaudePassthrough, args, nil
	case IsClaudePrintMode(args, stdoutTerminal):
		return PostureClaudePrint, PrintArgs(p, args), nil
	case IsClaudeHelpOrVersion(args):
		return PostureClaudePassthrough, args, nil
	default:
		return PostureClaudeBuilder, BuilderArgs(p, args), nil
	}
}

// EmitBanner writes the informational launch line for b's posture. Always to
// the caller's writer — `forgectl launch` passes stderr, so a piped stdout
// stays byte-clean.
//
// Codex gets a banner for the same reason Claude does, through a different
// writer: it has no equivalent of the Claude agents banner, so without this a
// Codex launch would leave no record of the argv it ran with — including the
// approval and sandbox posture, which is the part worth auditing.
//
// Four postures stay silent. The builder and print paths are what an operator
// scripts against, and the agents scripting passthrough and the Claude
// passthrough (subcommands, help, version) must reach claude byte-clean with no
// injection and no banner.
// An unrecognised posture banners rather than falling through silently. A
// posture added to selectPosture but forgotten here would otherwise suppress
// the only pre-session record of the argv — including
// --allow-dangerously-skip-permissions — and suppression is invisible, whereas
// an unwanted banner on stderr is obvious the first time anyone runs it.
// Failing toward visibility keeps the omission loud and costs nothing on
// stdout. allPostures pins the known set, so the default should stay dead.
func EmitBanner(w io.Writer, b BuiltInvocation) {
	switch b.Posture {
	case PostureClaudeBuilder, PostureAgentsPassthrough, PostureClaudePassthrough, PostureClaudePrint:
	case PostureClaudeSession, PostureClaudeAgents:
		Banner(w, b.Invocation.Args)
	case PostureCodexSession, PostureCodexExec, PosturePiSession, PosturePiArgs:
		HarnessBanner(w, b.Invocation.Harness, b.Invocation.Args)
	default:
		HarnessBanner(w, b.Invocation.Harness, b.Invocation.Args)
	}
}

// allPostures is every posture selectPosture can return. It exists so a test
// can prove EmitBanner classifies each one explicitly — adding a Posture
// constant without adding it here fails that test rather than silently landing
// in EmitBanner's default.
var allPostures = []Posture{
	PostureClaudeSession,
	PostureClaudeBuilder,
	PostureClaudeAgents,
	PostureAgentsPassthrough,
	PostureClaudePassthrough,
	PostureClaudePrint,
	PostureCodexSession,
	PostureCodexExec,
	PosturePiSession,
	PosturePiArgs,
}

// ResolveBinary resolves a harness binary and reports which layer chose it, in
// the same env-over-config-over-PATH order — and with the same validation and
// error text — as the path-only wrappers below, which delegate here.
// One resolution path is the point: ten call sites across the repo resolve a
// harness binary, and a second implementation that drifted would leave two of
// them running different binaries with no error anywhere.
// An unknown harness is refused rather than falling through to the Claude
// ladder. A silent fallback would read the Claude env var and config key for a
// harness nobody asked for and stamp a claude-* provenance on the result — a
// Source that agrees with how Path was chosen while disagreeing with what was
// requested, which is precisely the lie the surface policy would then gate on.
// Unreachable through BuildInvocation (Profile.Validate refuses first), but this
// is exported for a second consumer whose own policy wrapper calls it directly.
func ResolveBinary(harness string, defaults config.LaunchDefaults) (ResolvedBinary, error) {
	switch harness {
	case "claude":
		return resolveLayered(layered{
			envKey:      "FORGECTL_CLAUDE_BIN",
			envSource:   BinaryClaudeEnv,
			configPath:  defaults.BinaryPath,
			configLabel: "[launch.defaults] binary_path",
			configSrc:   BinaryClaudeConfig,
			name:        "claude",
		})
	case "codex":
		return resolveLayered(layered{
			envKey:      "FORGECTL_CODEX_BIN",
			envSource:   BinaryCodexEnv,
			configPath:  defaults.CodexBinaryPath,
			configLabel: "[launch.defaults] codex_binary_path",
			configSrc:   BinaryCodexConfig,
			name:        "codex",
		})
	case "pi":
		return resolveLayered(layered{
			envKey:      "FORGECTL_PI_BIN",
			envSource:   BinaryPiEnv,
			configPath:  defaults.PiBinaryPath,
			configLabel: "[launch.defaults] pi_binary_path",
			configSrc:   BinaryPiConfig,
			name:        "pi",
		})
	default:
		return ResolvedBinary{}, fmt.Errorf("unsupported launch harness %s: want claude, codex, or pi", termsafe.QuoteArgMax(harness, 0))
	}
}

// cloneStrings returns a copy with no shared backing array, and nil for an
// empty input so a built invocation does not carry an empty-but-non-nil slice
// where the old code carried nil.
func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}
