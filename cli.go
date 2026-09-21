// Package dockerkit drives the docker CLI: running commands, reading what a
// container was actually created with, and reporting where that has drifted
// from what the configuration now says it should be.
//
// It shells out to `docker` rather than speaking to the daemon's API, and has
// no dependencies beyond the standard library. That is a deliberate trade: a
// tool that already requires the docker CLI on the host pays nothing extra,
// and gets error messages operators can reproduce by pasting the command.
package dockerkit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

// HostSocket is the conventional path of the host's docker socket.
const HostSocket = "/var/run/docker.sock"

// Runner executes docker commands. The zero value runs the `docker` binary
// found on PATH; tests can substitute a fake.
type Runner struct {
	// Binary is the docker executable. Empty means "docker".
	Binary string

	// Exec replaces the command execution entirely. When non-nil it is called
	// instead of running a process, which is how tests avoid needing a daemon.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (r Runner) binary() string {
	if r.Binary == "" {
		return "docker"
	}
	return r.Binary
}

// Run executes a docker command and returns its combined output.
//
// Combined, not separated: docker writes some of what an operator needs to
// stderr and some to stdout, and which is which varies by subcommand and
// version. Splitting them means half the diagnosis goes missing from whichever
// stream the caller forgot to read.
func (r Runner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if r.Exec != nil {
		return r.Exec(ctx, r.binary(), args...)
	}
	return exec.CommandContext(ctx, r.binary(), args...).CombinedOutput()
}

// Default is the Runner used by the package-level helpers.
var Default = Runner{}

// Run is Default.Run.
func Run(ctx context.Context, args ...string) ([]byte, error) { return Default.Run(ctx, args...) }

// NotFound reports whether docker's output means "no such container".
//
// Matched on the message rather than the exit status because docker uses the
// same non-zero status for "it isn't there" and "I couldn't reach the daemon",
// and those call for opposite reactions: the first is usually fine, the second
// never is.
//
// Localized daemons are covered for the languages seen in the wild; a missed
// locale degrades to "some other error", which is the safe direction.
func NotFound(out []byte) bool {
	s := string(out)
	lower := strings.ToLower(s)
	if strings.Contains(lower, "no such container") || strings.Contains(lower, "no such object") {
		return true
	}
	return strings.Contains(s, "没有此容器") ||
		strings.Contains(s, "没有找到容器") ||
		strings.Contains(s, "未找到容器")
}

// PermissionDenied reports whether docker's output means the caller cannot
// reach the daemon: no permission on the socket, or nothing listening.
//
// Worth separating from every other failure because the fix is completely
// different -- a group membership or a mount, not anything about the command
// that was run.
func PermissionDenied(out []byte) bool {
	lower := strings.ToLower(string(out))
	return strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "permissions have not been granted") ||
		strings.Contains(lower, "cannot connect to the docker daemon") ||
		strings.Contains(lower, "is the docker daemon running") ||
		strings.Contains(lower, "connection refused")
}

// UnrecoverableStart reports whether `docker start` failed for a reason that
// starting again will never fix, because the container was created against
// something that no longer exists -- most often a network removed by
// `docker compose down`.
//
// The container has to be deleted and recreated. Callers that retry instead
// will retry forever.
func UnrecoverableStart(out []byte) bool {
	lower := strings.ToLower(string(out))
	if strings.Contains(lower, "network") &&
		(strings.Contains(lower, "not found") || strings.Contains(lower, "no such")) {
		return true
	}
	return strings.Contains(lower, "could not find network") ||
		strings.Contains(lower, "could not attach to network") ||
		strings.Contains(lower, "failed to create endpoint") ||
		strings.Contains(lower, "failed to get network")
}

// Error wraps a failed docker command with its output and, when the failure is
// a daemon-access problem, with the hint that actually resolves it.
//
// An error that says only "exit status 1" costs the reader a round trip to the
// host to find out what docker said. The output is the diagnosis; carry it.
func Error(op string, out []byte, err error) error {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		// An empty body with a non-zero status is itself informative: it
		// usually means the process was killed rather than having failed.
		trimmed = "(no output)"
	}
	if PermissionDenied(out) {
		return fmt.Errorf("%s failed (cannot reach the docker daemon). %s. output: %s: %w",
			op, AccessHint, trimmed, err)
	}
	return fmt.Errorf("%s failed. output: %s: %w", op, trimmed, err)
}

// AccessHint is appended to daemon-access errors. It is a variable so a caller
// whose deployment has a more specific remedy can say so instead.
var AccessHint = "if the caller runs in a container, give it the host's docker group " +
	"(group_add with the gid of " + HostSocket + "), or run it as root"

// SocketGID returns the group that owns path, or -1 when that cannot be
// determined.
//
// Used to grant a container access to a mounted docker socket: when the socket
// comes from the host, stat inside the container still reports the host's gid,
// which is the number `--group-add` needs.
func SocketGID(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(st.Gid)
}

// Locks serializes operations per container name. The zero value is ready.
//
// Lifecycle operations on one container are not safe to interleave. A
// supervisor typically has several paths that can start the same container --
// a boot-time sweep, a periodic reconciler, an API call -- and once "recreate"
// means `docker rm` followed by `docker create`, two of them crossing can
// delete the container the other just created. A name collision is a logged
// annoyance; this is a container that silently isn't there.
type Locks struct {
	mu sync.Map // name -> *sync.Mutex
}

// Lock takes the lock for name and returns the function that releases it,
// so callers can write:
//
//	defer locks.Lock(name)()
func (l *Locks) Lock(name string) func() {
	v, _ := l.mu.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
