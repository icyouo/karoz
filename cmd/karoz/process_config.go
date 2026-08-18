package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

const (
	defaultProcessMaxConcurrent   = 8
	defaultProcessLogMaxBytes     = int64(8 << 20)
	defaultProcessTailLines       = 200
	defaultProcessOutputEventMax  = int64(8 << 10)
	maxProcessOutputEventMax      = int64(64 << 10)
	defaultProcessExitDrain       = 250 * time.Millisecond
	maxProcessExitDrain           = 2 * time.Second
	defaultProcessTerminalRecords = 200
	maxProcessTerminalRecords     = 2000
	defaultProcessTerminalAge     = 7 * 24 * time.Hour
	maxProcessTerminalAge         = 30 * 24 * time.Hour
	defaultProcessLogTotalBytes   = int64(256 << 20)
	maxProcessLogTotalBytes       = int64(2 << 30)
)

type processReleaseConfig struct {
	Supervisor processSupervisorConfig
	Retention  processdomain.RetentionPolicy
}

func processReleaseConfigFromEnv() (processReleaseConfig, error) {
	maxConcurrent, err := positiveEnvInt(
		"KAROZ_PROCESS_MAX_CONCURRENT",
		defaultProcessMaxConcurrent,
		0,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	maxLifetime, err := positiveEnvDuration(
		"KAROZ_PROCESS_MAX_LIFETIME",
		0,
		0,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	defaultLifetime := defaultProcessLifetime
	if maxLifetime > 0 && maxLifetime < defaultLifetime {
		defaultLifetime = maxLifetime
	}
	logBytes, err := positiveEnvInt64(
		"KAROZ_PROCESS_LOG_MAX_BYTES",
		defaultProcessLogMaxBytes,
		0,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	tailLines, err := positiveEnvInt(
		"KAROZ_PROCESS_TAIL_LINES",
		defaultProcessTailLines,
		0,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	outputEventMax, err := positiveEnvInt64(
		"KAROZ_PROCESS_OUTPUT_EVENT_MAX_BYTES",
		defaultProcessOutputEventMax,
		maxProcessOutputEventMax,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	exitDrain, err := positiveEnvDuration(
		"KAROZ_PROCESS_EXIT_DRAIN",
		defaultProcessExitDrain,
		maxProcessExitDrain,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	terminalRecords, err := positiveEnvInt(
		"KAROZ_PROCESS_TERMINAL_MAX_RECORDS",
		defaultProcessTerminalRecords,
		maxProcessTerminalRecords,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	terminalAge, err := positiveEnvDuration(
		"KAROZ_PROCESS_TERMINAL_RETENTION",
		defaultProcessTerminalAge,
		maxProcessTerminalAge,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	logTotalBytes, err := positiveEnvInt64(
		"KAROZ_PROCESS_LOG_TOTAL_BYTES",
		defaultProcessLogTotalBytes,
		maxProcessLogTotalBytes,
	)
	if err != nil {
		return processReleaseConfig{}, err
	}
	return processReleaseConfig{
		Supervisor: processSupervisorConfig{
			LogBytes: logBytes, TailLines: tailLines, ExitDrain: exitDrain,
			DefaultLifetime: defaultLifetime, MaxLifetime: maxLifetime,
			MaxConcurrent: maxConcurrent, OutputEventBytes: outputEventMax,
		},
		Retention: processdomain.RetentionPolicy{
			MaxRecords: terminalRecords, MaxAge: terminalAge,
			MaxTotalBytes: logTotalBytes,
		},
	}, nil
}

func positiveEnvDuration(
	name string,
	fallback, ceiling time.Duration,
) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	if ceiling > 0 && value > ceiling {
		return 0, fmt.Errorf("%s exceeds the product ceiling %s", name, ceiling)
	}
	return value, nil
}

func positiveEnvInt(name string, fallback, ceiling int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	if ceiling > 0 && value > ceiling {
		return 0, fmt.Errorf("%s exceeds the product ceiling %d", name, ceiling)
	}
	return value, nil
}

func positiveEnvInt64(name string, fallback, ceiling int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	if ceiling > 0 && value > ceiling {
		return 0, fmt.Errorf("%s exceeds the product ceiling %d", name, ceiling)
	}
	return value, nil
}
