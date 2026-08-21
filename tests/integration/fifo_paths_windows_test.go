//go:build windows

package integration

import "testing"

func createFIFOAudioPaths(t *testing.T) (rxPath, txPath string, cleanup func()) {
	t.Helper()
	t.Skip("FIFO audio paths require POSIX named pipes")
	return "", "", func() {}
}
