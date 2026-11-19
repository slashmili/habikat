//go:build integration

package vm

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStartStopStartThroughShutdownAPI(t *testing.T) {
	env := startFixture(t)
	if _, err := startVM(t.Context(), "vm", readyNow, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	first, err := readRecord(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), "vm", false); err != nil {
		t.Fatal(err)
	}
	console, err := os.ReadFile(filepath.Join(env.dir, "console.log"))
	if err != nil || !bytes.Contains(console, []byte("HABIKAT_SHUTDOWN_CLEAN")) {
		t.Fatalf("graceful API shutdown not observed: %q %v", console, err)
	}
	env.assertStopped(t)
	if _, err := startVM(t.Context(), "vm", readyNow, 5*time.Second); err != nil {
		t.Fatalf("restart after graceful stop: %v", err)
	}
	second, err := readRecord(env.dir)
	if err != nil || !second.Ready || !sameProcess(second, env.dir) {
		t.Fatalf("restarted record: %+v %v", second, err)
	}
	if second.InstanceID != first.InstanceID || second.PID == first.PID && second.StartTime == first.StartTime {
		t.Fatalf("restart identities: first %+v second %+v", first, second)
	}
	if launches := env.launches(t); len(launches) != 4 {
		t.Fatalf("launches = %q", launches)
	}
	env.assertStorage(t)
}

func TestStartFinishesStartInterruptedAfterPIDPublication(t *testing.T) {
	env := startFixture(t)
	if _, err := startVM(t.Context(), "vm", readyNow, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	interrupted, err := readRecord(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	interrupted.Ready = false
	if err := saveRecord(env.dir, interrupted); err != nil {
		t.Fatal(err)
	}
	if err := vmNotRunning("vm", env.dir, interrupted); err == nil {
		t.Fatal("interrupted start treated as ready")
	}
	result, err := startVM(t.Context(), "vm", readyNow, 5*time.Second)
	if err != nil || result.AlreadyRunning {
		t.Fatalf("finish interrupted start: %+v %v", result, err)
	}
	current, err := readRecord(env.dir)
	if err != nil || !current.Ready || current.PID != interrupted.PID {
		t.Fatalf("recovered record = %+v %v", current, err)
	}
	if launches := env.launches(t); len(launches) != 2 {
		t.Fatalf("recovery launched another writer: %q", launches)
	}
}

func TestStartRecoveryLeavesUnreadyAdoptedProcess(t *testing.T) {
	env := startFixture(t)
	if _, err := startVM(t.Context(), "vm", readyNow, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	interrupted, err := readRecord(env.dir)
	if err != nil {
		t.Fatal(err)
	}
	interrupted.Ready = false
	if err := saveRecord(env.dir, interrupted); err != nil {
		t.Fatal(err)
	}
	_, err = startVM(t.Context(), "vm", neverReady, 200*time.Millisecond)
	assertErrorContains(t, err, "earlier interrupted start")
	if !sameProcess(interrupted, env.dir) {
		t.Fatal("start killed a process it did not launch")
	}
	if err := Stop(t.Context(), "vm", true); err != nil {
		t.Fatal(err)
	}
	env.assertStopped(t)
	env.assertStorage(t)
}

func TestFailedStartKeepsDeleteProtection(t *testing.T) {
	env := startFixture(t)
	env.setChildMode(t, "fail")
	if _, err := startVM(t.Context(), "vm", neverReady, 5*time.Second); err == nil {
		t.Fatal("failed boot reported success")
	}
	confirmed := false
	err := Delete(t.Context(), "vm", func() error { confirmed = true; return errors.New("declined") })
	assertErrorContains(t, err, "hk delete --force vm")
	if confirmed {
		t.Fatal("delete of an uninspectable stopped VM asked to discard without --force")
	}
	env.assertStorage(t)
	if err := Delete(t.Context(), "vm", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(env.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forced delete retained storage: %v", err)
	}
}

func TestConcurrentStartsLaunchOneWriter(t *testing.T) {
	env := startFixture(t)
	const starters = 6
	var wg sync.WaitGroup
	errs := make([]error, starters)
	for i := range starters {
		wg.Go(func() {
			_, errs[i] = startVM(t.Context(), "vm", readyNow, 5*time.Second)
		})
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !strings.Contains(err.Error(), "lifecycle operation") {
			t.Fatalf("unexpected start failure: %v", err)
		}
	}
	if succeeded == 0 {
		t.Fatal("no start succeeded")
	}
	processes, err := processesForVM(env.dir)
	if err != nil || len(processes) != 1 {
		t.Fatalf("concurrent starts launched %d writers: %v", len(processes), err)
	}
	env.assertStorage(t)
}

func TestConcurrentStartAndDeleteNeverOrphanAWriter(t *testing.T) {
	for range 5 {
		env := startFixture(t)
		var wg sync.WaitGroup
		var startErr, deleteErr error
		wg.Go(func() { _, startErr = startVM(t.Context(), "vm", readyNow, 5*time.Second) })
		wg.Go(func() { deleteErr = deleteState(t.Context(), "vm", stopProcess) })
		wg.Wait()
		_, statErr := os.Lstat(env.dir)
		processes, err := processesForVM(env.dir)
		if err != nil {
			t.Fatal(err)
		}
		if errors.Is(statErr, os.ErrNotExist) {
			if len(processes) != 0 {
				t.Fatalf("deleted VM left an orphaned writer (start: %v, delete: %v)", startErr, deleteErr)
			}
			continue
		}
		env.assertStorage(t)
		if len(processes) > 1 || (startErr == nil && len(processes) != 1) {
			t.Fatalf("start/delete race left %d writers (start: %v, delete: %v)", len(processes), startErr, deleteErr)
		}
	}
}
