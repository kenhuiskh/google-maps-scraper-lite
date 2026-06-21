//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// validateProcessMatch guards against PID reuse: before killing a recorded
// group it confirms the live pid is still our scraper exe or a Chromium child
// by inspecting /proc/<pid>/cmdline. A recycled, unrelated pid returns false so
// the caller cleans the stale row instead of killing an innocent process.
func validateProcessMatch(pid int, exe string) bool {
	if pid <= 1 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	// cmdline args are NUL-separated.
	cmdline := strings.ToLower(string(bytes.ReplaceAll(data, []byte{0}, []byte{' '})))
	if exe != "" && strings.Contains(cmdline, strings.ToLower(exe)) {
		return true
	}
	return strings.Contains(cmdline, "chrome") || strings.Contains(cmdline, "chromium")
}
