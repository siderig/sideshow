package main

import (
	"strings"
	"sync"
)

// logRing is a fixed-size in-memory ring of recent log lines (agent + child
// output), exposed via GET /api/logs so a node can be debugged without SSH. It
// implements io.Writer so it can sit in the agent's log MultiWriter; child
// output is fed in line-by-line by prefixWriter.
type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

var logs = &logRing{max: 800}

func (l *logRing) add(line string) {
	if line == "" {
		return
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > l.max {
		l.lines = l.lines[len(l.lines)-l.max:]
	}
	l.mu.Unlock()
}

// Write lets the ring be a log sink; splits on newlines into lines.
func (l *logRing) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		l.add(line)
	}
	return len(p), nil
}

// tail returns the last n lines (all if n<=0 or n exceeds the ring).
func (l *logRing) tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.lines) {
		n = len(l.lines)
	}
	out := make([]string, n)
	copy(out, l.lines[len(l.lines)-n:])
	return out
}
