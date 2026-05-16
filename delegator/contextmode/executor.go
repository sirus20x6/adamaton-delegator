package contextmode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ExecResult is the raw output of a script execution before any
// indexing or compression.
type ExecResult struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Truncated bool
	Duration  time.Duration
	TimedOut  bool
}

// ExecOptions controls a single script invocation.
type ExecOptions struct {
	// Language picks the runtime. Detect() resolves bash/python/node/go.
	Language Language
	// Script is the source code to execute.
	Script string
	// WorkingDir is the cwd for the subprocess (defaults to process cwd).
	WorkingDir string
	// Env is the literal environment slice, replacing os.Environ() when
	// non-nil. Pass nil to inherit. Empty slice means no env at all.
	Env []string
	// Timeout is the wall-clock budget. <= 0 → 60s.
	Timeout time.Duration
	// MaxBytes caps each stream (stdout, stderr) before truncation.
	// 0 → 10MB.
	MaxBytes int
}

// Run spawns a script in the chosen language, waits for it (or the
// timeout) and returns the captured output. The subprocess is given
// its own process group so a hung child is reliably killed when ctx
// fires or the timeout elapses.
func Run(ctx context.Context, opts ExecOptions) (*ExecResult, error) {
	if opts.Script == "" {
		return nil, errors.New("script is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 10 * 1024 * 1024
	}
	binary, args, stdinScript, err := Command(opts.Language)
	if err != nil {
		return nil, err
	}
	if !stdinScript {
		return nil, fmt.Errorf("language %q does not support stdin script", opts.Language)
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Dir = opts.WorkingDir
	cmd.Env = opts.Env

	// Process-group leader so SIGKILL on cancel takes the children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Override the default Cancel: signal the whole process GROUP, not
	// just the leader. Otherwise children (e.g. `sleep` spawned by
	// bash) keep stdout/stderr fds open after bash dies and the Wait()
	// I/O goroutines block until those children finish naturally —
	// completely defeating the timeout.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Belt-and-braces: if the I/O goroutines are still copying after
	// the process has exited (e.g. a daemonised grandchild keeping the
	// pipe open), wait this long then force-close.
	cmd.WaitDelay = 2 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutBuf := &boundedBuffer{cap: opts.MaxBytes}
	stderrBuf := &boundedBuffer{cap: opts.MaxBytes}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start subprocess: %w", err)
	}

	// Pipe the script then close stdin — most interpreters refuse to
	// run until they see EOF on stdin.
	go func() {
		_, _ = io.WriteString(stdin, opts.Script)
		_ = stdin.Close()
	}()

	waitErr := cmd.Wait()
	dur := time.Since(start)

	res := &ExecResult{
		Stdout:    stdoutBuf.Bytes(),
		Stderr:    stderrBuf.Bytes(),
		Duration:  dur,
		Truncated: stdoutBuf.truncated || stderrBuf.truncated,
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.ExitCode = -1
		}
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}
	// Reap any orphans in the process group; ignore errors.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return res, nil
}

// boundedBuffer caps a stream at `cap` bytes. Writes past the cap are
// silently dropped and `truncated` is set so the caller can flag the
// output as incomplete.
type boundedBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.cap - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) <= remaining {
		return b.buf.Write(p)
	}
	_, _ = b.buf.Write(p[:remaining])
	b.truncated = true
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }
