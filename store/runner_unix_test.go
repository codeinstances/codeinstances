// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package store

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

func TestRunStopsWhatTheProgramStarted(t *testing.T) {
	f := newRunFixture(t)
	pidFile := t.TempDir() + "/pid"
	t.Setenv("STORE_TEST_CHILD", "spawn")
	t.Setenv("STORE_TEST_PIDFILE", pidFile)
	inst, err := codeinstances.Record(context.Background(), codeinstances.RecordOptions{
		Argv: []string{os.Args[0], "-test.run=^$"}, KeepEnv: []string{"STORE_TEST_CHILD", "STORE_TEST_PIDFILE"}, Timeout: 500 * time.Millisecond, Isolate: true})
	if err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("STORE_TEST_CHILD")
	inst.Sign(f.key, "ci", time.Now())
	data, _ := instance.Marshal(inst)
	id, _, _ := f.srv.Store.Put(data)
	os.Remove(pidFile)
	f.srv.Runner.Timeout = 500 * time.Millisecond
	if w, res := runReq(t, f.srv, "/v1/instances/"+strings.TrimPrefix(id, "sha256:")+"/replay", "sesame", ""); w.Code != 200 || !res.TimedOut {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the grandchild outlived the run")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
