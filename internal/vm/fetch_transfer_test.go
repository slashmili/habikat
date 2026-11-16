package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadBinaryExport(t *testing.T) {
	dir := privateTempDir(t)
	payload := append(bytes.Repeat([]byte{0, 255, 10, 13, 32, 'x'}, 18000), []byte("end")...)
	writeTestFile(t, filepath.Join(dir, "payload"), payload)
	installFetchSSH(t, "printf 'harmless diagnostic' >&2\ncat payload")
	var out bytes.Buffer
	if err := downloadExport(t.Context(), dir, "fixture command", &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("binary export changed: %d vs %d bytes", out.Len(), len(payload))
	}
}

func TestDownloadExportFailures(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		cancelled    bool
	}{
		{name: "remote failed", script: "printf partial; exit 27"},
		{name: "diagnostic overflow", script: "head -c 65537 /dev/zero >&2"},
		{name: "cancelled", script: "cat /dev/zero", cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := privateTempDir(t)
			installFetchSSH(t, tc.script)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var out io.Writer = &bytes.Buffer{}
			if tc.cancelled {
				out = cancelBundleWriter{cancel: cancel}
			}
			err := downloadExport(ctx, dir, "fixture", out)
			if err == nil {
				t.Fatal("failed transfer succeeded")
			}
			if !tc.cancelled && errors.Is(err, context.Canceled) {
				t.Fatalf("transfer failure misreported as user cancellation: %.200s", err)
			}
			if tc.cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}

type cancelBundleWriter struct{ cancel context.CancelFunc }

func (writer cancelBundleWriter) Write(data []byte) (int, error) {
	writer.cancel()
	return 0, context.Canceled
}

func installFetchSSH(t *testing.T, script string) {
	t.Helper()
	bin := privateTempDir(t)
	path := filepath.Join(bin, "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

func TestBoundedBundleWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out bytes.Buffer
	writer := &boundedBundleWriter{writer: &out, remaining: 4, cancel: cancel}
	if n, err := writer.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatalf("boundary write=%d %v", n, err)
	}
	if n, err := writer.Write([]byte("5")); n != 0 || !errors.Is(err, errFetchBundleLimit) {
		t.Fatalf("oversized write=%d %v", n, err)
	}
	if out.String() != "1234" || ctx.Err() != context.Canceled {
		t.Fatal("oversized transfer was not bounded and cancelled")
	}
}

func TestFetchTransferFailureClassification(t *testing.T) {
	outputErr := errors.New("disk write failed")
	for _, tc := range []struct {
		name     string
		output   error
		overflow bool
		caller   error
		want     error
	}{
		{name: "size", output: errFetchBundleLimit, want: errFetchBundleLimit},
		{name: "diagnostic", overflow: true, want: errGuestOutputLimit},
		{name: "output", output: outputErr, want: outputErr},
		{name: "caller cancelled", output: outputErr, caller: context.Canceled, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fetchTransferFailure(context.Canceled, tc.output, tc.overflow, tc.caller)
			if !errors.Is(err, tc.want) {
				t.Fatalf("lost actual failure: %v", err)
			}
			if tc.caller == nil && errors.Is(err, context.Canceled) {
				t.Fatal("internal cancellation masked transfer failure")
			}
		})
	}
}

func FuzzFetchTransferBound(f *testing.F) {
	f.Add([]byte(""), uint16(0))
	f.Add([]byte("12345"), uint16(4))
	f.Add([]byte("\x00\xff\n"), uint16(3))
	f.Fuzz(func(t *testing.T, data []byte, limit uint16) {
		if len(data) > 1<<16 {
			t.Skip()
		}
		var out bytes.Buffer
		cancelled := false
		writer := &boundedBundleWriter{writer: &out, remaining: int64(limit), cancel: func() { cancelled = true }}
		_, err := io.Copy(writer, strings.NewReader(string(data)))
		if out.Len() > int(limit) {
			t.Fatal("transfer exceeded bound")
		}
		if len(data) <= int(limit) && (err != nil || !bytes.Equal(out.Bytes(), data) || cancelled) {
			t.Fatal("valid binary data changed")
		}
		if len(data) > int(limit) && (!errors.Is(err, errFetchBundleLimit) || !cancelled) {
			t.Fatal("oversized data accepted")
		}
	})
}
