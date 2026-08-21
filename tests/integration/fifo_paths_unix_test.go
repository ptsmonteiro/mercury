//go:build !windows

package integration

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func createFIFOAudioPaths(t *testing.T) (rxPath, txPath string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	rxPath = filepath.Join(dir, "rx.s32le.fifo")
	txPath = filepath.Join(dir, "tx.s32le.fifo")
	for _, path := range []string{rxPath, txPath} {
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatalf("mkfifo %s: %v", path, err)
		}
	}

	var keepers []*os.File
	for _, path := range []string{rxPath, txPath} {
		fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0600)
		if err != nil {
			for _, f := range keepers {
				_ = f.Close()
			}
			t.Fatalf("open FIFO keeper %s: %v", path, err)
		}
		keepers = append(keepers, os.NewFile(uintptr(fd), path))
	}

	return rxPath, txPath, func() {
		for _, f := range keepers {
			_ = f.Close()
		}
	}
}
