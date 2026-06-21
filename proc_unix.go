//go:build linux || darwin

package main

import (
	"os/exec"
	"syscall"
)

// setProcGroup makes the child the leader of a new process group so the whole
// group (the Go subprocess plus its Chromium children) can be signalled at
// once. Without this, killing the stored PID would orphan Chromium — the leak
// this reaper exists to prevent.
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// processGroupID returns the process group id for a started pid, falling back
// to the pid itself (a new group leader's pgid equals its pid).
func processGroupID(pid int) int {
	if pgid, err := syscall.Getpgid(pid); err == nil {
		return pgid
	}
	return pid
}

// killProcessGroup SIGKILLs an entire process group via the negative-pid
// signal convention.
func killProcessGroup(pgid int) error {
	if pgid <= 1 {
		return nil
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// processAlive reports whether a pid exists (signal 0 probes without killing).
func processAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
