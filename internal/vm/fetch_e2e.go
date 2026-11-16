//go:build e2e

package vm

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

func init() {
	afterFetchDownload = func(ctx context.Context, path string) error {
		return waitFetchBoundary(ctx, os.Getenv("HK_E2E_FETCH_AFTER_DOWNLOAD"), path)
	}
	fetchExportWriter = func(ctx context.Context, file *os.File) io.Writer {
		marker := os.Getenv("HK_E2E_FETCH_DURING_DOWNLOAD")
		if marker == "" {
			return file
		}
		return &fetchBoundaryWriter{ctx: ctx, file: file, marker: marker}
	}
}

type fetchBoundaryWriter struct {
	ctx     context.Context
	file    *os.File
	marker  string
	written int
	reached bool
}

func (writer *fetchBoundaryWriter) Write(data []byte) (int, error) {
	n, err := writer.file.Write(data)
	writer.written += n
	if err == nil && !writer.reached && writer.written >= 64<<10 {
		writer.reached = true
		err = waitFetchBoundary(writer.ctx, writer.marker, writer.file.Name())
	}
	return n, err
}

func waitFetchBoundary(ctx context.Context, marker, path string) error {
	if marker == "" {
		return nil
	}
	if err := os.WriteFile(marker, []byte(path), 0600); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(marker + ".continue"); err == nil {
			return ctx.Err()
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
