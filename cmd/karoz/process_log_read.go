package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	processdomain "github.com/karoz/karoz/internal/process"
)

const (
	maxProcessLogWindowLines   = 200
	maxProcessLogResponseBytes = 64 << 10
	maxProcessLogScanBytes     = 8 << 20
	maxProcessLogLineBytes     = 8 << 10
)

var sensitiveProcessValuePattern = regexp.MustCompile(
	`(?i)(\b(?:[A-Z0-9_]*(?:API_KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|AUTHORIZATION)[A-Z0-9_]*|AUTH)\s*[:=]\s*)([^\s,;]+)`,
)

var errProcessLogGone = errors.New("process log is gone")

type processLogWindow struct {
	ProcessID string   `json:"process_id"`
	Offset    int      `json:"offset"`
	Next      int      `json:"next_offset"`
	Lines     []string `json:"lines"`
	Tail      bool     `json:"tail"`
	EOF       bool     `json:"eof"`
	Truncated bool     `json:"truncated,omitempty"`
}

type trackedProcessLogReader struct {
	runtime   *processRuntimePersistence
	projectID string
	processID string
	reader    runtimeReadSeekCloser
	once      sync.Once
}

func (reader *trackedProcessLogReader) Read(value []byte) (int, error) {
	return reader.reader.Read(value)
}

func (reader *trackedProcessLogReader) Seek(
	offset int64,
	whence int,
) (int64, error) {
	return reader.reader.Seek(offset, whence)
}

func (reader *trackedProcessLogReader) Close() error {
	var err error
	reader.once.Do(func() {
		err = reader.reader.Close()
		reader.runtime.releaseProcessLogReader(reader.projectID, reader.processID)
	})
	return err
}

func (runtime *processRuntimePersistence) OpenLogReader(
	projectID, processID string,
) (runtimeReadSeekCloser, processdomain.Process, error) {
	project := runtime.projectRuntime(projectID)
	if project == nil {
		return nil, processdomain.Process{}, errors.New("process project runtime is unavailable")
	}
	project.lane.Lock()
	runtime.authorityMu.Lock()
	partition := runtime.authority.Projects[project.identity.SafeProjectKey]
	durable, exists := partition.Records[processID]
	if !exists {
		_, gone := partition.Tombstones[processID]
		runtime.authorityMu.Unlock()
		project.lane.Unlock()
		if gone {
			return nil, processdomain.Process{}, errProcessLogGone
		}
		return nil, processdomain.Process{}, errors.New("process not found")
	}
	record := cloneDurableProcess(durable.Process)
	runtime.authorityMu.Unlock()
	runtime.readerMu.Lock()
	byProcess := runtime.openReaders[projectID]
	if byProcess == nil {
		byProcess = make(map[string]int)
		runtime.openReaders[projectID] = byProcess
	}
	byProcess[processID]++
	runtime.readerMu.Unlock()
	project.lane.Unlock()

	file, err := runtime.store.openRead(record.LogPath)
	if err != nil {
		runtime.releaseProcessLogReader(projectID, processID)
		if errors.Is(err, os.ErrNotExist) {
			return nil, processdomain.Process{}, errProcessLogGone
		}
		return nil, processdomain.Process{}, err
	}
	return &trackedProcessLogReader{
		runtime: runtime, projectID: projectID, processID: processID, reader: file,
	}, record, nil
}

func (runtime *processRuntimePersistence) releaseProcessLogReader(
	projectID, processID string,
) {
	runtime.readerMu.Lock()
	defer runtime.readerMu.Unlock()
	byProcess := runtime.openReaders[projectID]
	if byProcess == nil {
		return
	}
	if byProcess[processID] <= 1 {
		delete(byProcess, processID)
	} else {
		byProcess[processID]--
	}
	if len(byProcess) == 0 {
		delete(runtime.openReaders, projectID)
	}
}

func readProcessLogWindow(
	reader runtimeReadSeekCloser,
	processID string,
	totalLines int64,
	offset, limit int,
	tail bool,
) (processLogWindow, error) {
	if limit < 1 || limit > maxProcessLogWindowLines {
		return processLogWindow{}, errors.New("process log limit is out of range")
	}
	if offset < 0 {
		return processLogWindow{}, errors.New("process log offset must not be negative")
	}
	if tail {
		return readProcessLogTail(reader, processID, totalLines, limit)
	}
	return readProcessLogOffset(reader, processID, offset, limit)
}

