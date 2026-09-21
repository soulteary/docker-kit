package dockerkit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Limits are the resource caps applied to a container. An empty field means
// "no limit"; the values are passed through to docker unchanged.
//
// Worth setting even when it feels unnecessary: one runaway job in an
// unlimited container can exhaust the host's memory and take the supervisor
// down with it, turning a single failed task into an outage.
type Limits struct {
	// CPUs is --cpus, e.g. "2" or "1.5".
	CPUs string
	// Memory is --memory, e.g. "512m" or "4g".
	Memory string
	// MemorySwap is --memory-swap: the TOTAL of memory plus swap, not the
	// swap on its own. "-1" means unlimited swap.
	MemorySwap string
	// PidsLimit is --pids-limit. Positive applies a cap, -1 means unlimited,
	// 0 means "not set".
	PidsLimit int
}

var (
	// docker accepts a plain positive number for --cpus.
	cpusRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	// docker sizes: a byte count, optionally with a b/k/m/g suffix.
	sizeRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[bkmgBKMG]?$`)
)

// Args renders the limits as `docker create` arguments. Unset fields produce
// nothing, so the zero value adds no arguments at all.
func (l Limits) Args() []string {
	var args []string
	if v := strings.TrimSpace(l.CPUs); v != "" {
		args = append(args, "--cpus", v)
	}
	if v := strings.TrimSpace(l.Memory); v != "" {
		args = append(args, "--memory", v)
	}
	if v := strings.TrimSpace(l.MemorySwap); v != "" {
		args = append(args, "--memory-swap", v)
	}
	if l.PidsLimit != 0 {
		args = append(args, "--pids-limit", strconv.Itoa(l.PidsLimit))
	}
	return args
}

// UpdateArgs renders the limits as a `docker update` command for an existing
// container, or nil when there is nothing to apply.
//
// The limits in Args only take effect at creation. A deployment that adds
// limits after its containers already exist gets no benefit from them, and
// stopping and starting does not help either -- the container keeps whatever
// it was created with. `docker update` applies them to what is already there,
// without a rebuild.
func (l Limits) UpdateArgs(name string) []string {
	args := l.Args()
	if len(args) == 0 {
		return nil
	}
	return append(append([]string{"update"}, args...), name)
}

// Validate rejects values docker would reject, so the error arrives while the
// configuration is being read rather than at `docker create` time -- by which
// point the message has lost all trace of which setting caused it.
func (l Limits) Validate() error {
	if v := strings.TrimSpace(l.CPUs); v != "" {
		// The regexp rejects forms ParseFloat accepts but docker does not,
		// such as "1e3" and "NaN"; ParseFloat then checks the value is
		// positive.
		f, err := strconv.ParseFloat(v, 64)
		if !cpusRe.MatchString(v) || err != nil || f <= 0 {
			return fmt.Errorf("cpus must be a positive number such as \"2\" or \"1.5\", got %q", l.CPUs)
		}
	}
	if v := strings.TrimSpace(l.Memory); v != "" && !sizeRe.MatchString(v) {
		return fmt.Errorf("memory must be a docker size such as \"512m\" or \"4g\", got %q", l.Memory)
	}
	if v := strings.TrimSpace(l.MemorySwap); v != "" && v != "-1" && !sizeRe.MatchString(v) {
		return fmt.Errorf("memory_swap must be a docker size or \"-1\", got %q", l.MemorySwap)
	}
	if l.PidsLimit < -1 {
		return fmt.Errorf("pids_limit must be positive or -1 (unlimited), got %d", l.PidsLimit)
	}

	memory := strings.TrimSpace(l.Memory)
	swap := strings.TrimSpace(l.MemorySwap)
	if swap != "" && memory == "" {
		return fmt.Errorf("memory_swap requires memory to be set as well; docker refuses to create the container otherwise")
	}
	// memory-swap is the total of memory plus swap, so it can never be the
	// smaller of the two. Reading it as "how much swap" and setting it below
	// memory is the usual mistake, and docker only says so at create time.
	if swap != "" && swap != "-1" && memory != "" {
		memBytes, memErr := ParseSize(memory)
		swapBytes, swapErr := ParseSize(swap)
		if memErr == nil && swapErr == nil && swapBytes < memBytes {
			return fmt.Errorf("memory_swap (%s) cannot be smaller than memory (%s): it is the total of memory plus swap, and docker refuses to create the container",
				l.MemorySwap, l.Memory)
		}
	}
	return nil
}

// ParseSize converts a docker size ("4g", "512m", "1048576") to bytes.
func ParseSize(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if !sizeRe.MatchString(v) {
		return 0, fmt.Errorf("not a docker size: %q", v)
	}
	mult := int64(1)
	switch last := v[len(v)-1]; last {
	case 'b', 'B':
		v = v[:len(v)-1]
	case 'k', 'K':
		mult, v = 1024, v[:len(v)-1]
	case 'm', 'M':
		mult, v = 1024*1024, v[:len(v)-1]
	case 'g', 'G':
		mult, v = 1024*1024*1024, v[:len(v)-1]
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("not a docker size: %q", v)
	}
	return int64(n * float64(mult)), nil
}
