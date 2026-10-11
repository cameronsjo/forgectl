# Contributing to forgectl

Agent-facing working notes (build, lint, the docs reader, review) live in
[AGENTS.md](AGENTS.md), and release mechanics in
[docs/RELEASING.md](docs/RELEASING.md). This file collects the gotchas a
human contributor hits outside those.

## Building the container image

The [Dockerfile](Dockerfile) builds `forgectl tasks mcp --http` on
`golang:1.26-bookworm`. The official golang images set
`GOTOOLCHAIN=local`, so the Go inside the image never downloads a newer
toolchain. `go.mod` requires a minimum patch release (`go 1.26.8` at the time
of writing). A locally cached `golang:1.26-bookworm` pulled before that patch
shipped therefore fails `docker build` at `go mod download`:

```text
go: go.mod requires go >= 1.26.8 (running go 1.26.x; GOTOOLCHAIN=local)
```

The failure is a stale image, not a broken tree. Refresh the tag and rebuild:

```sh
docker pull golang:1.26-bookworm
docker build .
```

`docker build --pull .` does the same in one step. The release workflow's
image build runs on a fresh runner with no cached base image, and CI's
setup-go steps set `check-latest: true` for the same reason, so only a local
build hits this.

## Local development loop

These are the commands [CI](.github/workflows/ci.yml) runs, in its order. Run
them from the repo root before you push. If one fails here, it fails in CI.

```sh
go build ./...
go vet ./...
GOOS=windows go vet ./...
GOOS=freebsd go vet ./...
FORGECTL_REQUIRE_TMUX=1 FORGECTL_REQUIRE_SOPS_INTEGRATION=1 FORGECTL_REQUIRE_RG=1 go test ./...
gofmt -l .   # must print nothing
```

- **Vet for other platforms.** Windows and FreeBSD are not release targets,
  but CI vets for both. Vet type-checks test files, which `go build` skips. A
  unix-only syscall or a platform-specific fixture in an untagged test file
  fails only these two steps.
- **The test tools.** Some tests drive real binaries: `tmux`, `rg`
  (ripgrep), and `sops` plus `age`/`age-keygen`. CI pins sops v3.13.3 and age
  v1.2.1. Without a tool, its tests SKIP and the package still reports `ok`.
  Each `FORGECTL_REQUIRE_*` variable makes a missing tool a failure instead,
  which is how CI runs. Drop a variable only if you do not have that tool and
  accept that its tests did not run.
- **Speed.** The full `go test ./...` takes a few minutes. While iterating,
  test only the package you are changing (`go test ./internal/<pkg>`), and run
  the whole suite before you push.
- **macOS.** CI also runs `go test -v ./...` on macOS with the same
  `FORGECTL_REQUIRE_*` variables, because some tests run only on Darwin.

### Lint

CI pins golangci-lint **v2.13.1**. To run that exact version without
installing it:

```sh
git fetch origin main   # .golangci.yml compares against origin/main
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1 run
```

`go run` builds the linter with your local Go toolchain. A prebuilt
golangci-lint built with an older Go fails on this module with a message like
"go1.25 … lower than targeted 1.26.8". Building it this way avoids that. The
first run takes about 30 seconds to build, and later runs use the cache.
`.golangci.yml` sets `new-from-rev: origin/main`, so the linter reports only
new findings. A stale or missing `origin/main` gives a wrong result, so fetch
it first.

### Swift helper

If you change `helper/forgectl-bless-helper`, CI also runs `swift build` and
`swift test` in that directory on macOS:

```sh
cd helper/forgectl-bless-helper
swift build
swift test
swift package reset   # remove .build before running the Go tests again
```

Run `swift package reset` when you finish. `swift build` leaves symlinked
directories under `.build/`, and `TestModuleTreeHidesNoGoSource` in
`internal/exec` fails on them. CI never hits this because the Swift job runs
on a separate runner.
