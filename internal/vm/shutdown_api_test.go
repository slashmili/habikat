package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func shutdownServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hk-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return dir
}

func TestShutdownAPIActionAndPeer(t *testing.T) {
	requests := make(chan string, 1)
	dir := shutdownServer(t, func(w http.ResponseWriter, r *http.Request) {
		var action struct {
			Type string `json:"action_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&action); err != nil {
			t.Error(err)
		}
		requests <- r.Method + " " + r.URL.Path + " " + action.Type + " " + r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := requestShutdown(ctx, record{PID: os.Getpid()}, dir); err != nil {
		t.Fatal(err)
	}
	if got := <-requests; got != "PUT /actions SendCtrlAltDel application/json" {
		t.Fatalf("%q", got)
	}
	if err := requestShutdown(ctx, record{PID: os.Getpid() + 1}, dir); err == nil {
		t.Fatal("unrelated socket server accepted")
	}
	select {
	case request := <-requests:
		t.Fatalf("sent action to wrong PID: %s", request)
	default:
	}
}

func TestShutdownAPIRejectsPathAndResponse(t *testing.T) {
	for _, kind := range []string{"missing", "file", "symlink", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			dir := privateTempDir(t)
			if kind == "rejected" {
				dir = shutdownServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) })
			}
			if kind == "file" {
				writeTestFile(t, filepath.Join(dir, "api.sock"), []byte("keep"))
			}
			if kind == "symlink" {
				if err := os.Symlink("/dev/null", filepath.Join(dir, "api.sock")); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := requestShutdown(ctx, record{PID: os.Getpid()}, dir); err == nil {
				t.Fatal("unsupported shutdown accepted")
			}
			if kind == "file" {
				assertFileContents(t, filepath.Join(dir, "api.sock"), []byte("keep"))
			}
		})
	}
}

func TestShutdownAPIHonorsCancellation(t *testing.T) {
	received := make(chan struct{})
	dir := shutdownServer(t, func(w http.ResponseWriter, r *http.Request) { close(received); <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err := requestShutdown(ctx, record{PID: os.Getpid()}, dir)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	<-received
}
