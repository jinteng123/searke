package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"syscall"
	"time"
)

// Executor carries out commands and file transfers on one node, either
// over SSH (Client) or directly on this machine (LocalExecutor).
type Executor interface {
	Run(cmd string) (*Result, error)
	RunSudo(cmd string) (*Result, error)
	RunStdin(cmd string, stdin []byte) (*Result, error)
	PrepareStaging() error
	UploadFile(local, remote string, mode os.FileMode) error
	DownloadFile(remote, local string) error
	WriteFile(remote string, content []byte, mode os.FileMode) error
	Close() error
	Addr() string
}

var _ Executor = (*LocalExecutor)(nil)

// LocalExecutor implements Executor by running commands and copying
// files on this machine instead of over SSH. Privileged operations run
// directly when searke itself is root (the intended usage:
// `sudo searke apply`), or via `sudo -n` for a non-root user when
// spec.ssh.sudo is set.
type LocalExecutor struct {
	sudo bool   // wrap privileged commands in sudo -n
	user string // staging dir owner on the non-root path
	addr string
}

// NewLocalExecutor builds an executor that operates on the local host.
func NewLocalExecutor(address string, cfg SSHConfig) (*LocalExecutor, error) {
	u := cfg.User
	if u == "" {
		if cur, err := user.Current(); err == nil {
			u = cur.Username
		}
	}
	return &LocalExecutor{sudo: cfg.Sudo, user: u, addr: address}, nil
}

// useSudo reports whether privileged commands need a sudo wrapper:
// root needs nothing, a non-root user needs it only when allowed by
// spec.ssh.sudo (mirroring the SSH client's semantics).
func (l *LocalExecutor) useSudo() bool {
	return l.sudo && os.Geteuid() != 0
}

func (l *LocalExecutor) Run(cmd string) (*Result, error) {
	return l.run(cmd, false, nil)
}

func (l *LocalExecutor) RunSudo(cmd string) (*Result, error) {
	return l.run(cmd, true, nil)
}

func (l *LocalExecutor) RunStdin(cmd string, stdin []byte) (*Result, error) {
	return l.run(cmd, true, stdin)
}

// RunSudoStreamContext runs a local privileged command and forwards output
// as it is written instead of buffering it until exit.
func (l *LocalExecutor) RunSudoStreamContext(ctx context.Context, cmd string, stdout, stderr io.Writer) (*Result, error) {
	name, args := "sh", []string{"-c", cmd}
	if l.useSudo() {
		name, args = "sudo", append([]string{"-n", "sh"}, args...)
	}
	c := exec.CommandContext(ctx, name, args...)
	// CommandContext kills the process on cancellation, but SIGKILL sent
	// to sudo cannot be relayed to the privileged child — the child (and
	// its pipes) would survive, so Run() could block forever on journalctl
	// or leave install.sh orphaned after Ctrl-C. Signal the wrapper so
	// sudo forwards SIGTERM to its child, and bound the wait so a stuck
	// pipeline still returns promptly.
	c.Cancel = func() error {
		if c.Process == nil {
			return os.ErrProcessDone
		}
		return c.Process.Signal(syscall.SIGTERM)
	}
	c.WaitDelay = 3 * time.Second
	var outBuf, errBuf bytes.Buffer
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	c.Stdout = io.MultiWriter(&outBuf, stdout)
	c.Stderr = io.MultiWriter(&errBuf, stderr)
	err := c.Run()
	res := &Result{Stdout: outBuf.String(), Stderr: errBuf.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

func (l *LocalExecutor) run(cmd string, sudo bool, stdin []byte) (*Result, error) {
	name, args := "sh", []string{"-c", cmd}
	if sudo && l.useSudo() {
		name, args = "sudo", append([]string{"-n", "sh"}, args...)
	}
	c := exec.Command(name, args...)
	var outBuf, errBuf bytes.Buffer
	c.Stdout = &outBuf
	c.Stderr = &errBuf
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	err := c.Run()
	res := &Result{Stdout: outBuf.String(), Stderr: errBuf.String()}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return nil, err
	}
	return res, nil
}

// PrepareStaging creates the staging dir, chowning it to the invoking
// user on the non-root path so the follow-up copy can write.
func (l *LocalExecutor) PrepareStaging() error {
	if !l.useSudo() {
		res, err := l.Run("mkdir -p " + shellQuote(StagingDir))
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("prepare staging: %s", res.Stderr)
		}
		return nil
	}
	res, err := l.RunSudo(fmt.Sprintf("mkdir -p %s && chown %s %s",
		shellQuote(StagingDir), shellQuote(l.user), shellQuote(StagingDir)))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("prepare staging: %s", res.Stderr)
	}
	return nil
}

