package process

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestOutputBufferLineContract(t *testing.T) {
	buffer := NewOutputBuffer(2, 100)
	if lines, bytes := buffer.Append("one\r"); len(lines) != 0 || bytes != 4 {
		t.Fatalf("partial append = %#v, %d", lines, bytes)
	}
	if lines, bytes := buffer.Append("\ntwo\nthree"); fmt.Sprint(lines) != "[one two]" || bytes != 10 {
		t.Fatalf("complete append = %#v, %d", lines, bytes)
	}
	if lines, bytes := buffer.Flush(); fmt.Sprint(lines) != "[three]" || bytes != 0 {
		t.Fatalf("flush = %#v, %d", lines, bytes)
	}
	if got := fmt.Sprint(buffer.Tail(200)); got != "[two three]" {
		t.Fatalf("tail = %s", got)
	}
	if bytes, lines, truncated := buffer.Stats(); bytes != 14 || lines != 3 || truncated {
		t.Fatalf("stats = %d, %d, %t", bytes, lines, truncated)
	}
}

func TestOutputBufferCaps(t *testing.T) {
	buffer := NewOutputBuffer(200, 5)
	lines, accepted := buffer.Append("abc\ndef\n")
	if fmt.Sprint(lines) != "[abc]" || accepted != 5 {
		t.Fatalf("capped append = %#v, %d", lines, accepted)
	}
	if bytes, _, truncated := buffer.Stats(); bytes != 5 || !truncated {
		t.Fatalf("capped stats = %d, %t", bytes, truncated)
	}
	if lines, _ := buffer.Flush(); fmt.Sprint(lines) != "[d]" {
		t.Fatalf("accepted partial missing: %#v", lines)
	}

	long := NewOutputBuffer(2, 32*1024)
	payload := strings.Repeat("x", maxPendingLineBytes+100) + "\n"
	linesWithSeq, _ := long.AppendSequenced(payload)
	if len(linesWithSeq) != 1 || len(linesWithSeq[0].Text) != maxPendingLineBytes || !linesWithSeq[0].Truncated {
		t.Fatalf("long line not bounded: %#v", linesWithSeq)
	}
}

func TestOutputBufferConcurrentSequences(t *testing.T) {
	const count = 100
	buffer := NewOutputBuffer(200, 1<<20)
	var wg sync.WaitGroup
	sequences := make(chan uint64, count)
	for index := 0; index < count; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			lines, _ := buffer.AppendSequenced(fmt.Sprintf("%03d\n", index))
			if len(lines) != 1 {
				t.Errorf("append %d returned %d lines", index, len(lines))
				return
			}
			sequences <- lines[0].Sequence
		}(index)
	}
	wg.Wait()
	close(sequences)
	got := make([]int, 0, count)
	for sequence := range sequences {
		got = append(got, int(sequence))
	}
	sort.Ints(got)
	for index, sequence := range got {
		if sequence != index+1 {
			t.Fatalf("sequence[%d] = %d", index, sequence)
		}
	}
}
