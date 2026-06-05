package ada

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"time"
)

// SpawnInferenceEngine starts a llama.cpp / llama-server style binary as a
// managed subprocess (the static Go brain supervises swappable muscle — §14.2).
// The returned *exec.Cmd is owned by the caller, which should Kill it on exit.
func SpawnInferenceEngine(ctx context.Context, binaryPath, modelPath, hostPort string, ctxSize int) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, binaryPath,
		"-m", modelPath,
		"--port", portOf(hostPort),
		"--ctx-size", fmt.Sprintf("%d", ctxSize),
		"--parallel", "1",
	)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to spawn inference engine: %w", err)
	}
	// Poll readiness — don't blind-sleep. Cold load time varies with model size
	// and disk speed; a fixed sleep either races or wastes startup (§14.2).
	if err := WaitForPort(hostPort, 60*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	return cmd, nil
}

func portOf(hostPort string) string {
	if _, p, err := net.SplitHostPort(hostPort); err == nil {
		return p
	}
	return hostPort
}

// WaitForPort blocks until a TCP port accepts connections or the timeout
// elapses, polling rather than sleeping a fixed interval (§14.2).
func WaitForPort(hostPort string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", hostPort, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("port %s not ready after %s", hostPort, timeout)
}
