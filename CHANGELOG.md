# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Nothing is tagged yet, so `go get` resolves to a pseudo-version of `main` and
the API may still change.

## [Unreleased]

### Added

- `CHANGELOG.md` and `SECURITY.md`. The security policy covers what follows
  from shelling out to `docker`: daemon access is root on the host, a `Spec`
  is only as trusted as the configuration it was built from, `Spec.Extra` is
  unchecked passthrough that `Drift` cannot see, and diffs and errors carry
  values verbatim — which is what `Spec.EnvSet` is for.
- Runnable examples (`ExampleSpec_CreateArgs`, `ExampleSpec_Drift` and its
  variants, `ExampleRunner_Inspect`, `ExampleLimits_Validate`,
  `ExampleNotFound`) that `go test` verifies, so they cannot drift from the
  API. They are an external test package (`package dockerkit_test`):
  compiling only against the exported API keeps that API honest about being
  sufficient from outside, and `Runner.Exec` means none of them needs a daemon.
- A Go Report Card workflow, run on demand, that regenerates
  `.github/goreportcard.svg` and `.github/goreportcard-report.md` and commits
  them back.
- A `.gitignore` covering build output, coverage artifacts and editor files.

### Fixed

- `(Limits).Validate` was over gocyclo's complexity threshold (19 vs 15). The
  per-field checks are now one function each — `validateCPUs`, `validateSizes`,
  `validatePidsLimit` — and the one rule that spans two fields, that
  `memory_swap` is the total of memory plus swap and so can never be smaller
  than `memory`, is `validateSwapTotal`. No behaviour change: the same values
  are rejected, in the same order, with the same messages.

### Changed

- The package doc moved from `cli.go` to `doc.go`, and gained the problem
  statement (settings are fixed at `docker create`, so an edit appears to take
  effect while the container keeps running with the old value) and a layout
  section covering `Spec`, `Facts`, `Runner`, `Limits`, `Locks` and
  `ImageIDCache`.
- CI pins `actions/checkout`, `actions/setup-go` and `actions/upload-artifact`
  to v7, and uploads the HTML coverage report as a build artifact. There is no
  coverage service: the profile is produced and summarised inside the job, and
  the browsable report is downloadable from the run.

## Before the first release

dockerkit was extracted from a GitHub Actions runner supervisor. The failure
that motivated `Spec.Drift` was a container still mounting the host's docker
socket long after the setting was changed to stop it: the isolation the edit
was meant to achieve never happened, and everything looked fine.
