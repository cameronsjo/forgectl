# forgectl as a container, for one purpose: `forgectl tasks mcp --http`.
#
# Nothing else in forgectl is meant to run here — the binary is a macOS
# workbench control plane, and most of its verbs drive a local terminal, a
# keychain, or a Swift helper that is not in this image. The MCP server is the
# one subcommand with no macOS dependency.
#
# Distroless static, and specifically the :nonroot tag: no shell, no package
# manager, no libc, and uid 65532. A prompt-injectable agent reaching this
# container through the gateway finds one process and no interpreter to reach
# for. The binary holds a bearer token, so that matters more here than the
# image size does.

# --platform=$BUILDPLATFORM pins the BUILDER to the machine doing the building,
# and the Go build below cross-compiles to $TARGETARCH instead. Without it,
# building a linux/amd64 image on an arm64 Mac runs the amd64 Go toolchain
# under emulation — where it does not merely run slowly, it crashes:
# `fatal error: found pointer to free object` out of the runtime, mid-`go mod
# download`, which reads as a corrupt module cache rather than an emulation
# fault. Go cross-compiles natively, so there is nothing to emulate.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

ARG TARGETARCH

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 is what makes the binary runnable on distroless *static*: with
# cgo the resulting binary needs a dynamic loader and glibc, neither of which
# exists in the runtime layer, and the failure is an exec format / "no such
# file or directory" error naming a file that is plainly present.
#
# -trimpath keeps build-host paths out of the binary; the ldflags strip the
# symbol table and DWARF, which the server does not need.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
	go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/forgectl .

FROM gcr.io/distroless/static-debian12:nonroot

# The entrypoint is the bare binary, so `command:` in a compose file supplies
# only the subcommand and its flags:
#
#   command: ["tasks","mcp","--http",":3000","--token-file","/run/secrets/vikunja-token", ...]
#
# There is no shell form available and that is deliberate — an entrypoint that
# goes through `sh -c` is one string-interpolation away from putting a token on
# a command line.
COPY --from=build /out/forgectl /forgectl

USER nonroot:nonroot
EXPOSE 3000
ENTRYPOINT ["/forgectl"]
