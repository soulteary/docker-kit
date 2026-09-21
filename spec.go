package dockerkit

import (
	"fmt"
	"sort"
	"strings"
)

// Spec is what a container should look like under the current configuration.
//
// It is the single definition behind both halves of the problem: CreateArgs
// builds the container from it, and Drift compares an existing container
// against it. Keeping those in one place is the whole point -- when they are
// written separately, a change to the create path leaves the comparison
// checking the previous shape, and the drift it exists to catch goes
// unreported.
type Spec struct {
	// Name is the container name (--name).
	Name string
	// Image is the image reference, passed last.
	Image string
	// Network is the network to create the container on (--network).
	Network string
	// Binds are mounts in docker's "source:destination[:options]" form.
	Binds []string
	// Env are environment variables injected at creation (-e).
	Env map[string]string
	// EnvSet are environment variables whose value the caller supplies but
	// does not want compared. See Drift.
	EnvSet map[string]string
	// Labels are labels written at creation (--label).
	Labels map[string]string
	// GroupAdd are supplementary group ids (--group-add).
	GroupAdd []string
	// Limits are resource caps. The zero value adds nothing.
	Limits Limits
	// Extra are arguments appended verbatim, just before the image. The
	// escape hatch for flags this type does not model; they are NOT compared
	// by Drift, because inspect gives no general way to read them back.
	Extra []string
	// Cmd is the command and arguments after the image.
	Cmd []string

	// LabelPrefix namespaces the bookkeeping labels this package writes.
	// Empty means DefaultLabelPrefix.
	LabelPrefix string
}

// DefaultLabelPrefix namespaces the labels dockerkit writes for its own use.
const DefaultLabelPrefix = "io.dockerkit"

func (s Spec) labelPrefix() string {
	if s.LabelPrefix == "" {
		return DefaultLabelPrefix
	}
	return s.LabelPrefix
}

// NetworkLabel is the label recording the network the container was CREATED
// with, as opposed to the networks it happens to be attached to now.
func (s Spec) NetworkLabel() string { return s.labelPrefix() + ".network" }

