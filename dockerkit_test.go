package dockerkit

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- output classification ---

func TestNotFound(t *testing.T) {
	yes := []string{
		"Error: No such container: app",
		"Error response from daemon: No such object: app",
		"错误: 没有此容器: app",
		"未找到容器 app",
	}
	for _, s := range yes {
		if !NotFound([]byte(s)) {
			t.Errorf("NotFound(%q) = false", s)
		}
	}
	no := []string{
		"Cannot connect to the Docker daemon",
		"permission denied while trying to connect",
		"",
	}
	for _, s := range no {
		if NotFound([]byte(s)) {
			t.Errorf("NotFound(%q) = true", s)
		}
	}
}

// The daemon being unreachable and the container being absent share an exit
// status but call for opposite reactions, so they must not be conflated.
func TestPermissionDeniedIsNotNotFound(t *testing.T) {
	out := []byte("Got permission denied while trying to connect to the Docker daemon socket")
	if !PermissionDenied(out) {
		t.Fatal("PermissionDenied = false")
	}
	if NotFound(out) {
		t.Fatal("NotFound = true for a daemon-access failure")
	}
}

func TestUnrecoverableStart(t *testing.T) {
	yes := []string{
		"Error response from daemon: network app-net not found",
		"could not find network app-net",
		"failed to create endpoint app on network app-net",
		"error while creating mount source: failed to get network",
	}
	for _, s := range yes {
		if !UnrecoverableStart([]byte(s)) {
			t.Errorf("UnrecoverableStart(%q) = false", s)
		}
	}
	// A plain start failure is recoverable: retrying is reasonable.
	if UnrecoverableStart([]byte("Error response from daemon: container is marked for removal")) {
		t.Error("UnrecoverableStart = true for an ordinary start failure")
	}
}

// An error that says only "exit status 1" costs a trip to the host; the
// output is the diagnosis, so it has to be carried.
func TestErrorCarriesOutputAndHint(t *testing.T) {
	base := errors.New("exit status 1")

	err := Error("docker create", []byte("boom"), base)
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry the output: %v", err)
	}
	if !errors.Is(err, base) {
		t.Error("error should wrap the cause")
	}
	if strings.Contains(err.Error(), AccessHint) {
		t.Error("an ordinary failure should not carry the daemon-access hint")
	}

	err = Error("docker ps", []byte("permission denied"), base)
	if !strings.Contains(err.Error(), AccessHint) {
		t.Errorf("a daemon-access failure should carry the hint: %v", err)
	}

	// An empty body with a non-zero status is itself informative.
	err = Error("docker rm", nil, base)
	if !strings.Contains(err.Error(), "(no output)") {
		t.Errorf("empty output should be stated, got %v", err)
	}
}

// --- inspect ---

const inspectJSON = `[{
  "Image": "sha256:abc123",
  "State": {"Status": "running", "Running": true},
  "Config": {
    "Image": "app:v2",
    "Env": ["PATH=/usr/bin", "MODE=prod", "TOKEN=s3cr3t", "EMPTY="],
    "Labels": {"io.dockerkit.network": "app-net", "team": "infra"}
  },
  "HostConfig": {
    "Binds": ["/srv/data:/data", "/var/run/docker.sock:/var/run/docker.sock"],
    "GroupAdd": ["999"],
    "NetworkMode": "app-net"
  },
  "NetworkSettings": {"Networks": {"app-net": {}, "extra-net": {}}}
}]`

