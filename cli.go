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

// CommandError is a failed docker command, carrying the output docker produced
// alongside the underlying error.
//
// The output is a field rather than only a fragment of the message because the
// classifiers -- [NotFound], [PermissionDenied], [UnrecoverableStart] -- all
// take the raw output, and a caller that receives an error usually wants to
// ask one of them what kind of failure it is. Folding the output into a string
// leaves string matching as the only way to find out, on text this package is
// free to reword.
//
//	var cmdErr *dockerkit.CommandError
//	if errors.As(err, &cmdErr) && dockerkit.UnrecoverableStart(cmdErr.Output) {
//		// recreate the container rather than retrying the start
//	}
type CommandError struct {
	// Op is what was attempted, e.g. "docker inspect".
	Op string

	// Output is docker's combined output, verbatim and untrimmed. It can
	// contain registry credentials, mount paths and environment values --
	// see SECURITY.md.
	Output []byte

	// Err is the error the command failed with, usually an *exec.ExitError.
	Err error
}

// Error renders the operation, docker's output and the underlying error.
//
// An error that says only "exit status 1" costs the reader a round trip to the
// host to find out what docker said. The output is the diagnosis; carry it.
func (e *CommandError) Error() string {
	trimmed := strings.TrimSpace(string(e.Output))
	if trimmed == "" {
		// An empty body with a non-zero status is itself informative: it
		// usually means the process was killed rather than having failed.
		trimmed = "(no output)"
	}
	if PermissionDenied(e.Output) {
		return fmt.Sprintf("%s failed (cannot reach the docker daemon). %s. output: %s: %v",
			e.Op, AccessHint, trimmed, e.Err)
	}
	return fmt.Sprintf("%s failed. output: %s: %v", e.Op, trimmed, e.Err)
}

// Unwrap returns the underlying error, so errors.Is and errors.As reach it.
func (e *CommandError) Unwrap() error { return e.Err }

// WrapError wraps a failed docker command as a [CommandError].
//
// When the failure is a daemon-access problem the rendered message also
// carries [AccessHint], which is the hint that actually resolves it.
func WrapError(op string, out []byte, err error) error {
	return &CommandError{Op: op, Output: out, Err: err}
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
	mu      sync.Mutex
	entries map[string]*lockEntry
}

// lockEntry is one name's mutex, plus the number of callers currently holding
// or waiting for it. The count is what lets the entry be removed again: a
// supervisor whose container names change over time -- a job id, a timestamp
// -- would otherwise accumulate one mutex per name it ever saw, for the life
// of the process.
type lockEntry struct {
	mu   sync.Mutex
	refs int
}

// Lock takes the lock for name and returns the function that releases it,
// so callers can write:
//
//	defer locks.Lock(name)()
//
// The returned function must be called exactly once, like sync.Mutex.Unlock.
func (l *Locks) Lock(name string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*lockEntry)
	}
	e, ok := l.entries[name]
	if !ok {
		e = &lockEntry{}
		l.entries[name] = e
	}
	// Counted before the entry mutex is taken, so a caller waiting on it keeps
	// the entry alive while the current holder releases.
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()

	return func() {
		e.mu.Unlock()

		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.entries, name)
		}
		l.mu.Unlock()
	}
}
