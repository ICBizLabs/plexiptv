// Package logbuf keeps the most recent log lines in memory for the web panel.
package logbuf

import (
	"io"
	"strings"
	"sync"
)

type Buffer struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

func New(size int) *Buffer { return &Buffer{lines: make([]string, size)} }

// Write implements io.Writer; each call is expected to be one log line.
func (b *Buffer) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	b.mu.Lock()
	b.lines[b.next] = line
	b.next = (b.next + 1) % len(b.lines)
	if b.next == 0 {
		b.full = true
	}
	b.mu.Unlock()
	return len(p), nil
}

// Lines returns the buffered lines, oldest first.
func (b *Buffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.full {
		return append([]string(nil), b.lines[:b.next]...)
	}
	return append(append([]string(nil), b.lines[b.next:]...), b.lines[:b.next]...)
}

// Tee writes every line to both w and the buffer.
func (b *Buffer) Tee(w io.Writer) io.Writer { return io.MultiWriter(b, w) } // buffer first: w may be a dead console under a service
