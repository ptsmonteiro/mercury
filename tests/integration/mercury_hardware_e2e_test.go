//go:build hardware

package integration

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type hardwareEndpoint struct {
	name       string
	call       string
	port       int
	cmd        *exec.Cmd
	proc       *processWait
	stdoutPath string
	stderrPath string
	control    net.Conn
	rw         *bufio.ReadWriter
	data       net.Conn
}

// TestMercuryHardwareE2E performs a real, bidirectional, over-the-air ARQ
// exchange. Running this test keys both configured radios. Each endpoint gets
// its audio and PTT settings from a separate INI file while the harness supplies
// collision-free local TCP ports.
func TestMercuryHardwareE2E(t *testing.T) {
	repoRoot := mustRepoRoot(t)
	station1Config := hardwareConfig(t, repoRoot, "MERCURY_HW_STATION1_CONFIG", "station1.ini")
	station2Config := hardwareConfig(t, repoRoot, "MERCURY_HW_STATION2_CONFIG", "station2.ini")
	bin := locateOrBuildMercury(t, repoRoot)

	station1Port := freePortPair(t)
	station2Port := freePortPair(t)
	station1BroadcastPort := station1Port + 100
	station2BroadcastPort := station2Port + 100
	t.Logf("ports: station1 control/data/broadcast=%d/%d/%d; station2=%d/%d/%d",
		station1Port, station1Port+1, station1BroadcastPort,
		station2Port, station2Port+1, station2BroadcastPort)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := func(name, call, config string, port, broadcastPort int) *hardwareEndpoint {
		stdout, stderr := tempLogFilesNamed(t, name)
		cmd := exec.CommandContext(ctx, bin,
			"-C", config,
			"-p", fmt.Sprint(port),
			"-b", fmt.Sprint(broadcastPort),
			"-v",
		)
		cmd.Dir = repoRoot
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s Mercury: %v", name, err)
		}
		e := &hardwareEndpoint{name: name, call: call, port: port, cmd: cmd,
			proc: waitForProcess(cmd), stdoutPath: stdout.Name(), stderrPath: stderr.Name()}
		t.Cleanup(func() {
			if e.control != nil {
				_ = e.control.Close()
			}
			if e.data != nil {
				_ = e.data.Close()
			}
			_ = stopProcess(t, e.cmd, e.proc, e.stdoutPath, e.stderrPath)
		})
		return e
	}

	station1 := start("station1", envOr("MERCURY_HW_STATION1_CALL", "STATION1"), station1Config, station1Port, station1BroadcastPort)
	station2 := start("station2", envOr("MERCURY_HW_STATION2_CALL", "STATION2"), station2Config, station2Port, station2BroadcastPort)

	fail := func(format string, args ...interface{}) {
		printLogs(t, station1.stdoutPath, station1.stderrPath)
		printLogs(t, station2.stdoutPath, station2.stderrPath)
		t.Fatalf(format, args...)
	}

	// On Windows, dialing station1 while station2 is still starting can assign
	// station2's not-yet-bound data port as the client's ephemeral source port.
	// Wait for both engines to bind all three listeners before opening any
	// harness connection.
	for _, e := range []*hardwareEndpoint{station1, station2} {
		if err := waitForLogText(ctx, e.stderrPath, "Mercury engine initialised", 15*time.Second, e.proc); err != nil {
			fail("%s startup: %v", e.name, err)
		}
	}

	for _, e := range []*hardwareEndpoint{station1, station2} {
		conn, err := waitForTCP(ctx, "127.0.0.1", e.port, 15*time.Second, e.proc)
		if err != nil {
			fail("%s control port: %v", e.name, err)
		}
		e.control = conn
		e.rw = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		if got := sendHardwareCommand(t, conn, e.rw, "MYCALL "+e.call, "OK"); !strings.HasPrefix(got, "OK") {
			fail("%s MYCALL response %q", e.name, got)
		}
		if !waitControlPrefix(t, conn, e.rw, "REGISTERED "+e.call, 5*time.Second) {
			fail("%s did not register call %s", e.name, e.call)
		}
		if got := sendHardwareCommand(t, conn, e.rw, "LISTEN ON", "OK"); !strings.HasPrefix(got, "OK") {
			fail("%s LISTEN response %q", e.name, got)
		}
		data, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", e.port+1), 5*time.Second)
		if err != nil {
			fail("%s data port: %v", e.name, err)
		}
		e.data = data
	}

	t.Log("RADIO TRANSMISSION STARTING: station1 calling station2")
	if got := sendHardwareCommand(t, station1.control, station1.rw,
		"CONNECT "+station1.call+" "+station2.call, "OK"); !strings.HasPrefix(got, "OK") {
		fail("CONNECT response %q", got)
	}
	if !waitControlPrefix(t, station1.control, station1.rw, "CONNECTED", 3*time.Minute) {
		fail("station1 did not connect within 3 minutes")
	}

	// Sequential transfers exercise both transmitters and both receivers while
	// keeping airtime and the test's RF footprint small.
	aToB := []byte("MERCURY-HARDWARE-E2E STATION1 TO STATION2 0123456789")
	bToA := []byte("MERCURY-HARDWARE-E2E STATION2 TO STATION1 9876543210")
	transferHardwarePayload(t, fail, station1, station2, aToB)
	transferHardwarePayload(t, fail, station2, station1, bToA)

	if got := sendHardwareCommand(t, station1.control, station1.rw, "DISCONNECT", "OK"); !strings.HasPrefix(got, "OK") {
		fail("DISCONNECT response %q", got)
	}
	t.Logf("hardware E2E passed: %d bytes station1->station2 and %d bytes station2->station1", len(aToB), len(bToA))
}

