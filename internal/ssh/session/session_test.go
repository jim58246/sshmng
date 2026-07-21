package session

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"sshmng/internal/ssh/conn"
)

// fakeConn 是 Conn 接口的测试替身，记录所有调用并允许控制 Run 的行为。
type fakeConn struct {
	mu           sync.Mutex
	closed       bool
	runCalls     []string
	runResult    fakeRunResult
	runDelay     time.Duration // Run 阻塞时长，模拟命令执行
	runBlocking  bool          // Run 是否阻塞直到 Close 中断
	runCh        chan struct{} // 用于 blocking 模式下通知 Run 返回
	runUnusable  bool          // Run 返回 connUnusable=true（模拟 drain 超时，但不自己 Close）
}

type fakeRunResult struct {
	output     string
	rawOutput  string
	exitCode   int
	timedOut   bool
	ctrlCSent  bool
	truncated  bool
	totalBytes int
	err        error
}

func newFakeConn() *fakeConn {
	return &fakeConn{runCh: make(chan struct{})}
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.runCh)
	return nil
}

// Closed 返回连接是否已关闭（线程安全，供测试断言使用）。
func (f *fakeConn) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeConn) Run(cmd string, timeoutMs int, maxOutputBytes int) (string, string, int, bool, bool, bool, int, bool, error) {
	f.mu.Lock()
	f.runCalls = append(f.runCalls, cmd)
	f.mu.Unlock()
	if f.runBlocking {
		// 模拟卡住的命令：阻塞直到被中断
		<-f.runCh
		return f.runResult.output, f.runResult.rawOutput, f.runResult.exitCode, f.runResult.timedOut, f.runResult.ctrlCSent, f.runResult.truncated, f.runResult.totalBytes, f.runUnusable, f.runResult.err
	}
	if f.runDelay > 0 {
		time.Sleep(f.runDelay)
	}
	// runUnusable=true 模拟 PtyConn drain 超时：返回 connUnusable=true 让 Session
	// 决定关闭，而不是自己调 Close（close 决策在状态机层）。
	return f.runResult.output, f.runResult.rawOutput, f.runResult.exitCode, f.runResult.timedOut, f.runResult.ctrlCSent, f.runResult.truncated, f.runResult.totalBytes, f.runUnusable, f.runResult.err
}

// SftpAvailable 默认返回 false（fakeConn 不模拟 sftp）。
func (f *fakeConn) SftpAvailable() bool { return false }

// Upload / Download 在 fakeConn 中不支持 sftp，返回 conn.ErrSftpUnavailable。
func (f *fakeConn) Upload(io.Reader, string, int) (int, bool, error) {
	return 0, false, conn.ErrSftpUnavailable
}

func (f *fakeConn) Download(string, io.Writer, int) (int, bool, error) {
	return 0, false, conn.ErrSftpUnavailable
}

// --- 状态机基本转换 ---

// TestRunInSessionStoresRawOutputInTrace 验证 RunInSession 把 conn.Run 返回的
// rawOutput 存入 CommandTrace.RawOutput，供 get_trace 调试时查看未清洗的 PTY 字节。
func TestRunInSessionStoresRawOutputInTrace(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	raw := "hello\r\n\x1b[0m__E_deadbeef__:0__\r\n__P_deadbeef__> "
	conn.runResult = fakeRunResult{
		output:    "hello",
		rawOutput: raw,
		exitCode:  0,
	}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	if _, _, _, _, _, err := s.RunInSession("echo hello", 1000, 0); err != nil {
		t.Fatalf("RunInSession: %v", err)
	}

	traces := s.GetTrace(0, 0)
	if len(traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(traces))
	}
	if traces[0].RawOutput != raw {
		t.Errorf("RawOutput = %q, want %q", traces[0].RawOutput, raw)
	}
	if traces[0].Output != "hello" {
		t.Errorf("Output = %q, want %q", traces[0].Output, "hello")
	}
}

// TestRunInSessionRecordsCtrlCSentInTrace 验证 RunInSession 把 conn.Run 返回的
// ctrlCSent 存入 CommandTrace.CtrlCSent，供 get_trace 诊断"超时是否发了 Ctrl-C"。
func TestRunInSessionRecordsCtrlCSentInTrace(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{
		output:    "",
		exitCode:  130,
		timedOut:  true,
		ctrlCSent: true,
	}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	if _, _, _, _, _, err := s.RunInSession("hang-cmd", 1000, 0); err != nil {
		t.Fatalf("RunInSession: %v", err)
	}

	traces := s.GetTrace(0, 0)
	if len(traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(traces))
	}
	if !traces[0].CtrlCSent {
		t.Errorf("CtrlCSent = false, want true (conn.Run reported ctrlCSent=true)")
	}
	if !traces[0].TimedOut {
		t.Errorf("TimedOut = false, want true")
	}
}