func readProcessLogTail(
	reader runtimeReadSeekCloser,
	processID string,
	totalLines int64,
	limit int,
) (processLogWindow, error) {
	end, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return processLogWindow{}, err
	}
	start := end - maxProcessLogResponseBytes
	if start < 0 {
		start = 0
	}
	if _, err := reader.Seek(start, io.SeekStart); err != nil {
		return processLogWindow{}, err
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxProcessLogResponseBytes))
	if err != nil {
		return processLogWindow{}, err
	}
	if start > 0 {
		if newline := strings.IndexByte(string(body), '\n'); newline >= 0 {
			body = body[newline+1:]
		} else {
			body = nil
		}
	}
	rawLines := splitProcessLogLines(string(body))
	truncated := start > 0
	if len(rawLines) > limit {
		rawLines = rawLines[len(rawLines)-limit:]
	}
	lines, clipped := redactAndBoundProcessLogLines(rawLines)
	truncated = truncated || clipped
	startOffset := int(totalLines) - len(lines)
	if startOffset < 0 {
		startOffset = 0
	}
	return processLogWindow{
		ProcessID: processID, Offset: startOffset,
		Next: startOffset + len(lines), Lines: lines,
		Tail: true, EOF: true, Truncated: truncated,
	}, nil
}

func readProcessLogOffset(
	reader runtimeReadSeekCloser,
	processID string,
	offset, limit int,
) (processLogWindow, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return processLogWindow{}, err
	}
	buffered := bufio.NewReaderSize(reader, 32<<10)
	lineNumber, scannedBytes, responseBytes := 0, 0, 0
	lines := make([]string, 0, limit)
	eof, truncated := false, false
	for len(lines) < limit {
		line, err := buffered.ReadString('\n')
		scannedBytes += len(line)
		if scannedBytes > maxProcessLogScanBytes {
			if lineNumber < offset {
				return processLogWindow{}, errors.New("process log offset exceeds bounded scan window")
			}
			truncated = true
			break
		}
		if line != "" {
			if lineNumber >= offset {
				value := redactSensitiveProcessText(
					strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"),
				)
				value = limitString(value, maxProcessLogLineBytes)
				encodedBytes := processLogEncodedSize(value)
				if responseBytes+encodedBytes > maxProcessLogResponseBytes {
					truncated = true
					break
				}
				lines = append(lines, value)
				responseBytes += encodedBytes
			}
			lineNumber++
		}
		if errors.Is(err, io.EOF) {
			eof = true
			break
		}
		if err != nil {
			return processLogWindow{}, err
		}
	}
	return processLogWindow{
		ProcessID: processID, Offset: offset, Next: offset + len(lines),
		Lines: lines, EOF: eof, Truncated: truncated,
	}, nil
}

func splitProcessLogLines(value string) []string {
	if value == "" {
		return []string{}
	}
	lines := strings.Split(value, "\n")
	if strings.HasSuffix(value, "\n") {
		lines = lines[:len(lines)-1]
	}
	for index := range lines {
		lines[index] = strings.TrimSuffix(lines[index], "\r")
	}
	return lines
}

func redactAndBoundProcessLogLines(values []string) ([]string, bool) {
	lines := make([]string, 0, len(values))
	bytes := 0
	truncated := false
	for _, value := range values {
		value = limitString(
			redactSensitiveProcessText(value),
			maxProcessLogLineBytes,
		)
		encodedBytes := processLogEncodedSize(value)
		if bytes+encodedBytes > maxProcessLogResponseBytes {
			truncated = true
			continue
		}
		lines = append(lines, value)
		bytes += encodedBytes
	}
	return lines, truncated
}

func processLogEncodedSize(value string) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return len(value) + 2
	}
	return len(encoded)
}

func redactSensitiveProcessText(value string) string {
	value = strings.ToValidUTF8(value, "�")
	return sensitiveProcessValuePattern.ReplaceAllString(value, "${1}[REDACTED]")
}
