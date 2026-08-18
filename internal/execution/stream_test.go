package execution

import (
	"context"
	"io"
	"runtime"
	"testing"
)

func TestHostStreamRunnerStartsPipedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-specific")
	}
	process, err := (HostStreamRunner{}).Start(context.Background(), StreamCommandRequest{
		Name: "sh", Args: []string{"-c", `read line; printf %s "$line"; printf err >&2`}, StdinPipe: true,
	})
	if err != nil {
		t.Fatalf("start command: %v", err)
	}
	_, _ = process.Stdin.Write([]byte("out\n"))
	_ = process.Stdin.Close()
	stdout, _ := io.ReadAll(process.Stdout)
	stderr, _ := io.ReadAll(process.Stderr)
	_ = process.Stdout.Close()
	_ = process.Stderr.Close()
	if err := process.Cmd.Wait(); err != nil {
		t.Fatalf("wait command: %v", err)
	}
	if string(stdout) != "out" || string(stderr) != "err" {
		t.Fatalf("streams = stdout %q stderr %q", stdout, stderr)
	}
}
