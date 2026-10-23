# k8s Specification

## Purpose
`forgectl k8s` wraps ordinary kubectl calls for logs, namespace, exec, and inspection, forwarding kubectl arguments structurally and rendering untrusted cluster output terminal-safe.

## Requirements

### Requirement: logs forwards kubectl arguments verbatim
`forgectl k8s logs` MUST run exactly one `kubectl logs` with every argument forwarded token for token and no shell. forgectl SHALL consume only `--log-level` and `--color` (spaced or `=` form) before the first `--`; after it, those names MUST reach kubectl. A call with no kubectl arguments MUST be refused.

#### Scenario: Same-named flag after the separator
- **WHEN** the operator runs `forgectl k8s logs pod/api -- --color x`
- **THEN** kubectl receives `logs pod/api --color x`

#### Scenario: No resource
- **WHEN** the operator runs `forgectl k8s logs --log-level warn`
- **THEN** the command refuses because kubectl logs requires a resource or selector

### Requirement: Severity floor filters only recognized JSON
`--log-level` SHALL accept `trace`, `debug`, `info`, `warn`, `error`, `fatal` and refuse anything else. A line MUST be suppressed only when it parses as JSON with a top-level string `level` or `severity` ranked below the floor; plain text, malformed JSON, and unknown severities MUST always print. The key names MUST match exactly, in lower case.

#### Scenario: Floor at warn
- **WHEN** `--log-level warn` is set and kubectl emits `{"level":"info"}`, `{"level":"error"}`, and `starting server`
- **THEN** only the error line and the plain text line are printed

#### Scenario: Key in another case
- **WHEN** `--log-level warn` is set and kubectl emits `{"Level":"debug"}` and `{"level":"error","Level":"debug"}`
- **THEN** both lines are printed

### Requirement: Untrusted output is rendered inert
Pod output and kubectl diagnostics MUST be rendered with control and invisible formatting characters shown as visible escapes, while ordinary text stays byte-identical. Output SHALL be transformed line by line with a fixed memory bound, streaming oversized lines in chunks.

#### Scenario: Escape sequence in a log line
- **WHEN** a pod writes a line containing an ANSI escape sequence
- **THEN** the terminal receives a visible escape rather than the raw control byte

### Requirement: Color policy
`--color` SHALL accept `auto`, `always`, or `never`. `auto` MUST colorize recognized severities only when stdout is a terminal and `NO_COLOR` is not present in the environment.

#### Scenario: NO_COLOR present
- **WHEN** `NO_COLOR` is set and `--color` is `auto` on a terminal
- **THEN** no color is applied

### Requirement: kubectl exit status propagates
Every `k8s` subcommand MUST exit with kubectl's own nonzero exit code when kubectl fails, and cancellation SHALL propagate to the kubectl process.

#### Scenario: kubectl fails
- **WHEN** kubectl exits 1 because the pod is not found
- **THEN** `forgectl k8s logs` exits 1

### Requirement: ns reads or switches the namespace
`forgectl k8s ns` SHALL print the current context's namespace, or `default` when unset. With `--json` it SHALL emit `{"namespace":"..."}`. `forgectl k8s ns NAME` SHALL switch the current context's namespace. `--json` combined with a namespace argument, or an empty namespace, MUST be refused.

#### Scenario: Unset namespace
- **WHEN** the current context sets no namespace and the operator runs `forgectl k8s ns --json`
- **THEN** stdout is `{"namespace":"default"}`

#### Scenario: JSON with an argument
- **WHEN** the operator runs `forgectl k8s ns staging --json`
- **THEN** the command refuses and the context is unchanged

### Requirement: exec hands kubectl the terminal
`forgectl k8s exec` MUST forward every argument to `kubectl exec` unchanged, parse no flags of its own, and give kubectl the real stdin, stdout, and stderr. It MUST refuse a call with no arguments.

#### Scenario: Interactive shell
- **WHEN** the operator runs `forgectl k8s exec -it pod/api -- sh`
- **THEN** kubectl runs `exec -it pod/api -- sh` attached to the operator's terminal

### Requirement: inspect validates the workload reference
`forgectl k8s inspect` MUST require a first argument of the form `kind/name`, where both halves match kubectl's lowercase name charset, and MUST refuse a reference starting with `-` before anything reaches kubectl.

#### Scenario: Flag injection attempt
- **WHEN** the operator runs `forgectl k8s inspect --server=https://evil.example/x`
- **THEN** the command refuses and kubectl is not invoked

### Requirement: inspect runs a fixed describe, get, events sequence
`forgectl k8s inspect kind/name [args]` SHALL run `kubectl describe`, `kubectl get -o wide`, and `kubectl get events` filtered to the workload's name, in that order, forwarding remaining arguments to all three. Each section MUST print under a `== <title> ==` banner with terminal-safe lines, and the sequence MUST stop at the first failure.

#### Scenario: Describe fails
- **WHEN** `kubectl describe` fails for the workload
- **THEN** no get or events section is printed and kubectl's exit code is returned
