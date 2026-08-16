package launch

import (
	"errors"
	"fmt"
	"io"

	"github.com/cameronsjo/forgectl/internal/config"
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
	PostureCodexSession      Posture = "codex-session"
	PostureCodexExec         Posture = "codex-exec"
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
	Resolve     BinaryResolver
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

	profile := Resolve(req.Config, req.CWD)
	if err := profile.Validate(); err != nil {
		return BuiltInvocation{}, err
	}

	args := cloneStrings(req.Args)
	posture, harnessArgs, err := selectPosture(profile, args)
	if err != nil {
		return BuiltInvocation{}, err
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

	return BuiltInvocation{
		Invocation: Invocation{
			Harness: profile.Harness,
			Binary:  binary,
			Args:    harnessArgs,
			Env:     MergeEnv(cloneStrings(req.BaseEnv), extra),
			CWD:     req.CWD,
		},
		Profile: profile,
		Posture: posture,
	}, nil
}

// selectPosture routes args to the builder that owns them and reports which one
// ran. args is already a private copy, so the passthrough branch can return it
// without aliasing the caller.
func selectPosture(p Profile, args []string) (Posture, []string, error) {
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

	switch {
	case len(args) == 0:
		return PostureClaudeSession, SessionArgs(p), nil
	case args[0] == "agents":
		if IsAgentsPassthrough(args) {
			return PostureAgentsPassthrough, args, nil
		}
		return PostureClaudeAgents, AgentsArgs(p, args), nil
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
// Two postures stay silent. The builder path is what an operator scripts
// against, and the agents scripting passthrough must reach claude byte-clean
// with no injection and no banner.
// An unrecognised posture banners rather than falling through silently. A
// posture added to selectPosture but forgotten here would otherwise suppress
// the only pre-session record of the argv — including
// --allow-dangerously-skip-permissions — and suppression is invisible, whereas
// an unwanted banner on stderr is obvious the first time anyone runs it.
// Failing toward visibility keeps the omission loud and costs nothing on
// stdout. allPostures pins the known set, so the default should stay dead.
func EmitBanner(w io.Writer, b BuiltInvocation) {
	switch b.Posture {
	case PostureClaudeBuilder, PostureAgentsPassthrough:
	case PostureClaudeSession, PostureClaudeAgents:
		Banner(w, b.Invocation.Args)
	case PostureCodexSession, PostureCodexExec:
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
	PostureCodexSession,
	PostureCodexExec,
}

// ResolveBinary resolves a harness binary and reports which layer chose it, in
// the same env-over-config-over-PATH order — and with the same validation and
// error text — as the ClaudePath/CodexPath wrappers below, which delegate here.
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
	default:
		return ResolvedBinary{}, fmt.Errorf("unsupported launch harness %q: want claude or codex", harness)
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
