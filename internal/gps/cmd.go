// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gps

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Masterminds/vcs"
)

// monitoredCmd wraps a cmd and will keep monitoring the process until it
// finishes, the provided context is canceled, or a certain amount of time has
// passed and the command showed no signs of activity.
type monitoredCmd struct {
	cmd     *exec.Cmd
	timeout time.Duration
	stdout  *activityBuffer
	stderr  *activityBuffer
}

// noProgressError indicates that the monitored process was terminated due to
// exceeding exceeding the progress timeout.
type noProgressError struct {
	timeout time.Duration
}

// killCmdError indicates that an error occurred while sending a kill signal to
// the monitored process.
type killCmdError struct {
	err error
}

func newMonitoredCmd(cmd *exec.Cmd, timeout time.Duration) *monitoredCmd {
	stdout, stderr := newActivityBuffer(), newActivityBuffer()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return &monitoredCmd{
		cmd:     cmd,
		timeout: timeout,
		stdout:  stdout,
		stderr:  stderr,
	}
}

// run will wait for the command to finish and return the error, if any. If the
// command does not show any progress, as indicated by writing to stdout or
// stderr, for more than the specified timeout, the process will be killed.
func (c *monitoredCmd) run(ctx context.Context) error {
	// Check for cancellation before even starting
	if ctx.Err() != nil {
		return ctx.Err()
	}

	err := c.cmd.Start()
	if err != nil {
		return err
	}

	// With ticker-based timeout control, the maximum possible running time
	// without progress is equal to timeout + ticker cycle - 1ns. As such, we
	// want a shorter ticker cycle time than the timeout; setting them equally
	// would result in a ceiling of nearly 2x the requested timeout.
	//
	// This is difficult to know for the general case, but we know that the
	// processes gps is launching will often run for either the multiple-minutes
	// range, the couple-seconds range, or the 10-200ms range. Given these
	// buckets, we can make some sane approximations for ticker values - we
	// start with a default of ten checks per timeout duration. For large
	// values, we can reduce this further to checking once per second, as that
	// will never be terribly costly. For smaller values, we can define a floor
	// of five milliseconds, or equal to the timeout duration, whichever is
	// less.
	tickDuration := c.timeout / 10
	switch {
	case tickDuration > time.Second:
		tickDuration = time.Second
	case tickDuration < 5*time.Millisecond:
		if c.timeout < 5*time.Millisecond {
			tickDuration = c.timeout
		} else {
			tickDuration = 5 * time.Millisecond
		}
	}

	ticker := time.NewTicker(tickDuration)
	defer ticker.Stop()

	// Atomic marker to track proc exit state. Guards against bad channel
	// select receive order, where a tick or context cancellation could come
	// in at the same time as process completion, but one of the former are
	// picked first; in such a case, cmd.Process could(?) be nil by the time we
	// call signal methods on it.
	var isDone *int32 = new(int32)
	done := make(chan error, 1)

	go func() {
		// Wait() can only be called once, so this must act as the completion
		// indicator for both normal *and* signal-induced termination.
		done <- c.cmd.Wait()
		atomic.CompareAndSwapInt32(isDone, 0, 1)
	}()

	var killerr error
selloop:
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			if !atomic.CompareAndSwapInt32(isDone, 1, 1) && c.hasTimedOut() {
				if err := killProcess(c.cmd, isDone); err != nil {
					killerr = &killCmdError{err}
				} else {
					killerr = &noProgressError{c.timeout}
				}
				break selloop
			}
		case <-ctx.Done():
			if !atomic.CompareAndSwapInt32(isDone, 1, 1) {
				if err := killProcess(c.cmd, isDone); err != nil {
					killerr = &killCmdError{err}
				} else {
					killerr = ctx.Err()
				}
				break selloop
			}
		}
	}

	// This is only reachable on the signal-induced termination path, so block
	// until a message comes through the channel indicating that the command has
	// exited.
	//
	// TODO(sdboyer) if the signaling process errored (resulting in a
	// killCmdError stored in killerr), is it possible that this receive could
	// block forever on some kind of hung process?
	<-done
	return killerr
}

func (c *monitoredCmd) hasTimedOut() bool {
	t := time.Now().Add(-c.timeout)
	return c.stderr.lastActivity().Before(t) &&
		c.stdout.lastActivity().Before(t)
}

func (c *monitoredCmd) combinedOutput(ctx context.Context) ([]byte, error) {
	if err := c.run(ctx); err != nil {
		return c.stderr.buf.Bytes(), err
	}

	// FIXME(sdboyer) this is not actually combined output
	return c.stdout.buf.Bytes(), nil
}

// activityBuffer is a buffer that keeps track of the last time a Write
// operation was performed on it.
type activityBuffer struct {
	sync.Mutex
	buf               *bytes.Buffer
	lastActivityStamp time.Time
}

func newActivityBuffer() *activityBuffer {
	return &activityBuffer{
		buf: bytes.NewBuffer(nil),
	}
}

func (b *activityBuffer) Write(p []byte) (int, error) {
	b.Lock()
	b.lastActivityStamp = time.Now()
	defer b.Unlock()
	return b.buf.Write(p)
}

func (b *activityBuffer) lastActivity() time.Time {
	b.Lock()
	defer b.Unlock()
	return b.lastActivityStamp
}

func (e noProgressError) Error() string {
	return fmt.Sprintf("command killed after %s of no activity", e.timeout)
}

func (e killCmdError) Error() string {
	return fmt.Sprintf("error killing command: %s", e.err)
}

func runFromCwd(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	c := newMonitoredCmd(exec.Command(cmd, args...), 2*time.Minute)
	return c.combinedOutput(ctx)
}

func runFromRepoDir(ctx context.Context, repo vcs.Repo, cmd string, args ...string) ([]byte, error) {
	c := newMonitoredCmd(repo.CmdFromDir(cmd, args...), 2*time.Minute)
	return c.combinedOutput(ctx)
}
