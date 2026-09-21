// Package dockerkit drives the docker CLI: running commands, reading what a
// container was actually created with, and reporting where that has drifted
// from what the configuration now says it should be.
//
// It shells out to `docker` rather than speaking to the daemon's API, and has
// no dependencies beyond the standard library. That is a deliberate trade: a
// tool that already requires the docker CLI on the host pays nothing extra,
// and gets error messages operators can reproduce by pasting the command.
//
// # The problem it exists for
//
// A container's image, network, mounts, environment and group membership are
// fixed at `docker create`. Nothing afterwards changes them -- `docker start`
// simply starts the container that already exists.
//
// So when a configuration is edited, the edit *appears* to take effect. The
// config file says one thing, the UI says it was saved, and the container
// keeps running with the old value. Nothing anywhere reports the mismatch.
// [Spec.Drift] is the comparison that makes the edit real.
//
// # Layout
//
// [Spec] is the single definition of what a container should look like, and it
// faces both ways: [Spec.CreateArgs] builds the container from it, and
// [Spec.Drift] compares an existing container against it. Keeping those in one
// place is the whole point -- written separately, a change to the create path
// leaves the comparison checking the previous shape, and the drift it exists
// to catch goes unreported.
//
// [Facts] is `docker inspect` reduced to the fields that say how a container
// was created; [ParseInspect] decodes them, [Runner.Inspect] fetches them.
//
// [Runner] executes docker commands. The zero value runs the `docker` binary
// on PATH, and package-level [Run], [Inspect] and [ImageID] use [Default];
// tests substitute [Runner.Exec]. [NotFound], [PermissionDenied] and
// [UnrecoverableStart] classify command output, and [WrapError] wraps a
// failure as a [CommandError], which keeps that output reachable so a caller
// holding the error can still ask the classifiers what went wrong.
//
// [Limits] are the resource caps, validated before `docker create` sees them
// rather than after. [Locks] serializes lifecycle operations per container,
// and [ImageIDCache] keeps `docker image inspect` off the hot path.
//
// # Getting started
//
//	spec := dockerkit.Spec{
//		Name:    "app",
//		Image:   "app:v2",
//		Network: "app-net",
//		Binds:   []string{"/srv/app/data:/data"},
//		Env:     map[string]string{"LOG_LEVEL": "info"},
//		Limits:  dockerkit.Limits{CPUs: "2", Memory: "512m"},
//	}
//
//	facts, err := dockerkit.Inspect(ctx, spec.Name)
//	switch {
//	case err != nil: // no such container yet -- create it
//		args, err := spec.CreateArgs()
//		...
//	default:
//		if diffs := spec.Drift(facts, dockerkit.ImageID(ctx, spec.Image)); len(diffs) > 0 {
//			log.Printf("%s must be recreated: %s", spec.Name, dockerkit.DiffsString(diffs))
//		}
//	}
//
// Only what the spec asks for is compared. Environment variables, labels and
// mounts the container has but the spec does not mention are left alone,
// because images legitimately set their own and a container is not wrong for
// carrying them. [Spec.EnvSet] is the other half of that rule: values the
// caller supplies but does not want compared -- a rotated token whose change
// is not a reason to recreate anything.
package dockerkit