// TestGetTraceTruncatesRawOutput 验证 truncOutput 同时截断 Output 和 RawOutput。
func TestGetTraceTruncatesRawOutput(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{
		output:    "0123456789",
		rawOutput: "RWXYZ0123456789",
	}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)
	s.RunInSession("cmd", 1000, 0)

	traces := s.GetTrace(0, 5)
	if len(traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(traces))
	}
	if traces[0].Output != "01234" {
		t.Errorf("Output = %q, want %q", traces[0].Output, "01234")
	}
	if traces[0].RawOutput != "RWXYZ" {
		t.Errorf("RawOutput = %q, want %q", traces[0].RawOutput, "RWXYZ")
	}
}

func TestSessionStartsIdle(t *testing.T) {
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid1", "srv", newFakeConn(), time.Minute, nil)
	if got := s.State(); got != StateIdle {
		t.Errorf("got %v, want StateIdle", got)
	}
}

func TestRunInSessionTransitionsToRunningAndBack(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "hello", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	// 在 Run 执行前/后检查状态。由于 fakeConn 同步返回，状态转换不可观察；
	// 改用 runDelay 让 Run 阻塞一小段时间，从而能在执行中观察 running。
	conn.runDelay = 50 * time.Millisecond
	done := make(chan struct{})
	go func() {
		_, _, _, _, _, _ = s.RunInSession("echo hello", 1000, 0)
		close(done)
	}()
	// 等一小段让 goroutine 进入 Run
	time.Sleep(10 * time.Millisecond)
	if got := s.State(); got != StateRunning {
		t.Errorf("during Run, state = %v, want StateRunning", got)
	}
	<-done
	if got := s.State(); got != StateIdle {
		t.Errorf("after Run, state = %v, want StateIdle", got)
	}
}

func TestRunInSessionWhileBusyReturnsError(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runDelay = 50 * time.Millisecond
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	done := make(chan struct{})
	go func() {
		_, _, _, _, _, _ = s.RunInSession("long cmd", 1000, 0)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)

	_, _, _, _, _, err := s.RunInSession("second cmd", 1000, 0)
	if err == nil {
		t.Errorf("expected 'session busy' error")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Errorf("error should mention 'busy', got: %v", err)
	}

	<-done
}

// --- idle timeout ---

func TestIdleTimeoutAutoCloses(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	s := mgr.newSessionWithConn("sid1", "srv", conn, 50*time.Millisecond, nil)
	// 等 idle timeout 触发
	time.Sleep(150 * time.Millisecond)
	if got := s.State(); got != StateClosed {
		t.Errorf("state = %v, want StateClosed after idle timeout", got)
	}
	if !conn.Closed() {
		t.Errorf("conn should be closed after idle timeout")
	}
	// session 应从 manager 移除
	if _, err := mgr.Get("sid1"); err == nil {
		t.Errorf("session should be removed from manager after close")
	}
}

func TestActivityResetsIdleTimer(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, 80*time.Millisecond, nil)

	// 在 50ms 时跑一次命令（重置 timer）
	time.Sleep(50 * time.Millisecond)
	_, _, _, _, _, _ = s.RunInSession("cmd", 1000, 0)
	// 命令结束后又过 50ms，总时长虽然超过 80ms，但 timer 应该被重置
	time.Sleep(50 * time.Millisecond)
	if got := s.State(); got == StateClosed {
		t.Errorf("state = StateClosed, want alive (timer should be reset by activity)")
	}

	// 再等一个完整 timeout，应该关闭
	time.Sleep(120 * time.Millisecond)
	if got := s.State(); got != StateClosed {
		t.Errorf("state = %v, want StateClosed after idle timeout", got)
	}
}

func TestRunDuringRunningDoesNotResetIdleTimerPrematurely(t *testing.T) {
	// 命令执行期间不算空闲，timer 不应触发
	mgr := NewManager()
	conn := newFakeConn()
	conn.runDelay = 100 * time.Millisecond
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, 30*time.Millisecond, nil)

	done := make(chan struct{})
	go func() {
		_, _, _, _, _, _ = s.RunInSession("long cmd", 5000, 0)
		close(done)
	}()
	time.Sleep(60 * time.Millisecond) // 超过 idle timeout
	if got := s.State(); got == StateClosed {
		t.Errorf("state should not be Closed during running command")
	}
	<-done
}

