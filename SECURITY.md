# Security Policy

## Reporting a vulnerability

**Please do not open a public issue for a security problem.**

Report it through GitHub's private vulnerability reporting: go to the
[Security tab](https://github.com/soulteary/docker-kit/security) and choose
**Report a vulnerability**. That opens a private advisory visible only to the
maintainers.

If you do not see that option, open a normal issue saying only that you have a
security report and need a private channel — **no details, no reproducer** —
and a maintainer will arrange one.

Please include, once you have a private channel:

- the affected version or commit,
- what an attacker can do, and what they need in order to do it,
- a reproducer, if you have one.

Expect an acknowledgement within a few days. This is a small
volunteer-maintained project, so please allow reasonable time for a fix before
disclosing publicly.

## Supported versions

| Version | Supported |
| ------- | --------- |
| `main`  | ✅ |

Nothing is tagged yet, so `go get` resolves to a pseudo-version of `main`, and
that is the only thing fixes land on. Once there is a `v1`, fixes will land on
the latest minor of the current major. There are no long-term support branches.

## What this library does, and what it does not

docker-kit runs `docker`. Everything below follows from that one fact.

### Access to the docker daemon is root on the host

A process that can run `docker` can start a container that mounts `/` and runs
as uid 0. There is no meaningful privilege boundary between "may talk to the
daemon" and "is root on this machine", and nothing in this package adds one.

That is the context for `AccessHint`, which suggests giving a container the
host's docker group. It is the remedy that works, and it is also a grant of
root on the host to whatever runs in that container. Say so where you use it.

### A Spec becomes a command line — so build it from configuration you control

`Spec.CreateArgs` renders the spec into the argv of `docker create`.

**No shell is involved.** Commands run through `exec.CommandContext` with an
argument list, so there is no word splitting and no `$(…)`, `;` or backtick
expansion. A bind or an environment value containing shell metacharacters is
passed to docker exactly as written.

That removes one class of problem and not the other. The modelled fields are
each passed as the value of their own flag (`-v <bind>`, `-e K=V`,
`--label K=V`), so a value cannot become a flag. **`Spec.Extra` is different**:
it is appended verbatim, as separate arguments, precisely so it can carry flags
this type does not model. `--privileged`, `--pid=host` and `-v /:/host` are all
things it will happily pass through, and `Spec.Name` and `Spec.Image` are
positions docker's own parser interprets.

So: a `Spec` is as trusted as the configuration it was built from. Do not build
one — least of all its `Extra`, `Binds` or `Image` — from user input, an API
request body, or anything else that crosses a trust boundary.

`Spec.Drift` does **not** compare `Extra`, because inspect gives no general way
to read those flags back. Anything put there is invisible to the drift check
and will not be noticed when it changes.

### Diffs and errors carry values verbatim

`Facts.Env` holds the container's environment as docker reports it, and
`Diff.String()` renders `env FOO: old → new` with both values in full. A log
line built from `DiffsString` on a spec whose `Env` holds a token publishes
that token to wherever the log goes.

`Error(op, out, err)` embeds docker's combined output for the same reason a
diff embeds values — an error saying only "exit status 1" costs the reader a
trip to the host — and that output can contain registry credentials, mount
paths and environment values.

**`Spec.EnvSet` is the tool for this.** Its values are compared for presence
only, never by value, so a secret placed there cannot appear in a `Diff`: the
only thing a mismatch can produce is `env API_TOKEN: (none) → set`. It exists
because rotating a secret should not delete a working container, and the
absence of one should still force a rebuild — a container created before the
secret existed reads an empty value and usually falls back to "no
authentication", silently.

### Output classification is text matching, and errs toward "unknown"

`NotFound`, `PermissionDenied` and `UnrecoverableStart` match on the text
docker printed, because docker returns the same non-zero exit status for "it
isn't there" and "I couldn't reach the daemon".

Text matching is inherently incomplete: a locale or a docker version these
patterns do not cover degrades to "some other error". That is the safe
direction — an unclassified failure is surfaced rather than swallowed — but do
not build an authorization decision on top of these predicates. They are for
choosing a reaction to a failure, not for deciding whether something is
allowed.

### Serialize lifecycle operations, or lose containers

`Locks` exists because lifecycle operations on one container are not safe to
interleave. Once "recreate" means `docker rm` followed by `docker create`, two
paths crossing can delete the container the other just created — a container
that silently is not there, rather than a logged collision. If your supervisor
has more than one path that can act on a container, take `Locks.Lock(name)`
around the whole sequence.
