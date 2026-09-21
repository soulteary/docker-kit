package dockerkit_test

import (
	"context"
	"fmt"
	"log"
	"strings"

	dockerkit "github.com/soulteary/docker-kit"
)

// A spec is the single definition of what a container should look like, and it
// faces both ways: CreateArgs builds the container from it, Drift compares an
// existing container against it.
var spec = dockerkit.Spec{
	Name:     "app",
	Image:    "app:v2",
	Network:  "app-net",
	Binds:    []string{"/srv/app/data:/data"},
	Env:      map[string]string{"LOG_LEVEL": "info"},
	Labels:   map[string]string{"owner": "platform"},
	GroupAdd: []string{"998"},
	Limits:   dockerkit.Limits{CPUs: "2", Memory: "512m"},
}

// CreateArgs is deterministic: maps are emitted in sorted key order, so the
// same spec always produces the same command. Callers log these, diff them and
// paste them into a shell, and a command whose flags reshuffle between runs is
// useless for all three.
//
// Note the second --label: the network is recorded at creation, because "which
// network was this created with" and "which networks is it attached to now"
// are different questions. See Spec.Drift.
func ExampleSpec_CreateArgs() {
	args, err := spec.CreateArgs()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("docker", strings.Join(args, " "))

	// Output:
	// docker create --name app -v /srv/app/data:/data --network app-net --label io.dockerkit.network=app-net --label owner=platform -e LOG_LEVEL=info --group-add 998 --cpus 2 --memory 512m app:v2
}

// Drift is the check that makes a configuration edit real. Image, network,
// mounts, environment and group membership are fixed at `docker create`;
// `docker start` just starts the container that already exists, so without a
// comparison an edit appears to take effect while the container keeps running
// with the old value.
//
// Here the running container predates two edits: the data directory moved, and
// the image was bumped to v2.
func ExampleSpec_Drift() {
	running := &dockerkit.Facts{
		Running:     true,
		ImageRef:    "app:v1",
		Binds:       []string{"/mnt/old-data:/data"},
		Env:         []string{"LOG_LEVEL=info"},
		Labels:      map[string]string{"owner": "platform", "io.dockerkit.network": "app-net"},
		GroupAdd:    []string{"998"},
		NetworkMode: "app-net",
	}

	diffs := spec.Drift(running, "")
	fmt.Println(dockerkit.DiffsString(diffs))

	// Output:
	// image: app:v1 → app:v2; mount /data: /mnt/old-data → /srv/app/data
}

// Only what the spec asks for is compared. Environment variables, labels and
// mounts the container carries but the spec never mentions are left alone,
// because images legitimately set their own and a container is not wrong for
// having them.
func ExampleSpec_Drift_onlyWhatIsAsked() {
	minimal := dockerkit.Spec{Name: "app", Image: "app:v2"}

	running := &dockerkit.Facts{
		ImageRef: "app:v2",
		Env:      []string{"PATH=/usr/bin", "TZ=UTC", "LOG_LEVEL=debug"},
		Labels:   map[string]string{"org.opencontainers.image.version": "2.0"},
	}

	fmt.Println(len(minimal.Drift(running, "")))

	// Output:
	// 0
}

// EnvSet is compared for presence only, and that is the whole reason it is a
// separate field. A rotated token should not delete a working container --
// the container surfaces a stale one by failing to authenticate. Its ABSENCE
// is different in kind: a container created before the secret existed reads an
// empty value and usually falls back to "no authentication", silently.
//
// A value that is set but empty counts as absent, because that is how the
// consumer will read it.
func ExampleSpec_Drift_envSet() {
	withSecret := dockerkit.Spec{
		Name:   "app",
		Image:  "app:v2",
		EnvSet: map[string]string{"API_TOKEN": "rotated-today"},
	}

	stale := &dockerkit.Facts{ImageRef: "app:v2", Env: []string{"API_TOKEN=issued-last-year"}}
	fmt.Println("different value:", len(withSecret.Drift(stale, "")))

	never := &dockerkit.Facts{ImageRef: "app:v2", Env: []string{"API_TOKEN="}}
	fmt.Println("never set:      ", dockerkit.DiffsString(withSecret.Drift(never, "")))

	// Output:
	// different value: 0
	// never set:       env API_TOKEN: (none) → set
}

// A tag rebuilt in place keeps its reference and changes its contents. It is
// reported separately, because "image: app:v2 → app:v2" reads like a bug in
// the diff rather than a reason to recreate the container.
func ExampleSpec_Drift_rebuiltTag() {
	running := &dockerkit.Facts{ImageRef: "app:v2", ImageID: "sha256:0000old"}
	current := dockerkit.Spec{Name: "app", Image: "app:v2"}

	fmt.Println(dockerkit.DiffsString(current.Drift(running, "sha256:1111new")))

	// Output:
	// image: rebuilt → app:v2
}

// Runner.Exec replaces command execution entirely, which is how a caller tests
// against docker without a daemon. The zero Runner runs the real `docker` on
// PATH.
//
// Absence is not an error: a container that does not exist comes back as
// (nil, nil), because callers branch on "does it exist" rather than parse an
// error string to find out.
func ExampleRunner_Inspect() {
	docker := dockerkit.Runner{
		Exec: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[len(args)-1] == "ghost" {
				return []byte("Error: No such container: ghost"), fmt.Errorf("exit status 1")
			}
			return []byte(`[{"Image":"sha256:abc","State":{"Status":"running","Running":true},
			                 "Config":{"Image":"app:v2","Env":["LOG_LEVEL=info"]},
			                 "HostConfig":{"Binds":["/srv/app/data:/data"],"NetworkMode":"app-net"}}]`), nil
		},
	}

	facts, err := docker.Inspect(context.Background(), "app")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(facts.Status, facts.ImageRef, facts.NetworkMode)

	source, ok := facts.BindSource("/data")
	fmt.Println(source, ok)

	missing, err := docker.Inspect(context.Background(), "ghost")
	fmt.Println(missing == nil, err == nil)

	// Output:
	// running app:v2 app-net
	// /srv/app/data true
	// true true
}

// Validate rejects what docker would reject, while the configuration is being
// read rather than at `docker create` time -- by which point the message has
// lost all trace of which setting caused it.
//
// --memory-swap is the TOTAL of memory plus swap, not the swap on its own.
// Reading it as "how much swap" and setting it below memory is the usual
// mistake, and docker only says so at create time.
func ExampleLimits_Validate() {
	fmt.Println(dockerkit.Limits{CPUs: "1.5", Memory: "512m", MemorySwap: "1g"}.Validate())
	fmt.Println(dockerkit.Limits{Memory: "4g", MemorySwap: "2g"}.Validate())
	fmt.Println(dockerkit.Limits{CPUs: "all the cpus"}.Validate())

	// Output:
	// <nil>
	// memory_swap (2g) cannot be smaller than memory (4g): it is the total of memory plus swap, and docker refuses to create the container
	// cpus must be a positive number such as "2" or "1.5", got "all the cpus"
}

// NotFound matches on docker's message rather than its exit status, because
// docker uses the same non-zero status for "it isn't there" and "I couldn't
// reach the daemon" -- and those call for opposite reactions: the first is
// usually fine, the second never is.
func ExampleNotFound() {
	fmt.Println(dockerkit.NotFound([]byte("Error: No such container: app")))
	fmt.Println(dockerkit.NotFound([]byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock")))
	fmt.Println(dockerkit.PermissionDenied([]byte("permission denied while trying to connect to the Docker daemon socket")))

	// Output:
	// true
	// false
	// true
}
