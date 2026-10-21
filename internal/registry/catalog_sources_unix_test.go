//go:build unix

package registry

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadCatalogSourceRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.fifo")
	if errMkfifo := syscall.Mkfifo(path, 0600); errMkfifo != nil {
		t.Fatal(errMkfifo)
	}
	done := make(chan error, 1)
	go func() {
		_, errRead := readCatalogSource(context.Background(), path)
		done <- errRead
	}()
	select {
	case errRead := <-done:
		if errRead == nil {
			t.Fatal("accepted FIFO catalog source")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO catalog source blocked reader")
	}
}
