package process

import (
	"sort"
	"strings"
	"sync"
)

const maxPendingLineBytes = 8 * 1024

type OutputLine struct {
	Sequence  uint64 `json:"sequence"`
	Stream    string `json:"stream,omitempty"`
	Text      string `json:"text"`
	Truncated bool   `json:"line_truncated,omitempty"`
}

type partialLine struct {
	pending          strings.Builder
	pendingTruncated bool
	discardingLine   bool
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

	partials map[string]*partialLine
	tail     []OutputLine
}

func NewOutputBuffer(tailLimit int, byteLimit int64) *OutputBuffer {
	if tailLimit < 0 {
		tailLimit = 0
	}
	if byteLimit < 0 {
		byteLimit = 0
	}
	return &OutputBuffer{
		tailLimit: tailLimit, byteLimit: byteLimit,
		partials: make(map[string]*partialLine),
	}
}

func (b *OutputBuffer) Append(chunk string) (accepted []string, accountedBytes int64) {
	lines, accountedBytes := b.AppendSequenced(chunk)
	return lineTexts(lines), accountedBytes
}

func (b *OutputBuffer) AppendSequenced(chunk string) (accepted []OutputLine, accountedBytes int64) {
	return b.AppendStream("", chunk)
}

func (b *OutputBuffer) AppendStream(stream, chunk string) (accepted []OutputLine, accountedBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.appendLocked(stream, chunk)
}

func (b *OutputBuffer) appendLocked(stream, chunk string) ([]OutputLine, int64) {
	partial := b.partialLocked(stream)
	remaining := b.byteLimit - b.bytes
	if remaining <= 0 {
		if chunk != "" {
			b.truncated = true
			if partial.pending.Len() > 0 || partial.discardingLine {
				partial.pendingTruncated = true
			}
		}
		return nil, 0
	}
	accounted := int64(len(chunk))
	clipped := false
	if accounted > remaining {
		accounted = remaining
		b.truncated = true
		clipped = true
	}
	acceptedChunk := chunk[:int(accounted)]
	b.bytes += accounted

	var accepted []OutputLine
	for _, part := range strings.SplitAfter(acceptedChunk, "\n") {
		hasNewline := strings.HasSuffix(part, "\n")
		if hasNewline {
			part = strings.TrimSuffix(part, "\n")
		}
		if !partial.discardingLine && part != "" {
			available := maxPendingLineBytes - partial.pending.Len()
			if len(part) <= available {
				partial.pending.WriteString(part)
			} else {
				if available > 0 {
					partial.pending.WriteString(part[:available])
				}
				partial.pendingTruncated = true
				partial.discardingLine = true
			}
		}
		if !hasNewline {
			continue
		}
		text := strings.TrimSuffix(partial.pending.String(), "\r")
		accepted = append(accepted, b.acceptLineLocked(stream, text, partial.pendingTruncated))
		partial.pending.Reset()
		partial.pendingTruncated = false
		partial.discardingLine = false
	}
	if clipped && (partial.pending.Len() > 0 || partial.discardingLine) {
		partial.pendingTruncated = true
	}
	return accepted, accounted
}

func (b *OutputBuffer) Flush() (accepted []string, accountedBytes int64) {
	lines, accountedBytes := b.FlushSequenced()
	return lineTexts(lines), accountedBytes
}

func (b *OutputBuffer) FlushSequenced() (accepted []OutputLine, accountedBytes int64) {
	return b.FlushStream("")
}

func (b *OutputBuffer) FlushStream(stream string) (accepted []OutputLine, accountedBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.flushStreamLocked(stream), 0
}

func (b *OutputBuffer) FlushAllSequenced() []OutputLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	streams := make([]string, 0, len(b.partials))
	for stream := range b.partials {
		streams = append(streams, stream)
	}
	// Map iteration must not decide cross-stream sequence ordering at EOF.
	// Callers that need source order should flush each known stream explicitly.
	sort.Strings(streams)
	var accepted []OutputLine
	for _, stream := range streams {
		accepted = append(accepted, b.flushStreamLocked(stream)...)
	}
	return accepted
}

func (b *OutputBuffer) flushStreamLocked(stream string) []OutputLine {
	partial, ok := b.partials[stream]
	if !ok || partial.pending.Len() == 0 && !partial.pendingTruncated {
		return nil
	}
	text := strings.TrimSuffix(partial.pending.String(), "\r")
	line := b.acceptLineLocked(stream, text, partial.pendingTruncated)
	delete(b.partials, stream)
	return []OutputLine{line}
}

func (b *OutputBuffer) partialLocked(stream string) *partialLine {
	partial := b.partials[stream]
	if partial == nil {
		partial = &partialLine{}
		b.partials[stream] = partial
	}
	return partial
}

func (b *OutputBuffer) acceptLineLocked(stream, text string, truncated bool) OutputLine {
	b.nextSeq++
	b.lines++
	line := OutputLine{Sequence: b.nextSeq, Stream: stream, Text: text, Truncated: truncated}
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