func facts(t *testing.T) *Facts {
	t.Helper()
	f, err := ParseInspect([]byte(inspectJSON))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseInspect(t *testing.T) {
	f := facts(t)
	if !f.Running || f.Status != "running" {
		t.Errorf("state = %v/%q", f.Running, f.Status)
	}
	if f.ImageRef != "app:v2" || f.ImageID != "sha256:abc123" {
		t.Errorf("image = %q / %q", f.ImageRef, f.ImageID)
	}
	if f.NetworkMode != "app-net" {
		t.Errorf("NetworkMode = %q", f.NetworkMode)
	}
	if f.Networks[0] != "app-net" {
		t.Errorf("NetworkMode should come first in Networks, got %v", f.Networks)
	}
	if len(f.Networks) != 2 {
		t.Errorf("Networks = %v, want both", f.Networks)
	}
}

func TestParseInspectEmptyArray(t *testing.T) {
	f, err := ParseInspect([]byte(`[]`))
	if err != nil || f != nil {
		t.Fatalf("ParseInspect([]) = %v, %v; want nil, nil", f, err)
	}
	if _, err := ParseInspect([]byte(`not json`)); err == nil {
		t.Fatal("ParseInspect should reject malformed output")
	}
}

// A variable that is set but empty is not the same as one that is unset.
func TestEnvValueDistinguishesEmptyFromUnset(t *testing.T) {
	f := facts(t)
	if v, ok := f.EnvValue("TOKEN"); !ok || v != "s3cr3t" {
		t.Errorf("TOKEN = %q, %v", v, ok)
	}
	if v, ok := f.EnvValue("EMPTY"); !ok || v != "" {
		t.Errorf("EMPTY = %q, %v; want \"\", true", v, ok)
	}
	if _, ok := f.EnvValue("ABSENT"); ok {
		t.Error("ABSENT reported as set")
	}
	var nilFacts *Facts
	if _, ok := nilFacts.EnvValue("X"); ok {
		t.Error("nil Facts should report nothing set")
	}
}

func TestBindSource(t *testing.T) {
	f := facts(t)
	if src, ok := f.BindSource("/data"); !ok || src != "/srv/data" {
		t.Errorf("BindSource(/data) = %q, %v", src, ok)
	}
	if _, ok := f.BindSource("/nope"); ok {
		t.Error("BindSource(/nope) found something")
	}
}

func TestInspectTreatsAbsenceAsNotAnError(t *testing.T) {
	r := Runner{Exec: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Error: No such container: gone"), errors.New("exit status 1")
	}}
	f, err := r.Inspect(context.Background(), "gone")
	if f != nil || err != nil {
		t.Fatalf("Inspect = %v, %v; want nil, nil", f, err)
	}

	r = Runner{Exec: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("permission denied"), errors.New("exit status 1")
	}}
	if _, err := r.Inspect(context.Background(), "app"); err == nil {
		t.Fatal("a daemon-access failure should surface as an error")
	}
}

// Resolving an image id must never become a reason to pull.
func TestImageIDIsEmptyWhenImageIsAbsent(t *testing.T) {
	r := Runner{Exec: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Error: No such image"), errors.New("exit status 1")
	}}
	if id := r.ImageID(context.Background(), "app:v2"); id != "" {
		t.Fatalf("ImageID = %q, want \"\"", id)
	}
}

func TestImageIDCacheServesAndExpires(t *testing.T) {
	var calls int
	run := Runner{Exec: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return []byte("sha256:abc\n"), nil
	}}
	c := &ImageIDCache{TTL: time.Hour, Runner: run}
	for range 5 {
		if id := c.Get(context.Background(), "app:v2"); id != "sha256:abc" {
			t.Fatalf("Get = %q", id)
		}
	}
	if calls != 1 {
		t.Fatalf("docker called %d times, want 1", calls)
	}

	expired := &ImageIDCache{TTL: time.Nanosecond, Runner: run}
	expired.Get(context.Background(), "app:v2")
	time.Sleep(time.Millisecond)
	expired.Get(context.Background(), "app:v2")
	if calls != 3 {
		t.Fatalf("docker called %d times after expiry, want 3", calls)
	}
}

// --- limits ---

func TestLimitsArgsAndUpdate(t *testing.T) {
	var zero Limits
	if args := zero.Args(); len(args) != 0 {
		t.Errorf("zero Limits should add no arguments, got %v", args)
	}
	if args := zero.UpdateArgs("app"); args != nil {
		t.Errorf("zero Limits should need no update, got %v", args)
	}

	l := Limits{CPUs: " 2 ", Memory: "4g", PidsLimit: 512}
	if got, want := strings.Join(l.Args(), " "), "--cpus 2 --memory 4g --pids-limit 512"; got != want {
		t.Errorf("Args() = %q, want %q", got, want)
	}
	if got, want := strings.Join(l.UpdateArgs("app"), " "), "update --cpus 2 --memory 4g --pids-limit 512 app"; got != want {
		t.Errorf("UpdateArgs() = %q, want %q", got, want)
	}
}

