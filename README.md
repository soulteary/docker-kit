# docker-kit

[![CI](https://github.com/soulteary/docker-kit/actions/workflows/ci.yml/badge.svg)](https://github.com/soulteary/docker-kit/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/docker-kit.svg)](https://pkg.go.dev/github.com/soulteary/docker-kit)

Drive the docker CLI from Go: run commands, read what a container was actually created with, and report where that has drifted from what the configuration now says. Zero dependencies.

**Docs:** English · [中文](README_CN.md)

## The problem it exists for

A container's image, network, mounts, environment and group membership are fixed at `docker create`. Nothing afterwards changes them — `docker start` simply starts the container that already exists.

So when a configuration is edited, the edit **appears** to take effect. The config file says one thing, the UI says it was saved, and the container keeps running with the old value. Nothing anywhere reports the mismatch.

The failure that motivated this: a container still mounting the host's docker socket, long after the setting was changed to stop it. The isolation the edit was meant to achieve never happened, and everything looked fine.

```go
facts, _ := dockerkit.Inspect(ctx, "app")
if diffs := spec.Drift(facts, imageID); len(diffs) > 0 {
    log.Printf("container %s was created with different settings: %s",
        spec.Name, dockerkit.DiffsString(diffs))
    // → container_image: app:v1 → app:v2; mount /data: /srv/old → /srv/new
}
```

## Install

```bash
go get github.com/soulteary/docker-kit
```

## One spec, both directions

`Spec` is the single definition behind creating a container *and* comparing one:

```go
spec := dockerkit.Spec{
    Name:    "app",
    Image:   "app:v2",
    Network: "app-net",
    Binds:   []string{"/srv/data:/data"},
    Env:     map[string]string{"MODE": "prod"},
    EnvSet:  map[string]string{"TOKEN": secret},
    Limits:  dockerkit.Limits{CPUs: "2", Memory: "4g"},
}

args, err := spec.CreateArgs()          // docker create …
diffs := spec.Drift(facts, imageID)     // what no longer matches
```

Keeping those in one place is the point. Written separately, a change to the create path leaves the comparison checking the previous shape — and the drift it exists to catch goes unreported.

`CreateArgs` is deterministic: maps are emitted in sorted key order. Callers log these, diff them, and paste them into a shell; flags that reshuffle between runs are useless for all three.

## `Env` vs `EnvSet`

`Env` is compared by value. `EnvSet` is compared **only for presence**, and the distinction is why it exists.

A secret injected at creation — a token the container authenticates with — shouldn't be compared by value: rotating it is a runtime concern the container surfaces by failing to authenticate, and treating a mismatch as drift would delete a working container to inject a value it will be handed anyway.

But **absence is different in kind**. A container created before the secret was introduced has no value at all, and code reading an empty value usually falls back to "no authentication" — silently, with nothing failing to reveal it. That one needs a rebuild. A value that is set but empty counts as absent, because that's how the consumer will read it, and an image can ship `ENV TOKEN=` of its own.

## Why the network is written as a label

"Is it attached to the right network" is the wrong question.

`docker create --network` sets exactly one network, but a container can be attached to more later with `docker network connect`. When configuration changes from `net-a` to `net-b` and the container happens to be on both, an any-match test passes — leaving it attached to `net-a`, which is precisely the attachment the edit was meant to remove.

So `CreateArgs` records the network as a label, and `Drift` reads the label. `NetworkMode` is the fallback for containers created before the label existed; the rebuild that follows writes the label, so that path is taken at most once.

## Output classification

Docker uses the same non-zero exit status for "it isn't there" and "I couldn't reach the daemon", and those call for opposite reactions — the first is usually fine, the second never is.

| Function | Meaning |
|---|---|
| `NotFound(out)` | No such container (several locales) |
| `PermissionDenied(out)` | Can't reach the daemon: socket permissions, or nothing listening |
| `UnrecoverableStart(out)` | `docker start` failed for something retrying will never fix — usually a network removed by `compose down`. Delete and recreate; callers that retry will retry forever |
| `Error(op, out, err)` | Wraps a failure **with its output**, plus the access hint when relevant |

`Error` carries the output because an error that says only `exit status 1` costs the reader a trip to the host to find out what docker actually said.

## Resource limits

```go
l := dockerkit.Limits{CPUs: "2", Memory: "4g", MemorySwap: "8g", PidsLimit: 512}
l.Validate()          // catches what docker would only reject at create time
l.Args()              // --cpus 2 --memory 4g …
l.UpdateArgs("app")   // docker update … app
```

`Validate` is worth running while the configuration is being read: docker's own rejection arrives at `docker create` time, by which point the message has lost all trace of which setting caused it. It catches `1e3` and `NaN` (accepted by `ParseFloat`, rejected by docker), and the common misreading of `memory-swap` — that value is the **total** of memory plus swap, so it can never be the smaller of the two.

`UpdateArgs` exists because `Args` only takes effect at creation. A deployment that adds limits after its containers exist gets no benefit; stopping and starting doesn't help either. `docker update` applies them to what's already there, without a rebuild.

## Serializing lifecycle operations

```go
var locks dockerkit.Locks

func start(name string) {
    defer locks.Lock(name)()
    // …
}
```

A supervisor usually has several paths that can start the same container — a boot-time sweep, a periodic reconciler, an API call. Once "recreate" means `docker rm` followed by `docker create`, two of them crossing can delete the container the other just created. A name collision is a logged annoyance; this is a container that silently isn't there.

## Caching image ids

```go
cache := &dockerkit.ImageIDCache{}   // 10s default
id := cache.Get(ctx, "app:v2")
```

A status page listing N containers usually finds them all on the same image; without a cache each refresh spawns N `docker image inspect` processes.

**Do not use it on a lifecycle path.** "Rebuild the image, then press start" is an ordinary thing to do, and a cached id there means the rebuild is silently ignored — exactly the bug the drift check exists to catch. Use `ImageID` directly when starting.

## Testing

`Runner.Exec` replaces process execution, so tests need no daemon:

```go
r := dockerkit.Runner{Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
    return []byte(`[{"State":{"Running":true}}]`), nil
}}
facts, err := r.Inspect(ctx, "app")
```

`Runner.Binary` also points the package at a compatible CLI such as `podman`.

## License

Apache 2.0 — see [LICENSE](LICENSE).
