package dockerkit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Facts are the values `docker inspect` reports for a container, reduced to
// the ones that say how it was created.
//
// Only these fields are decoded. A container's inspect output is large and
// mostly irrelevant to the question "does this match the configuration", and
// decoding into a full struct would break every time docker adds a field.
type Facts struct {
	Running bool
	Status  string

	// ImageRef is the image as named at creation, e.g. "app:v2".
	ImageRef string
	// ImageID is the image actually in use. It changes when a tag is rebuilt
	// or re-pulled while the reference stays the same.
	ImageID string

	Binds       []string
	Env         []string
	Labels      map[string]string
	GroupAdd    []string
	NetworkMode string
	// Networks is every network the container is attached to, NetworkMode
	// first. A container can be attached to more after creation.
	Networks []string
}

type inspectRaw struct {
	Image string `json:"Image"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Binds       []string `json:"Binds"`
		GroupAdd    []string `json:"GroupAdd"`
		NetworkMode string   `json:"NetworkMode"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Inspect returns what the container was created with, or (nil, nil) when
// there is no such container.
//
// Absence is not an error: callers almost always want to branch on "does it
// exist" rather than parse an error string to find out.
func (r Runner) Inspect(ctx context.Context, name string) (*Facts, error) {
	out, err := r.Run(ctx, "inspect", name)
	if err != nil {
		if NotFound(out) {
			return nil, nil
		}
		return nil, Error("docker inspect", out, err)
	}
	return ParseInspect(out)
}

// Inspect is Default.Inspect.
func Inspect(ctx context.Context, name string) (*Facts, error) { return Default.Inspect(ctx, name) }

// ParseInspect decodes `docker inspect` output, which is an array.
func ParseInspect(out []byte) (*Facts, error) {
	var raw []inspectRaw
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing docker inspect output: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	c := raw[0]

	facts := &Facts{
		Running:     c.State.Running,
		Status:      c.State.Status,
		ImageRef:    c.Config.Image,
		ImageID:     c.Image,
		Binds:       c.HostConfig.Binds,
		Env:         c.Config.Env,
		Labels:      c.Config.Labels,
		GroupAdd:    c.HostConfig.GroupAdd,
		NetworkMode: c.HostConfig.NetworkMode,
	}
	if c.HostConfig.NetworkMode != "" {
		facts.Networks = append(facts.Networks, c.HostConfig.NetworkMode)
	}
	for name := range c.NetworkSettings.Networks {
		if name != c.HostConfig.NetworkMode {
			facts.Networks = append(facts.Networks, name)
		}
	}
	return facts, nil
}

// EnvValue returns the value of an environment variable, and whether it was
// set at all. An empty value that was set is not the same as unset.
func (f *Facts) EnvValue(key string) (string, bool) {
	if f == nil {
		return "", false
	}
	prefix := key + "="
	for _, e := range f.Env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

// BindSource returns the host path mounted at dest.
func (f *Facts) BindSource(dest string) (string, bool) {
	if f == nil {
		return "", false
	}
	for _, b := range f.Binds {
		parts := strings.Split(b, ":")
		if len(parts) >= 2 && parts[1] == dest {
			return parts[0], true
		}
	}
	return "", false
}

// ImageID resolves an image reference to the id it currently points at, or ""
// when the image is not present locally.
//
// "" on a missing image rather than an error, because the caller's next move
// is the same either way: compare only the reference, and let `docker create`
// pull if it needs to. Resolving must not become a reason to pull.
func (r Runner) ImageID(ctx context.Context, ref string) string {
	out, err := r.Run(ctx, "image", "inspect", "-f", "{{.Id}}", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ImageID is Default.ImageID.
func ImageID(ctx context.Context, ref string) string { return Default.ImageID(ctx, ref) }

// ImageIDCache memoizes image-id lookups for a short time.
//
// A status page listing N containers usually finds them all on the same image,
// and without a cache each refresh spawns N `docker image inspect` processes.
// The id only changes when an image is rebuilt or pulled, so a stale answer
// costs at most a few seconds' delay before a "configuration changed" badge
// appears.
//
// Do NOT use it on a lifecycle path. "Rebuild the image, then press start" is
// an ordinary thing to do, and a cached id there means the rebuild is silently
// ignored -- exactly the bug the drift check exists to catch.
type ImageIDCache struct {
	// TTL is how long an entry stays fresh. Zero means DefaultImageIDTTL.
	TTL time.Duration
	// Runner is the runner used for lookups. The zero value uses Default.
	Runner Runner

	entries sync.Map // ref -> imageIDEntry
}

// DefaultImageIDTTL is the ImageIDCache lifetime when TTL is zero.
const DefaultImageIDTTL = 10 * time.Second

type imageIDEntry struct {
	id string
	at time.Time
}

// Get returns the image id for ref, from cache when fresh.
func (c *ImageIDCache) Get(ctx context.Context, ref string) string {
	ttl := c.TTL
	if ttl == 0 {
		ttl = DefaultImageIDTTL
	}
	if v, ok := c.entries.Load(ref); ok {
		if e := v.(imageIDEntry); time.Since(e.at) < ttl {
			return e.id
		}
	}
	id := c.Runner.ImageID(ctx, ref)
	c.entries.Store(ref, imageIDEntry{id: id, at: time.Now()})
	return id
}