// CreateArgs renders the spec as a `docker create` command.
//
// Deterministic: maps are emitted in sorted key order, so the same spec always
// produces the same command. Callers log these, diff them, and paste them into
// a shell; a command whose flags reshuffle between runs is useless for all
// three.
func (s Spec) CreateArgs() ([]string, error) {
	if strings.TrimSpace(s.Name) == "" {
		return nil, fmt.Errorf("spec has no container name")
	}
	if strings.TrimSpace(s.Image) == "" {
		return nil, fmt.Errorf("spec has no image")
	}
	if err := s.Limits.Validate(); err != nil {
		return nil, err
	}

	args := []string{"create", "--name", s.Name}

	for _, b := range s.Binds {
		args = append(args, "-v", b)
	}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
		// Recorded as a label as well, so Drift can tell what the container
		// was created with. See Drift for why that matters.
		args = append(args, "--label", s.NetworkLabel()+"="+s.Network)
	}
	for _, k := range sortedKeys(s.Labels) {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	for _, k := range sortedKeys(s.Env) {
		args = append(args, "-e", k+"="+s.Env[k])
	}
	for _, k := range sortedKeys(s.EnvSet) {
		args = append(args, "-e", k+"="+s.EnvSet[k])
	}
	for _, g := range s.GroupAdd {
		args = append(args, "--group-add", g)
	}
	args = append(args, s.Limits.Args()...)
	args = append(args, s.Extra...)

	args = append(args, s.Image)
	return append(args, s.Cmd...), nil
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Diff is one difference between an existing container and the spec.
//
// Field names the setting, in the caller's own configuration vocabulary where
// possible, so the message points at what to edit rather than at a docker
// flag. Old and New are rendered values; New is empty for "no longer wanted"
// and Old is empty for "was not set".
type Diff struct {
	Field string
	Old   string
	New   string
}

// String renders a diff as "field: old → new".
func (d Diff) String() string {
	switch {
	case d.Old == "":
		return fmt.Sprintf("%s: (none) → %s", d.Field, d.New)
	case d.New == "":
		return fmt.Sprintf("%s: %s → (none)", d.Field, d.Old)
	default:
		return fmt.Sprintf("%s: %s → %s", d.Field, d.Old, d.New)
	}
}

// Drift reports where an existing container differs from the spec, or nil when
// they agree.
//
// This is the check that makes configuration edits real. Image, network,
// mounts, environment and group membership are fixed at `docker create` and
// nothing afterwards changes them: `docker start` simply starts the container
// that already exists. Without a comparison, an edit appears to take effect --
// the config file says one thing, the UI says it was saved, the container
// keeps running with the old value -- and nothing anywhere reports the
// mismatch. The failure mode that motivated this was a container still
// mounting the host's docker socket long after the setting was changed to stop
// it, with no sign anything was wrong.
//
// desiredImageID, when non-empty, is the id the image reference currently
// resolves to. It catches a tag rebuilt in place, where the reference is
// unchanged but the contents are not. Pass "" when the image is not present
// locally and only the reference should be compared.
//
// Only what the spec asks for is compared. Environment variables, labels and
// mounts the container has but the spec does not mention are left alone,
// because images legitimately set their own and a container is not wrong for
// carrying them.
func (s Spec) Drift(f *Facts, desiredImageID string) []Diff {
	if f == nil {
		return nil
	}
	var diffs []Diff

	if f.ImageRef != "" && f.ImageRef != s.Image {
		diffs = append(diffs, Diff{Field: "image", Old: f.ImageRef, New: s.Image})
	} else if desiredImageID != "" && f.ImageID != "" && f.ImageID != desiredImageID {
		// Same reference, different contents: the tag was rebuilt or
		// re-pulled. Reported separately because "image: app:v2 → app:v2"
		// reads like a bug in the diff.
		diffs = append(diffs, Diff{Field: "image", Old: "rebuilt", New: s.Image})
	}

	if d := s.networkDrift(f); d != nil {
		diffs = append(diffs, *d)
	}
	diffs = append(diffs, s.bindDrift(f)...)
	diffs = append(diffs, s.envDrift(f)...)
	diffs = append(diffs, s.labelDrift(f)...)
	if d := s.groupDrift(f); d != nil {
		diffs = append(diffs, *d)
	}

	return diffs
}

// networkDrift compares the network the container was created with.
//
// "Is it attached to the right network" is the wrong question. `docker create
// --network` sets exactly one, but a container can be attached to more later
// with `docker network connect`. When the configuration changes from net-a to
// net-b and the container happens to be on both, an any-match test passes --
// leaving it attached to net-a, which is precisely the attachment the edit was
// meant to remove.
//
// The network is therefore recorded as a label at creation. NetworkMode is the
// fallback for containers created before the label existed: docker may
// normalise it, so comparing it strictly could rebuild on every start, but a
// rebuild writes the label, after which the comparison is exact. One
// unnecessary rebuild, once.
func (s Spec) networkDrift(f *Facts) *Diff {
	if s.Network == "" {
		return nil
	}
	if got, ok := f.Labels[s.NetworkLabel()]; ok {
		if got != s.Network {
			return &Diff{Field: "network", Old: got, New: s.Network}
		}
		return nil
	}
	if f.NetworkMode != "" {
		if f.NetworkMode != s.Network {
			return &Diff{Field: "network", Old: f.NetworkMode, New: s.Network}
		}
		return nil
	}
	// Neither label nor NetworkMode: fall back to "attached to it at all".
	// Like every other comparison here, prefer missing a difference over
	// destroying a container on a guess.
	if len(f.Networks) > 0 && !contains(f.Networks, s.Network) {
		return &Diff{Field: "network", Old: strings.Join(f.Networks, ","), New: s.Network}
	}
	return nil
}

func (s Spec) bindDrift(f *Facts) []Diff {
	var diffs []Diff
	for _, want := range s.Binds {
		parts := strings.SplitN(want, ":", 3)
		if len(parts) < 2 {
			continue
		}
		src, dest := parts[0], parts[1]
		got, ok := f.BindSource(dest)
		if !ok {
			diffs = append(diffs, Diff{Field: "mount " + dest, New: src})
			continue
		}
		if got != src {
			diffs = append(diffs, Diff{Field: "mount " + dest, Old: got, New: src})
		}
	}
	return diffs
}

// envDrift compares environment variables.
//
// Env is compared by value. EnvSet is compared only for presence, and that
// distinction is the reason it exists.
//
// A secret injected at creation -- a token the container authenticates with --
// should not be compared by value: rotating it is a runtime concern the
// container surfaces by failing to authenticate, and treating a mismatch as
// drift would delete a working container to inject a value it will be handed
// anyway. But ABSENCE is different in kind. A container created before the
// secret was introduced has no value at all, and code that reads an empty
// value usually falls back to "no authentication" -- silently, with nothing
// failing to reveal it. That one needs a rebuild.
//
// A value that is set but empty counts as absent, because that is how the
// consumer will read it, and an image can ship `ENV TOKEN=` of its own.
func (s Spec) envDrift(f *Facts) []Diff {
	var diffs []Diff
	for _, k := range sortedKeys(s.Env) {
		want := s.Env[k]
		got, ok := f.EnvValue(k)
		if !ok {
			diffs = append(diffs, Diff{Field: "env " + k, New: want})
			continue
		}
		if got != want {
			diffs = append(diffs, Diff{Field: "env " + k, Old: got, New: want})
		}
	}
	for _, k := range sortedKeys(s.EnvSet) {
		if s.EnvSet[k] == "" {
			continue
		}
		if got, ok := f.EnvValue(k); !ok || strings.TrimSpace(got) == "" {
			diffs = append(diffs, Diff{Field: "env " + k, New: "set"})
		}
	}
	return diffs
}

func (s Spec) labelDrift(f *Facts) []Diff {
	var diffs []Diff
	for _, k := range sortedKeys(s.Labels) {
		want := s.Labels[k]
		got, ok := f.Labels[k]
		if !ok {
			diffs = append(diffs, Diff{Field: "label " + k, New: want})
			continue
		}
		if got != want {
			diffs = append(diffs, Diff{Field: "label " + k, Old: got, New: want})
		}
	}
	return diffs
}

func (s Spec) groupDrift(f *Facts) *Diff {
	if len(s.GroupAdd) == 0 {
		return nil
	}
	for _, want := range s.GroupAdd {
		if !contains(f.GroupAdd, want) {
			return &Diff{
				Field: "group_add",
				Old:   strings.Join(f.GroupAdd, ","),
				New:   strings.Join(s.GroupAdd, ","),
			}
		}
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// DiffsString renders a list of diffs for a log line or a tooltip.
func DiffsString(diffs []Diff) string {
	if len(diffs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		parts = append(parts, d.String())
	}
	return strings.Join(parts, "; ")
}