func TestLimitsValidate(t *testing.T) {
	ok := []Limits{
		{},
		{CPUs: "2"}, {CPUs: "1.5"},
		{Memory: "512m"}, {Memory: "4g", MemorySwap: "8g"},
		{Memory: "4g", MemorySwap: "-1"},
		{PidsLimit: -1}, {PidsLimit: 100},
	}
	for _, l := range ok {
		if err := l.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", l, err)
		}
	}

	bad := map[string]Limits{
		// Accepted by ParseFloat, rejected by docker.
		"cpus 1e3":            {CPUs: "1e3"},
		"cpus NaN":            {CPUs: "NaN"},
		"cpus zero":           {CPUs: "0"},
		"cpus negative":       {CPUs: "-1"},
		"memory nonsense":     {Memory: "many"},
		"swap without memory": {MemorySwap: "4g"},
		// memory-swap is the TOTAL, so it can never be below memory.
		"swap below memory": {Memory: "4g", MemorySwap: "2g"},
		"pids below -1":     {PidsLimit: -2},
	}
	for name, l := range bad {
		if err := l.Validate(); err == nil {
			t.Errorf("Validate(%s) = nil, want an error", name)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"1024": 1024, "1k": 1024, "1m": 1024 * 1024, "1g": 1024 * 1024 * 1024,
		"1K": 1024, "1G": 1024 * 1024 * 1024, "512b": 512, "1.5g": 1610612736,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "many", "1x", "-1"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = nil error", in)
		}
	}
}

// --- spec ---

func baseSpec() Spec {
	return Spec{
		Name:     "app",
		Image:    "app:v2",
		Network:  "app-net",
		Binds:    []string{"/srv/data:/data"},
		Env:      map[string]string{"MODE": "prod"},
		EnvSet:   map[string]string{"TOKEN": "s3cr3t"},
		Labels:   map[string]string{"team": "infra"},
		GroupAdd: []string{"999"},
	}
}

// Callers log these, diff them, and paste them into a shell. Flags that
// reshuffle between runs are useless for all three.
func TestCreateArgsIsDeterministic(t *testing.T) {
	s := baseSpec()
	s.Env = map[string]string{"B": "2", "A": "1", "C": "3"}
	s.Labels = map[string]string{"z": "1", "a": "2"}

	first, err := s.CreateArgs()
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, err := s.CreateArgs()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("CreateArgs is not deterministic:\n%v\n%v", first, again)
		}
	}
	joined := strings.Join(first, " ")
	if !strings.Contains(joined, "-e A=1 -e B=2 -e C=3") {
		t.Errorf("env should be sorted: %s", joined)
	}
	if first[len(first)-1] != "app:v2" {
		t.Errorf("image must come last, got %v", first)
	}
}

func TestCreateArgsWritesTheNetworkLabel(t *testing.T) {
	args, err := baseSpec().CreateArgs()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "--label io.dockerkit.network=app-net") {
		t.Fatalf("the network should be recorded as a label: %v", args)
	}
}

func TestCreateArgsRejectsIncompleteSpecs(t *testing.T) {
	for name, s := range map[string]Spec{
		"no name":    {Image: "app:v2"},
		"no image":   {Name: "app"},
		"bad limits": {Name: "app", Image: "app:v2", Limits: Limits{CPUs: "many"}},
	} {
		if _, err := s.CreateArgs(); err == nil {
			t.Errorf("CreateArgs(%s) = nil error", name)
		}
	}
}

func TestCreateArgsCmdFollowsImage(t *testing.T) {
	s := Spec{Name: "app", Image: "app:v2", Cmd: []string{"serve", "--port", "80"}}
	args, err := s.CreateArgs()
	if err != nil {
		t.Fatal(err)
	}
	tail := strings.Join(args[len(args)-4:], " ")
	if tail != "app:v2 serve --port 80" {
		t.Fatalf("cmd should follow the image, got %q", tail)
	}
}

