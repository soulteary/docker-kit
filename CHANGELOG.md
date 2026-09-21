# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] — unreleased

First tagged release. From here the exported API is a compatibility promise:
it will not change incompatibly before 2.0.0.

dockerkit was extracted from a GitHub Actions runner supervisor. The failure
that motivated `Spec.Drift` was a container still mounting the host's docker
socket long after the setting was changed to stop it: the isolation the edit
was meant to achieve never happened, and everything looked fine.

**Requires Go 1.22 or newer, the `docker` CLI on PATH at run time, and Unix.**
A library's `go` directive is a hard floor for everyone who imports it, so it
is kept as low as the code allows rather than tracking the newest toolchain.
`SocketGID` reads POSIX ownership through `syscall.Stat_t`, so the package does
not build on Windows.

### What 1.0.0 provides

- `Spec`, the single definition of what a container should look like, facing
  both ways: `Spec.CreateArgs` builds the container from it and `Spec.Drift`
  compares an existing container against it. Written separately, a change to
  the create path leaves the comparison checking the previous shape, and the
  drift it exists to catch goes unreported.
- `Spec.EnvSet` alongside `Spec.Env`: values compared for presence only, so
  rotating a secret does not delete a working container while its absence still
  forces a rebuild.
- The network recorded as a label at creation, because "which network was this
  created with" and "which networks is it attached to now" are different
  questions.
- `Facts` and `ParseInspect`: `docker inspect` reduced to the fields that say
  how a container was created, plus `Facts.EnvValue` and `Facts.BindSource`.
- `Runner`, with `Runner.Binary` for a compatible CLI such as podman and
  `Runner.Exec` to replace execution entirely; `Default` and the package-level
  `Run`, `Inspect` and `ImageID` that delegate to it.
- Output classification — `NotFound`, `PermissionDenied`, `UnrecoverableStart`
  — and `Error`, which carries docker's output rather than just "exit status 1".
- `Limits` with `Args`, `UpdateArgs` and `Validate`, which rejects what docker
  would only reject at `docker create` time; `ParseSize`.
- `Locks` to serialize lifecycle operations per container name, and
  `ImageIDCache` to keep `docker image inspect` off a status page's hot path.

### Fixed before the release

- `(Limits).Validate` was over gocyclo's complexity threshold (19 vs 15). The
  per-field checks are now one function each — `validateCPUs`, `validateSizes`,
  `validatePidsLimit` — and the one rule that spans two fields, that
  `memory_swap` is the total of memory plus swap and so can never be smaller
  than `memory`, is `validateSwapTotal`. No behaviour change: the same values
  are rejected, in the same order, with the same messages.
- `ImageIDCache.Runner` was documented as using `Default` when left zero. It
  does not — it uses the zero `Runner`, which runs `docker` on PATH. The two
  behave identically until a caller replaces `Default`, at which point the
  cache would not follow. The doc comment now says what the code does.

### Also in the repository

- `SECURITY.md`, covering what follows from shelling out to `docker`: daemon
  access is root on the host; a `Spec` becomes a command line, so it is only as
  trusted as the configuration it was built from, and `Spec.Extra` is unchecked
  passthrough that `Drift` cannot see; and diffs and errors carry values
  verbatim, which is what `EnvSet` is for.
- Runnable examples in `example_test.go` that `go test` verifies, as an
  external test package going through `Runner.Exec`, so none of them needs a
  daemon and they cannot drift from the exported API.
- CI covering formatting, vet, tests, golangci-lint and govulncheck, with the
  test job run against Go 1.22 and the current release on Linux and macOS. The
  HTML coverage report is uploaded as a build artifact; no coverage service is
  involved.
- A Go Report Card workflow, run on demand, that regenerates the badge and
  report and commits them back.

[1.0.0]: https://github.com/soulteary/docker-kit/releases/tag/v1.0.0
