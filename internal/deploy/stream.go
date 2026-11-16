package deploy

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// streamExecutor is an optional capability so the core Executor contract
// stays small for callers that don't need live output.
type streamExecutor interface {
	RunSudoStreamContext(context.Context, string, io.Writer, io.Writer) (*Result, error)
}

type progressUploader interface {
	UploadFileProgress(string, string, os.FileMode, func(written, total int64)) error
}

type nodeLineWriter struct {
	mu      sync.Mutex
	opts    *Options
	node    string
	stream  string
	partial []byte
}

func newNodeLineWriter(opts *Options, node, stream string) *nodeLineWriter {
	return &nodeLineWriter{opts: opts, node: node, stream: stream}
}

func (w *nodeLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	consumed := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.partial = append(w.partial, p...)
			break
		}
		w.partial = append(w.partial, p[:i]...)
		w.emit(string(w.partial))
		w.partial = w.partial[:0]
		p = p[i+1:]
	}
	return consumed, nil
}

func (w *nodeLineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) == 0 {
		return
	}
	w.emit(string(w.partial))
	w.partial = w.partial[:0]
}

func (w *nodeLineWriter) emit(line string) {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return
	}
	prefix := "[" + w.node + "]"
	if w.stream != "stdout" {
		prefix += " [" + w.stream + "]"
	}
	logf(w.opts, "%s %s", prefix, line)
}

func flushWriter(w io.Writer) {
	if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}
}

// runSudoStreaming uses live SSH/local streams when supported. Small test or
// third-party Executor implementations retain their existing contract and
// have captured output flushed after completion.
func runSudoStreaming(ctx context.Context, ex Executor, cmd string, opts *Options, node string) (*Result, error) {
	stdout := newNodeLineWriter(opts, node, "stdout")
	stderr := newNodeLineWriter(opts, node, "stderr")
	var res *Result
	var err error
	if sx, ok := ex.(streamExecutor); ok {
		res, err = sx.RunSudoStreamContext(ctx, cmd, stdout, stderr)
	} else {
		res, err = ex.RunSudo(cmd)
		if res != nil {
			_, _ = io.WriteString(stdout, res.Stdout)
			_, _ = io.WriteString(stderr, res.Stderr)
		}
	}
	flushWriter(stdout)
	flushWriter(stderr)
	return res, err
}

func runSudoStreamingWithHeartbeat(ctx context.Context, ex Executor, cmd string,
	opts *Options, node, action string, interval time.Duration) (*Result, error) {
	type commandResult struct {
		result *Result
		err    error
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	started := time.Now()
	done := make(chan commandResult, 1)
	go func() {
		res, err := runSudoStreaming(ctx, ex, cmd, opts, node)
		done <- commandResult{result: res, err: err}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case result := <-done:
			return result.result, result.err
		case <-ticker.C:
			logf(opts, "  [%s] still %s (elapsed %s)", node, action, time.Since(started).Round(time.Second))
		case <-ctx.Done():
			select {
			case result := <-done:
				return result.result, result.err
			case <-time.After(3 * time.Second):
				return nil, ctx.Err()
			}
		}
	}
}

func uploadWithProgress(ex Executor, local, remote string, mode os.FileMode,
	progress func(written, total int64)) error {
	if ux, ok := ex.(progressUploader); ok {
		return ux.UploadFileProgress(local, remote, mode, progress)
	}
	if progress != nil {
		if st, err := os.Stat(local); err == nil {
			progress(0, st.Size())
		}
	}
	if err := ex.UploadFile(local, remote, mode); err != nil {
		return err
	}
	if progress != nil {
		if st, err := os.Stat(local); err == nil {
			progress(st.Size(), st.Size())
		}
	}
	return nil
}

// progressReader reports byte progress at most once a second, plus the final
// byte count. It also works with io.Copy and SFTP's File.ReadFrom.
type progressReader struct {
	r         io.Reader
	total     int64
	written   int64
	lastWrite int64
	lastAt    time.Time
	report    func(written, total int64)
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.written += int64(n)
	if r.report != nil && (time.Since(r.lastAt) >= time.Second || err == io.EOF || r.written == r.total) {
		if r.written != r.lastWrite || err == io.EOF {
			r.report(r.written, r.total)
			r.lastWrite = r.written
			r.lastAt = time.Now()
		}
	}
	return n, err
}
