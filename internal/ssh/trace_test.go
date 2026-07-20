package ssh

import (
	"strings"
	"testing"
	"time"
)

// --- CommandTrace 存储 ---

// TestGetTraceEmpty: 新 session 的 GetTrace 返回空切片。
func TestGetTraceEmpty(t *testing.T) {
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid1", "srv", newFakeConn(), time.Minute, nil)
	if got := s.GetTrace(0, 0); len(got) != 0 {
		t.Errorf("got %d traces, want 0", len(got))
	}
}

// TestGetTraceAfterRun: 跑一条命令后 GetTrace 返回 1 条，字段正确。
func TestGetTraceAfterRun(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "hello", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	if _, _, _, _, _, err := s.RunInSession("echo hello", 1000, 0); err != nil {
		t.Fatalf("RunInSession: %v", err)
	}
	traces := s.GetTrace(0, 0)
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	tr := traces[0]
	if tr.Cmd != "echo hello" {
		t.Errorf("Cmd = %q, want %q", tr.Cmd, "echo hello")
	}
	if tr.Output != "hello" {
		t.Errorf("Output = %q, want %q", tr.Output, "hello")
	}
	if tr.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", tr.ExitCode)
	}
	if tr.Time.IsZero() {
		t.Errorf("Time should be set")
	}
}

// TestGetTraceMultipleRuns: 跑多条命令后 GetTrace 按顺序返回全部。
func TestGetTraceMultipleRuns(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	cmds := []string{"echo one", "echo two", "echo three"}
	for _, cmd := range cmds {
		s.RunInSession(cmd, 1000, 0)
	}
	traces := s.GetTrace(0, 0)
	if len(traces) != 3 {
		t.Fatalf("got %d traces, want 3", len(traces))
	}
	for i, want := range cmds {
		if traces[i].Cmd != want {
			t.Errorf("traces[%d].Cmd = %q, want %q", i, traces[i].Cmd, want)
		}
	}
}

// TestGetTraceLastN: lastN > 0 时只返回最近 N 条。
func TestGetTraceLastN(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	s.RunInSession("echo one", 1000, 0)
	s.RunInSession("echo two", 1000, 0)
	s.RunInSession("echo three", 1000, 0)

	traces := s.GetTrace(2, 0)
	if len(traces) != 2 {
		t.Fatalf("got %d traces, want 2", len(traces))
	}
	if traces[0].Cmd != "echo two" {
		t.Errorf("traces[0].Cmd = %q, want %q", traces[0].Cmd, "echo two")
	}
	if traces[1].Cmd != "echo three" {
		t.Errorf("traces[1].Cmd = %q, want %q", traces[1].Cmd, "echo three")
	}
}

// TestGetTraceLastNExceedsSize: lastN > 实际条数时返回全部。
func TestGetTraceLastNExceedsSize(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	s.RunInSession("echo one", 1000, 0)
	traces := s.GetTrace(10, 0)
	if len(traces) != 1 {
		t.Errorf("got %d traces, want 1", len(traces))
	}
}

// TestGetTraceTruncOutput: truncOutput > 0 时截断 Output；默认 200。
func TestGetTraceTruncOutput(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	longOutput := strings.Repeat("x", 500)
	conn.runResult = fakeRunResult{output: longOutput, exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	s.RunInSession("cat /big/file", 1000, 0)

	// truncOutput=10：截到 10 字符
	traces := s.GetTrace(0, 10)
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	if len(traces[0].Output) != 10 {
		t.Errorf("Output len = %d, want 10", len(traces[0].Output))
	}
	// truncOutput=0：不截断
	traces = s.GetTrace(0, 0)
	if len(traces[0].Output) != 500 {
		t.Errorf("Output len = %d, want 500 (no truncation)", len(traces[0].Output))
	}
	// 默认 200
	traces = s.GetTrace(0, 200)
	if len(traces[0].Output) != 200 {
		t.Errorf("Output len = %d, want 200 (default)", len(traces[0].Output))
	}
}

// --- send_input 记入 trace ---

// TestSendInputRecordedInTrace: Running 状态下 SendInput 的 text 记入当前 trace 的 Inputs。
func TestSendInputRecordedInTrace(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runBlocking = true // Run 阻塞直到 SendSpecial 中断
	conn.runResult = fakeRunResult{output: "interrupted", exitCode: 130, timedOut: false}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	done := make(chan struct{})
	go func() {
		s.RunInSession("long cmd", 30000, 0)
		close(done)
	}()
	// 等 Run 进入 Running
	time.Sleep(20 * time.Millisecond)

	if err := s.SendInput("y\n"); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	// 用 ctrl-c 中断 Run
	s.SendSpecial("ctrl-c")
	<-done

	traces := s.GetTrace(0, 0)
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	if len(traces[0].Inputs) != 1 {
		t.Errorf("Inputs len = %d, want 1", len(traces[0].Inputs))
	}
	if traces[0].Inputs[0] != "y\n" {
		t.Errorf("Inputs[0] = %q, want %q", traces[0].Inputs[0], "y\n")
	}
}

// TestSendInputIdleNotRecorded: Idle 状态下 SendInput 失败，不记入任何 trace。
func TestSendInputIdleNotRecorded(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	s.RunInSession("echo hello", 1000, 0)
	// 现在 idle，SendInput 应报错
	if err := s.SendInput("text"); err == nil {
		t.Errorf("expected idle error")
	}
	traces := s.GetTrace(0, 0)
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	if len(traces[0].Inputs) != 0 {
		t.Errorf("Inputs should be empty, got %v", traces[0].Inputs)
	}
}

// --- close_session 后 trace 保留 10min ---

// TestGetTraceAfterCloseFromGraveyard: close_session 后 GetTrace 仍能取到（走 graveyard）。
func TestGetTraceAfterCloseFromGraveyard(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)
	s.RunInSession("echo hello", 1000, 0)
	s.Close()

	// session 已关闭，但 trace 应仍在 graveyard
	traces, err := mgr.GetTrace("sid1", 0, 0)
	if err != nil {
		t.Fatalf("GetTrace after close: %v", err)
	}
	if len(traces) != 1 {
		t.Errorf("got %d traces, want 1", len(traces))
	}
	if traces[0].Cmd != "echo hello" {
		t.Errorf("Cmd = %q, want %q", traces[0].Cmd, "echo hello")
	}
}

// TestGetTraceGraveyardTTL: close_session 后 10min 内可取，超过 10min 自动清理。
func TestGetTraceGraveyardTTL(t *testing.T) {
	fakeNow := time.Now()
	mgr := NewManager()
	mgr.nowFunc = func() time.Time { return fakeNow }

	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)
	s.RunInSession("echo hello", 1000, 0)
	s.Close()

	// 5min 后仍可取
	fakeNow = fakeNow.Add(5 * time.Minute)
	if _, err := mgr.GetTrace("sid1", 0, 0); err != nil {
		t.Errorf("5min after close: should still be in graveyard, got err: %v", err)
	}

	// 11min 后自动清理，取不到
	fakeNow = fakeNow.Add(6 * time.Minute)
	_, err := mgr.GetTrace("sid1", 0, 0)
	if err == nil {
		t.Errorf("11min after close: should be cleaned up, got nil err")
	}
}

// TestGetTraceUnknownSID: 不存在的 sid 返回 error。
func TestGetTraceUnknownSID(t *testing.T) {
	mgr := NewManager()
	_, err := mgr.GetTrace("nope", 0, 0)
	if err == nil {
		t.Errorf("expected error for unknown sid")
	}
}
