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
