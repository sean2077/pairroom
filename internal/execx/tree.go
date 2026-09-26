package execx

import (
	"errors"
	"os"
	"os/exec"
	"sync"
)

// Tree owns a started child process together with the descendants it
// launches. On Windows the child is placed in a Job Object, so killing the
// Tree also kills the real CLI behind an npm `.cmd` shim instead of only the
// cmd.exe wrapper. Elsewhere the Tree kills the direct child, which is the
// vendor CLI itself.
type Tree struct {
	cmd *exec.Cmd

	mu       sync.Mutex
	released bool
	job      jobHandle
}

// StartTree starts cmd and binds its process tree to the returned Tree. The
// caller must call Release once cmd.Wait has returned.
//
// On Windows the child is assigned to its Job Object immediately after
// CreateProcess returns. os/exec cannot start a process suspended, so a
// descendant spawned inside that window of a few microseconds would escape
// the job; vendor shims first parse a batch file and start an interpreter,
// which takes far longer. If the assignment itself fails (for example under a
// parent job that forbids nesting), the Tree falls back to killing the direct
// child and callers still bound their wait for the tree to exit.
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	tree := &Tree{cmd: cmd}
	tree.job = attachJob(cmd.Process)
	return tree, nil
}

// Kill terminates the whole process tree. It is safe to call repeatedly and
// after the process has exited.
func (t *Tree) Kill() error {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	t.mu.Lock()
	terminated := !t.released && t.job != 0 && terminateJob(t.job) == nil
	t.mu.Unlock()
	if terminated {
		// The job owned the direct child too. Terminating an already-exiting
		// process again fails with access denied on Windows, so stop here.
		return nil
	}
	// Without a job (or if job termination failed) kill the direct child; any
	// surviving descendant is then still bounded by the caller's wait for the
	// tree to exit.
	if err := t.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// Release frees the tree's operating-system resources. Closing the Windows
// job also terminates any descendant that outlived the direct child.
func (t *Tree) Release() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		return
	}
	t.released = true
	if t.job != 0 {
		closeJob(t.job)
		t.job = 0
	}
}