// --- close_session ---

func TestCloseSessionSetsClosedState(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := s.State(); got != StateClosed {
		t.Errorf("state = %v, want StateClosed", got)
	}
	if !conn.Closed() {
		t.Errorf("conn should be closed")
	}
}

func TestCloseSessionWhileRunningForceCloses(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runDelay = 200 * time.Millisecond
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	done := make(chan struct{})
	go func() {
		_, _, _, _, _, _ = s.RunInSession("long cmd", 5000, 0)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatalf("Close while running: %v", err)
	}
	if got := s.State(); got != StateClosed {
		t.Errorf("state = %v, want StateClosed", got)
	}
	<-done
}

func TestRunInSessionAfterCloseReturnsError(t *testing.T) {
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid1", "srv", newFakeConn(), time.Minute, nil)
	s.Close()
	_, _, _, _, _, err := s.RunInSession("cmd", 1000, 0)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("expected 'closed' error, got: %v", err)
	}
}

// TestRunInSessionClosesSessionWhenConnReturnsUnusable 验证当 conn.Run 返回
// connUnusable=true（如 drain 超时），Session 应调 s.Close() 转 StateClosed、
// 从 Manager 移除。close 决策在 Session 层，PtyConn 不自己 Close。
//
// 修复前：PtyConn 在 drain 超时时自己调 p.Close()，Session 事后才发现（zombie）。
// 修复后：PtyConn 返回 connUnusable=true，Session 主动 Close。
func TestRunInSessionClosesSessionWhenConnReturnsUnusable(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runUnusable = true // 模拟 drain 超时：Run 返回 connUnusable=true
	conn.runResult = fakeRunResult{
		output:     "",
		exitCode:   -1,
		timedOut:   true,
		ctrlCSent:  true,
	}
	s := mgr.newSessionWithConn("sid1", "srv", conn, time.Minute, nil)

	_, _, _, _, _, err := s.RunInSession("hang-cmd", 1000, 0)
	if err != nil {
		t.Fatalf("RunInSession: %v", err)
	}

	// Session 应已转为 StateClosed
	if got := s.State(); got != StateClosed {
		t.Errorf("state = %v, want StateClosed (conn unusable after Run)", got)
	}
	// Session 应从 Manager 移除（stat 看不到）
	if _, err := mgr.Get("sid1"); err == nil {
		t.Errorf("session should be removed from Manager after conn unusable")
	}
	// stat 不应包含此 session
	stats := mgr.Stat()
	for _, st := range stats {
		if st.SID == "sid1" {
			t.Errorf("stat should not contain closed session sid1, got: %+v", st)
		}
	}
	// conn 不应被 fakeConn 自己 Close（close 决策在 Session，PtyConn 不自己 Close）
	// s.Close() 会调 conn.Close()，所以这里 conn 应已关闭——但是 Session 调的，不是 fakeConn.Run 调的
	if !conn.Closed() {
		t.Errorf("conn should be closed by s.Close() after connUnusable")
	}
}

// --- Manager ---

func TestManagerGetNotFound(t *testing.T) {
	mgr := NewManager()
	if _, err := mgr.Get("nope"); err == nil {
		t.Errorf("expected error for missing session")
	}
}

func TestManagerStatListsAllSessions(t *testing.T) {
	mgr := NewManager()
	conn := newFakeConn()
	conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
	mgr.newSessionWithConn("sid1", "srv1", newFakeConn(), time.Minute, nil)
	mgr.newSessionWithConn("sid2", "srv2", newFakeConn(), time.Minute, nil)

	stats := mgr.Stat()
	if len(stats) != 2 {
		t.Errorf("got %d sessions, want 2", len(stats))
	}
	names := map[string]bool{}
	for _, st := range stats {
		names[st.ServerName] = true
	}
	if !names["srv1"] || !names["srv2"] {
		t.Errorf("expected srv1 and srv2 in stat, got %v", names)
	}
}

func TestManagerConcurrentAccess(t *testing.T) {
	// 并发创建 + 查询 + 关闭，验证不 race / 不 panic。
	mgr := NewManager()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := "sid-" + string(rune('a'+i))
			conn := newFakeConn()
			conn.runResult = fakeRunResult{output: "ok", exitCode: 0}
			s := mgr.newSessionWithConn(sid, "srv", conn, time.Minute, nil)
			_, _, _, _, _, _ = s.RunInSession("cmd", 1000, 0)
			_ = s.Close()
			_ = mgr.Stat()
		}(i)
	}
	wg.Wait()
}
