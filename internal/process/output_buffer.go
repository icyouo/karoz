package process

import (
	"strings"
	"sync"
)

const maxPendingLineBytes = 8 * 1024

type OutputLine struct {
	Sequence  uint64 `json:"sequence"`
	Text      string `json:"text"`
	Truncated bool   `json:"line_truncated,omitempty"`
}

// OutputBuffer is safe for concurrent stdout/stderr collectors. Line
// acceptance and sequence assignment happen under the same lock.
type OutputBuffer struct {
	mu sync.Mutex

	tailLimit int
	byteLimit int64
	bytes     int64
	lines     int64
	truncated bool
	nextSeq   uint64

	pending          strings.Builder
	pendingTruncated bool
	discardingLine   bool
	tail             []OutputLine
}

func NewOutputBuffer(tailLimit int, byteLimit int64) *OutputBuffer {
	if tailLimit < 0 {
		tailLimit = 0
	}
	if byteLimit < 0 {
		byteLimit = 0
	}
	return &OutputBuffer{tailLimit: tailLimit, byteLimit: byteLimit}
}

func (b *OutputBuffer) Append(chunk string) (accepted []string, accountedBytes int64) {
	lines, accountedBytes := b.AppendSequenced(chunk)
	return lineTexts(lines), accountedBytes
}

func (b *OutputBuffer) AppendSequenced(chunk string) (accepted []OutputLine, accountedBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.appendLocked(chunk)
}

func (b *OutputBuffer) appendLocked(chunk string) ([]OutputLine, int64) {
	remaining := b.byteLimit - b.bytes
	if remaining <= 0 {
		if chunk != "" {
			b.truncated = true
		}
		return nil, 0
	}
	accounted := int64(len(chunk))
	if accounted > remaining {
		accounted = remaining
		b.truncated = true
	}
	acceptedChunk := chunk[:int(accounted)]
	b.bytes += accounted

	var accepted []OutputLine
	for _, part := range strings.SplitAfter(acceptedChunk, "\n") {
		hasNewline := strings.HasSuffix(part, "\n")
		if hasNewline {
			part = strings.TrimSuffix(part, "\n")
		}
		if !b.discardingLine && part != "" {
			available := maxPendingLineBytes - b.pending.Len()
			if len(part) <= available {
				b.pending.WriteString(part)
			} else {
				if available > 0 {
					b.pending.WriteString(part[:available])
				}
				b.pendingTruncated = true
				b.discardingLine = true
			}
		}
		if !hasNewline {
			continue
		}
		text := strings.TrimSuffix(b.pending.String(), "\r")
		accepted = append(accepted, b.acceptLineLocked(text, b.pendingTruncated))
		b.pending.Reset()
		b.pendingTruncated = false
		b.discardingLine = false
	}
	return accepted, accounted
}

func (b *OutputBuffer) Flush() (accepted []string, accountedBytes int64) {
	lines, accountedBytes := b.FlushSequenced()
	return lineTexts(lines), accountedBytes
}

func (b *OutputBuffer) FlushSequenced() (accepted []OutputLine, accountedBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending.Len() == 0 && !b.pendingTruncated {
		return nil, 0
	}
	text := strings.TrimSuffix(b.pending.String(), "\r")
	line := b.acceptLineLocked(text, b.pendingTruncated)
	b.pending.Reset()
	b.pendingTruncated = false
	b.discardingLine = false
	return []OutputLine{line}, 0
}

func (b *OutputBuffer) acceptLineLocked(text string, truncated bool) OutputLine {
	b.nextSeq++
	b.lines++
	line := OutputLine{Sequence: b.nextSeq, Text: text, Truncated: truncated}
	if b.tailLimit > 0 {
		if len(b.tail) == b.tailLimit {
			copy(b.tail, b.tail[1:])
			b.tail[len(b.tail)-1] = line
		} else {
			b.tail = append(b.tail, line)
		}
	}
	return line
}

func (b *OutputBuffer) Tail(limit int) []string {
	return lineTexts(b.TailSequenced(limit))
}

func (b *OutputBuffer) TailSequenced(limit int) []OutputLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 || limit > len(b.tail) {
		limit = len(b.tail)
	}
	result := make([]OutputLine, limit)
	copy(result, b.tail[len(b.tail)-limit:])
	return result
}

func (b *OutputBuffer) Stats() (bytes, lines int64, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes, b.lines, b.truncated
}

func lineTexts(lines []OutputLine) []string {
	result := make([]string, len(lines))
	for index := range lines {
		result[index] = lines[index].Text
	}
	return result
}