// --- drift ---

// baseSpec describes exactly the fixture container, so anything Drift reports
// here is a false positive.
func TestNoDriftWhenTheyAgree(t *testing.T) {
	s := baseSpec()
	if d := s.Drift(facts(t), "sha256:abc123"); len(d) != 0 {
		t.Fatalf("Drift = %v, want none", d)
	}
	if d := s.Drift(nil, ""); d != nil {
		t.Fatalf("Drift(nil facts) = %v, want nil", d)
	}
}

func TestImageDrift(t *testing.T) {
	s := baseSpec()
	s.Image = "app:v3"
	d := s.Drift(facts(t), "")
	if len(d) != 1 || d[0].Field != "image" || d[0].Old != "app:v2" || d[0].New != "app:v3" {
		t.Fatalf("Drift = %v", d)
	}
}

// Same tag, different contents: rebuilt in place.
func TestRebuiltImageIsDrift(t *testing.T) {
	d := baseSpec().Drift(facts(t), "sha256:NEW")
	if len(d) != 1 || d[0].Field != "image" || d[0].Old != "rebuilt" {
		t.Fatalf("Drift = %v, want a rebuilt-image diff", d)
	}
	// Image absent locally: compare the reference only, never pull.
	if d := baseSpec().Drift(facts(t), ""); len(d) != 0 {
		t.Fatalf("Drift with unknown image id = %v, want none", d)
	}
}

// The case that motivates recording the network as a label: a container
// attached to BOTH the old and the new network passes an "is it attached"
// test, leaving the old attachment in place -- exactly what the edit was
// meant to remove.
func TestNetworkDriftUsesTheCreationLabelNotAttachments(t *testing.T) {
	f := facts(t) // label says app-net; attached to app-net and extra-net
	s := baseSpec()
	s.Network = "extra-net"

	d := s.Drift(f, "sha256:abc123")
	found := false
	for _, x := range d {
		if x.Field == "network" && x.Old == "app-net" && x.New == "extra-net" {
			found = true
		}
	}
	if !found {
		t.Fatalf("attachment to extra-net must not hide that it was CREATED on app-net: %v", d)
	}
}

// Containers created before the label existed fall back to NetworkMode, and
// the rebuild that follows writes the label -- so it happens at most once.
func TestNetworkDriftFallsBackToNetworkMode(t *testing.T) {
	f := facts(t)
	delete(f.Labels, "io.dockerkit.network")

	s := baseSpec()
	s.Network = "other-net"
	d := s.Drift(f, "sha256:abc123")
	if len(d) == 0 || d[0].Field != "network" || d[0].Old != "app-net" {
		t.Fatalf("Drift = %v, want a network diff from NetworkMode", d)
	}

	s.Network = "app-net"
	for _, x := range s.Drift(f, "sha256:abc123") {
		if x.Field == "network" {
			t.Fatalf("NetworkMode already matches, should not be drift: %v", x)
		}
	}
}

func TestBindDrift(t *testing.T) {
	s := baseSpec()
	s.Binds = []string{"/srv/moved:/data", "/srv/new:/extra"}
	d := s.Drift(facts(t), "sha256:abc123")

	var fields []string
	for _, x := range d {
		fields = append(fields, x.String())
	}
	joined := strings.Join(fields, "; ")
	if !strings.Contains(joined, "mount /data: /srv/data → /srv/moved") {
		t.Errorf("a moved mount should be reported: %s", joined)
	}
	if !strings.Contains(joined, "mount /extra: (none) → /srv/new") {
		t.Errorf("a missing mount should be reported: %s", joined)
	}
}

// Env is compared by value; EnvSet only for presence.
func TestEnvDriftComparesValues(t *testing.T) {
	s := baseSpec()
	s.Env = map[string]string{"TOKEN": "different"}
	s.EnvSet = nil

	d := s.Drift(facts(t), "sha256:abc123")
	if len(d) != 1 || d[0].Field != "env TOKEN" || d[0].New != "different" {
		t.Fatalf("Drift = %v, want an env value diff", d)
	}
}

