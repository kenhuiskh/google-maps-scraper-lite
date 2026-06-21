//go:build darwin

package main

// validateProcessMatch on macOS (dev only; production target is the Docker
// Linux image) has no /proc to inspect, so it degrades to a liveness probe.
// If the pid is alive the group is killed; weaker PID-reuse protection is
// acceptable off the production path.
func validateProcessMatch(pid int, _ string) bool {
	return processAlive(pid)
}
