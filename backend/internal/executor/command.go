// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// orphanPipeWait is how long a stopped command waits for its output pipes to close.
var orphanPipeWait = 10 * time.Second

// Run runs cmd like cmd.Run, with one difference once ctx is done: it waits at most orphanPipeWait
// for the command's output to finish, then closes the pipes and returns. A descendant that the stop
// does not reach, such as the SSH child of git, can hold the pipes open, and an executor that never
// returns keeps its launcher from deregistering. A command that exits on its own is never cut
// short: its output is delivered whole however long its writers take, so a slow log stream cannot
// turn a successful job into a failed one. cmd must have been made with exec.CommandContext(ctx).
func Run(ctx context.Context, cmd *exec.Cmd) error {
	var pipes outputPipes
	if err := pipes.attach(cmd); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		pipes.abort()
		return err
	}
	pipes.start()
	err := cmd.Wait()
	if copyErr := pipes.wait(ctx); err == nil {
		err = copyErr
	}
	return err
}

// Output runs cmd like cmd.Output, with the bounded stop of Run.
func Output(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	captureStderr := cmd.Stderr == nil
	if captureStderr {
		cmd.Stderr = &stderr
	}
	err := Run(ctx, cmd)
	var exitErr *exec.ExitError
	if captureStderr && errors.As(err, &exitErr) {
		exitErr.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), err
}

// CombinedOutput runs cmd like cmd.CombinedOutput, with the bounded stop of Run.
func CombinedOutput(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, errors.New("exec: Stdout or Stderr already set")
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := Run(ctx, cmd)
	return out.Bytes(), err
}

// outputPipes gives each output writer of a command that is not a file a pipe of the executor's
// own, so the executor, not exec.Cmd, decides how long to wait for it.
type outputPipes struct {
	copies  []pipeCopy
	writers []*os.File // the child's ends, closed in the parent once the child has them
	errs    []error
	wg      sync.WaitGroup
}

type pipeCopy struct {
	r *os.File
	w io.Writer
}

func (p *outputPipes) attach(cmd *exec.Cmd) error {
	shared := cmd.Stdout != nil && cmd.Stdout == cmd.Stderr
	if err := p.replace(&cmd.Stdout); err != nil {
		return err
	}
	if shared {
		cmd.Stderr = cmd.Stdout
		return nil
	}
	if err := p.replace(&cmd.Stderr); err != nil {
		p.abort()
		return err
	}
	return nil
}

// replace leaves *w as it is when it is nil or a file, which the child writes directly, and
// otherwise puts in its place the write end of a new pipe whose copy goes into the original writer.
func (p *outputPipes) replace(w *io.Writer) error {
	if *w == nil {
		return nil
	}
	if _, ok := (*w).(*os.File); ok {
		return nil
	}
	r, wf, err := os.Pipe()
	if err != nil {
		return err
	}
	p.copies = append(p.copies, pipeCopy{r: r, w: *w})
	p.writers = append(p.writers, wf)
	*w = wf
	return nil
}

func (p *outputPipes) start() {
	for _, f := range p.writers {
		_ = f.Close()
	}
	p.errs = make([]error, len(p.copies))
	for i, c := range p.copies {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			_, p.errs[i] = io.Copy(c.w, c.r)
		}()
	}
}

// wait waits for every copy to reach the end of its pipe. Only once ctx is done does it bound that
// wait by orphanPipeWait, after which it closes the pipes and reports exec.ErrWaitDelay.
func (p *outputPipes) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
		case <-time.After(orphanPipeWait):
			for _, c := range p.copies {
				_ = c.r.Close()
			}
			<-done
			return exec.ErrWaitDelay
		}
	}
	for i, c := range p.copies {
		_ = c.r.Close()
		if p.errs[i] != nil {
			return p.errs[i]
		}
	}
	return nil
}

// abort closes both ends of every pipe when the command never started.
func (p *outputPipes) abort() {
	for _, f := range p.writers {
		_ = f.Close()
	}
	for _, c := range p.copies {
		_ = c.r.Close()
	}
}
