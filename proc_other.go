//go:build !linux && !darwin

package main

import "os/exec"

// On unsupported platforms (e.g. Windows) the reaper is inert: process groups
// are not set, nothing is killed, and validation never matches so the sweep
// only cleans stale registry rows.

func setProcGroup(*exec.Cmd) {}

func processGroupID(pid int) int { return pid }

func killProcessGroup(int) error { return nil }

func validateProcessMatch(int, string) bool { return false }