// UploadFile copies a local file into place. As root (or without sudo
// configured) it is a plain copy; as a non-root user with sudo it stages
// in StagingDir and installs as root, the same dance as over SFTP.
func (l *LocalExecutor) UploadFile(local, remote string, mode os.FileMode) error {
	return l.UploadFileProgress(local, remote, mode, nil)
}

// UploadFileProgress copies a local file and reports actual bytes copied.
func (l *LocalExecutor) UploadFileProgress(local, remote string, mode os.FileMode,
	progress func(written, total int64)) error {
	if !l.useSudo() {
		return copyFileProgress(local, remote, mode, progress)
	}
	stage := StagingDir + "/" + filepath.Base(remote)
	if err := copyFileProgress(local, stage, mode, progress); err != nil {
		return err
	}
	res, err := l.RunSudo(fmt.Sprintf("install -D -m %o -o root -g root %s %s && rm -f %s",
		mode, shellQuote(stage), shellQuote(remote), shellQuote(stage)))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("install %s: %s", remote, res.Stderr)
	}
	return nil
}

// DownloadFile copies a local file to the given path, reading through
// sudo when the file is root-owned.
func (l *LocalExecutor) DownloadFile(remote, local string) error {
	f, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	name, args := "sh", []string{"-c", "cat " + shellQuote(remote)}
	if l.useSudo() {
		name, args = "sudo", append([]string{"-n", "sh"}, args...)
	}
	c := exec.Command(name, args...)
	c.Stdout = f
	var errBuf bytes.Buffer
	c.Stderr = &errBuf
	if err := c.Run(); err != nil {
		os.Remove(local)
		return fmt.Errorf("download %s: %v: %s", remote, err, errBuf.String())
	}
	return nil
}

// WriteFile writes small text content to a path as root, same remote
// semantics as the SSH client: temp file + rename, never a truncated
// file on interruption.
func (l *LocalExecutor) WriteFile(remote string, content []byte, mode os.FileMode) error {
	tmp := remote + ".searke-tmp"
	res, err := l.RunStdin(fmt.Sprintf(
		"umask 077; tee %s > /dev/null && chmod %o %s && mv %s %s",
		shellQuote(tmp), mode, shellQuote(tmp), shellQuote(tmp), shellQuote(remote)), content)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s: %s", remote, res.Stderr)
	}
	return nil
}

// Close is a no-op; there is no connection to release.
func (l *LocalExecutor) Close() error { return nil }

// Addr returns the node address the executor was built for.
func (l *LocalExecutor) Addr() string { return l.addr }

// copyFile streams src to dst, creating parents and applying mode.
// On the same filesystem it hard-links instead of copying — the
// ~800 MiB payload then costs zero extra I/O on local execution.
func copyFile(src, dst string, mode os.FileMode) error {
	return copyFileProgress(src, dst, mode, nil)
}

func copyFileProgress(src, dst string, mode os.FileMode,
	progress func(written, total int64)) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	total := info.Size()
	if progress != nil {
		progress(0, total)
	}
	// Replace any stale target, then try a hard link — a failed link
	// (cross-device) just falls through to the copy below.
	os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		if err := os.Chmod(dst, mode); err != nil {
			return err
		}
		if progress != nil {
			progress(total, total)
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	reader := &progressReader{r: in, total: total, report: progress}
	_, cpErr := io.Copy(out, reader)
	clErr := out.Close()
	if cpErr != nil {
		return cpErr
	}
	if clErr != nil {
		return clErr
	}
	if progress != nil && total == 0 {
		progress(0, 0)
	}
	return os.Chmod(dst, mode)
}
