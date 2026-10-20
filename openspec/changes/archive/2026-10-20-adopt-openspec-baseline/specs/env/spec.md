## Purpose
`forgectl env` manages `.env` files and single keys in SOPS-encrypted YAML so that key names are always visible and secret values never appear in argv, terminal output, logs, or a session transcript.

## ADDED Requirements

### Requirement: Values never on argv or in output
No `env` verb SHALL accept a secret value as a command-line argument or print a value to stdout, stderr, or logs. `env set` MUST take its value from piped stdin, a no-echo prompt, or `--clipboard`, and `env get` MUST deliver a value only to the clipboard. Clipboard handling MUST NOT log a value's length.

#### Scenario: Get without clipboard
- **WHEN** the operator runs `forgectl env get API_KEY`
- **THEN** the command fails stating that `--clipboard` is required and that no print path exists

#### Scenario: Set from a pipe
- **WHEN** a value is piped into `forgectl env set API_KEY`
- **THEN** stdout names the key and the file and contains no part of the value

### Requirement: Key shape checked before input is read
`env set` MUST validate the key before reading stdin or the clipboard: a `.env` key MUST match `[A-Za-z_][A-Za-z0-9_]*` and a `--sops` key MUST be a dotted path. A rejected key MUST leave the input unconsumed.

#### Scenario: Value smuggled into the key
- **WHEN** the operator runs `forgectl env set API_KEY=secret`
- **THEN** the command refuses before reading any input and says values never come from argv

### Requirement: Repository containment
Every `env` verb MUST run inside a git repository and exit 1 outside one. `--file` MUST resolve inside the current repository, with symlink escapes refused, and the resolved directory SHALL be pinned so later reads and writes cannot be redirected by a path swap.

#### Scenario: Outside a repository
- **WHEN** `forgectl env keys` runs outside any git work tree
- **THEN** it exits 1 and tells the operator to `cd` into the repository

#### Scenario: Symlink escaping the repo
- **WHEN** `--file` names a symlink that resolves outside the repository
- **THEN** the command refuses

### Requirement: Env-file name allowlist
`--file` MUST name `.env`, `.env.*`, or `*.env`. `--any-file` SHALL override this only after an interactive confirmation that names the resolved path, and MUST refuse outright when no terminal is available.

#### Scenario: Git config target
- **WHEN** the operator runs `forgectl env set KEY --file .git/config`
- **THEN** the command refuses the file name

#### Scenario: any-file with no terminal
- **WHEN** `--any-file` is passed in a piped invocation
- **THEN** the command refuses because an interactive terminal is required

### Requirement: Safe writes
Every `.env` write MUST be atomic, via a fresh inode renamed onto the target, and MUST leave the file at mode `0600`. A looser existing mode SHALL be tightened and reported on stderr. A new value containing a carriage return MUST be refused.

#### Scenario: Loose permissions
- **WHEN** `.env` is mode `0644` and a key is set
- **THEN** the file ends at `0600` and stderr reports `tightened ".env" to 0600`

#### Scenario: Carriage return in a value
- **WHEN** the piped value contains a `\r` inside it
- **THEN** the write is refused

### Requirement: Read-only verbs print names or masks only
`env keys` MUST print key names only, with `--json` emitting `{"keys":[...],"skipped_malformed":N}`. `env redact` MUST print the file with every value masked to a fixed `****` and every comment masked to `# ****`, never revealing a value's length.

#### Scenario: Redact a commented-out secret
- **WHEN** `.env` contains `# old token: abc123`
- **THEN** `env redact` prints that line as `# ****`

### Requirement: check exit codes and JSON contract
`env check` SHALL compare the env file against `--example` (default `.env.example`) by key name: exit 0 on agreement, exit 1 on drift, exit 2 when either file is absent. Under `--json`, drift MUST be one `{"missing":[...],"extra":[...]}` object on stdout, and any other failure MUST be one `{"error","code","path"}` object on stderr with stdout empty.

#### Scenario: Drift
- **WHEN** `.env.example` lists a key `.env` lacks
- **THEN** `env check --json` prints that key under `missing` and exits 1

#### Scenario: Missing example
- **WHEN** the example file does not exist
- **THEN** `env check --json` writes `{"error":"env file not found","code":"file_not_found",...}` to stderr and exits 2

### Requirement: SOPS target rules
`env set --sops` MUST only write to a file whose name matches `*.sops.yaml`, `*.sops.yml`, `*.enc.yaml`, `*.enc.yml`, `secrets.yaml`, `secrets.yml`, or `secrets.*.yaml`/`.yml`, whose content has a top-level `sops:` mapping, and which already exists. The default target SHALL be `secrets.sops.yaml` at the repository root. `--any-file` with `--sops` MUST be refused.

#### Scenario: any-file with sops
- **WHEN** the operator passes both `--sops` and `--any-file`
- **THEN** the command refuses before reading the value

### Requirement: SOPS value is never in argv or environment
The SOPS write SHALL run sops with forgectl's own executable as the editor and pass the value as a file path, so the value MUST NOT enter any process argv or environment. sops MUST be resolved to an absolute path once per write.

#### Scenario: Process listing during a write
- **WHEN** another process lists argv and environment during `env set a.b --sops`
- **THEN** the value does not appear in either

### Requirement: SOPS write is verified encrypted
After a SOPS write, forgectl MUST decrypt the key and compare it byte-exactly with the input, and MUST confirm the stored scalar at that path carries an `ENC[AES256_GCM,` marker. Untouched values SHALL keep their exact ciphertext.

#### Scenario: Cleartext rule
- **WHEN** the path falls under the file's unencrypted rules
- **THEN** the write is refused rather than stored in cleartext

### Requirement: SOPS refusals for unsafe shapes
The SOPS route MUST refuse a missing block at any depth, a path naming a mapping, a dotted key name, the top-level `sops` block, a value with a newline, a C0 control byte other than tab, or invalid UTF-8, a file over 4 MiB, a top-level or `sops:` merge key, and document shapes it cannot bound.

#### Scenario: Missing parent block
- **WHEN** the path `db.password` names a `db` block that does not exist
- **THEN** the write is refused and the file is unchanged

### Requirement: Interrupted writes block the next write
Scratch files and directories forgectl creates beside a target MUST carry a hash of the target name and a `.gitignore` of `*`. The next `env set` on that target MUST refuse while any such leftover exists, or while a git stash holds one, naming each leftover and deleting nothing.

#### Scenario: Leftover scratch directory
- **WHEN** a `.forgectl-sops-<hash>-<random>/` directory remains beside the target
- **THEN** the next `env set --sops` on that target refuses and names the directory