func hardwareConfig(t *testing.T, repoRoot, envName, defaultName string) string {
	t.Helper()
	path := os.Getenv(envName)
	if path == "" {
		path = filepath.Join(repoRoot, "tests", "integration", defaultName)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("%s: %v", envName, err)
	}
	if info, err := os.Stat(abs); err != nil || info.IsDir() {
		t.Fatalf("%s config is not a readable file: %s", envName, abs)
	}
	return abs
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func waitForLogText(ctx context.Context, path, wanted string, timeout time.Duration, proc *processWait) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-proc.done:
			if proc.err != nil {
				return proc.err
			}
			return fmt.Errorf("process exited before %q", wanted)
		default:
		}

		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), wanted) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %q in %s", wanted, filepath.Base(path))
}

func waitControlPrefix(t *testing.T, conn net.Conn, rw *bufio.ReadWriter, prefix string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	_ = conn.SetReadDeadline(deadline)
	defer conn.SetReadDeadline(time.Time{})
	for time.Now().Before(deadline) {
		line, err := rw.ReadString('\r')
		if err != nil {
			return false
		}
		line = strings.TrimSpace(line)
		if line != "" {
			t.Logf("control: %s", line)
		}
		if strings.HasPrefix(line, prefix) {
			return true
		}
		if strings.HasPrefix(line, "DISCONNECTED") {
			return false
		}
	}
	return false
}

// sendHardwareCommand ignores asynchronous BUFFER/IAMALIVE notifications that
// can accumulate during a multi-minute OTA exchange and returns the requested
// command response.
func sendHardwareCommand(t *testing.T, conn net.Conn, rw *bufio.ReadWriter, command, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	_ = conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})
	if _, err := rw.WriteString(command + "\r"); err != nil {
		t.Fatalf("write %q: %v", command, err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatalf("flush %q: %v", command, err)
	}
	for time.Now().Before(deadline) {
		line, err := rw.ReadString('\r')
		if err != nil {
			t.Fatalf("read response for %q: %v", command, err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) || strings.HasPrefix(line, "FAULT") {
			return line
		}
		t.Logf("control while awaiting %q: %s", command, line)
	}
	return ""
}

func transferHardwarePayload(t *testing.T, fail func(string, ...interface{}), from, to *hardwareEndpoint, payload []byte) {
	t.Helper()
	if _, err := from.data.Write(payload); err != nil {
		fail("%s payload write: %v", from.name, err)
	}
	received := make([]byte, 0, len(payload))
	buf := make([]byte, 512)
	deadline := time.Now().Add(4 * time.Minute)
	for len(received) < len(payload) && time.Now().Before(deadline) {
		_ = to.data.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, err := to.data.Read(buf)
		if n > 0 {
			received = append(received, buf[:n]...)
			t.Logf("%s -> %s: %d/%d bytes", from.name, to.name, len(received), len(payload))
		}
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			fail("%s payload read: %v", to.name, err)
		}
	}
	_ = to.data.SetReadDeadline(time.Time{})
	if string(received) != string(payload) {
		fail("%s -> %s payload mismatch: got %d bytes, want %d", from.name, to.name, len(received), len(payload))
	}
}