// Rotating a secret is a runtime concern; deleting a working container to
// inject a value it will be handed anyway is not an improvement.
func TestEnvSetIgnoresTheValue(t *testing.T) {
	s := baseSpec()
	s.EnvSet = map[string]string{"TOKEN": "a-completely-different-token"}
	for _, x := range s.Drift(facts(t), "sha256:abc123") {
		if x.Field == "env TOKEN" {
			t.Fatalf("EnvSet must not compare values: %v", x)
		}
	}
}

// Absence is different in kind: the consumer reads an empty value and usually
// falls back to "no authentication", silently.
func TestEnvSetReportsAbsentOrEmpty(t *testing.T) {
	for name, envs := range map[string][]string{
		"absent": {"PATH=/usr/bin"},
		// An image can ship `ENV TOKEN=` of its own.
		"empty":      {"TOKEN="},
		"whitespace": {"TOKEN=   "},
	} {
		t.Run(name, func(t *testing.T) {
			f := facts(t)
			f.Env = envs

			s := Spec{Name: "app", Image: "app:v2", EnvSet: map[string]string{"TOKEN": "s3cr3t"}}
			d := s.Drift(f, "")
			if len(d) != 1 || d[0].Field != "env TOKEN" || d[0].New != "set" {
				t.Fatalf("Drift = %v, want env TOKEN: (none) → set", d)
			}
		})
	}
}

// Only what the spec asks for is compared: images legitimately set their own
// variables, mounts and labels, and a container is not wrong for carrying them.
func TestExtraValuesOnTheContainerAreNotDrift(t *testing.T) {
	s := Spec{Name: "app", Image: "app:v2"}
	if d := s.Drift(facts(t), "sha256:abc123"); len(d) != 0 {
		t.Fatalf("Drift = %v; a spec that asks for nothing should find nothing", d)
	}
}

func TestLabelAndGroupDrift(t *testing.T) {
	s := baseSpec()
	s.Labels = map[string]string{"team": "platform", "new": "1"}
	s.GroupAdd = []string{"1001"}

	joined := DiffsString(s.Drift(facts(t), "sha256:abc123"))
	for _, want := range []string{
		"label new: (none) → 1",
		"label team: infra → platform",
		"group_add: 999 → 1001",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
}

func TestDiffString(t *testing.T) {
	cases := map[Diff]string{
		{Field: "image", Old: "a", New: "b"}: "image: a → b",
		{Field: "image", New: "b"}:           "image: (none) → b",
		{Field: "image", Old: "a"}:           "image: a → (none)",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("Diff.String() = %q, want %q", got, want)
		}
	}
	if DiffsString(nil) != "" {
		t.Error("DiffsString(nil) should be empty")
	}
}

// --- locks ---

// Two lifecycle paths crossing on one container is how "recreate" deletes the
// container the other just created.
func TestLocksSerializePerName(t *testing.T) {
	var locks Locks
	var wg sync.WaitGroup
	var inside atomic.Int32
	var overlapped atomic.Bool

	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer locks.Lock("app")()
			if inside.Add(1) != 1 {
				overlapped.Store(true)
			}
			runtime.Gosched()
			inside.Add(-1)
		}()
	}
	wg.Wait()

	if overlapped.Load() {
		t.Fatal("two goroutines were inside the critical section at once")
	}

	// Different names must not block one another: taking b while a is held
	// has to return rather than deadlock.
	releaseA := locks.Lock("a")
	releaseB := locks.Lock("b")
	releaseB()
	releaseA()
}

func TestSocketGIDMissingPath(t *testing.T) {
	if gid := SocketGID("/definitely/not/here"); gid != -1 {
		t.Fatalf("SocketGID = %d, want -1", gid)
	}
}

func TestRunUsesTheInjectedExec(t *testing.T) {
	var gotArgs []string
	r := Runner{Binary: "podman", Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return []byte("ok"), nil
	}}
	out, err := r.Run(context.Background(), "ps", "-a")
	if err != nil || string(out) != "ok" {
		t.Fatalf("Run = %q, %v", out, err)
	}
	if !reflect.DeepEqual(gotArgs, []string{"podman", "ps", "-a"}) {
		t.Fatalf("args = %v", gotArgs)
	}
}
