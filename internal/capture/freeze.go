package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Freezing is done at the operating-system level: Chromium and every process
// it spawned (renderers, GPU, network) are stopped with SIGSTOP and resumed
// with SIGCONT. Stopped processes use no CPU at all, and the page can't tell
// it happened, which Chromium's own "frozen" page state couldn't promise:
// in headless mode a page frozen that way didn't repaint after waking.

// processTree is pid and all its descendants, parents before children.
func processTree(pid int) []int {
	children := map[int][]int{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		id, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ..." where comm may hold spaces: split after the last ")".
		rest := string(stat)
		if i := strings.LastIndex(rest, ")"); i >= 0 {
			rest = rest[i+1:]
		}
		f := strings.Fields(rest)
		if len(f) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(f[1]); err == nil {
			children[ppid] = append(children[ppid], id)
		}
	}
	out := []int{pid}
	for i := 0; i < len(out); i++ {
		out = append(out, children[out[i]]...)
	}
	return out
}

// stopTree pauses a process and its descendants; contTree resumes them.
// Parents are stopped first so none spawns a child that's missed; resuming
// goes children first for the same reason in reverse.
func stopTree(pid int) error {
	for _, p := range processTree(pid) {
		if err := syscall.Kill(p, syscall.SIGSTOP); err != nil && p == pid {
			return fmt.Errorf("stop %d: %w", p, err)
		}
	}
	return nil
}

func contTree(pid int) error {
	tree := processTree(pid)
	for i := len(tree) - 1; i >= 0; i-- {
		if err := syscall.Kill(tree[i], syscall.SIGCONT); err != nil && tree[i] == pid {
			return fmt.Errorf("resume %d: %w", tree[i], err)
		}
	}
	return nil
}
