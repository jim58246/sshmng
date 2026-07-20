package ssh

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConn 是 Conn 接口的测试替身，记录所有调用并允许控制 Run 的行为。
type fakeConn struct {
	mu          sync.Mutex
	closed      bool
	sendInputs  []string
	specials    []string
	runCalls    []string
	runResult   fakeRunResult
	runDelay    time.Duration // Run 阻塞时长，模拟命令执行
	runBlocking bool          // Run 是否阻塞直到 SendInput/SendSpecial 中断
	runCh       chan struct{} // 用于 blocking 模式下通知 Run 返回
}

type fakeRunResult struct {
	output     string
	exitCode   int
	timedOut   bool
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

func (f *fakeConn) Run(cmd string, timeoutMs int, maxOutputBytes int) (string, int, bool, bool, int, error) {
	f.mu.Lock()
	f.runCalls = append(f.runCalls, cmd)
	f.mu.Unlock()
	if f.runBlocking {
		// 模拟卡住的命令：阻塞直到被中断
		<-f.runCh
		return f.runResult.output, f.runResult.exitCode, f.runResult.timedOut, f.runResult.truncated, f.runResult.totalBytes, f.runResult.err
	}
	if f.runDelay > 0 {
		time.Sleep(f.runDelay)
	}
	return f.runResult.output, f.runResult.exitCode, f.runResult.timedOut, f.runResult.truncated, f.runResult.totalBytes, f.runResult.err
}

func (f *fakeConn) SendInput(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("connection closed")
	}
	f.sendInputs = append(f.sendInputs, text)
	return nil
}

func (f *fakeConn) SendSpecial(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("connection closed")
	}
	f.specials = append(f.specials, key)
	if f.runBlocking {
		// 模拟 ctrl-c 中断：让 Run 返回
		select {
		case <-f.runCh:
		default:
			close(f.runCh)
		}
	}
	return nil
}

// --- 状态机基本转换 ---

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

func TestSendInputWhileIdleReturnsError(t *testing.T) {
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid1", "srv", newFakeConn(), time.Minute, nil)
	err := s.SendInput("text")
	if err == nil {
		t.Errorf("expected 'session idle' error")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Errorf("error should mention 'idle', got: %v", err)
	}
}

func TestSendSpecialWhileIdleReturnsError(t *testing.T) {
	mgr := NewManager()
	s := mgr.newSessionWithConn("sid1", "srv", newFakeConn(), time.Minute, nil)
	err := s.SendSpecial("ctrl-c")
	if err == nil {
		t.Errorf("expected 'session idle' error")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Errorf("error should mention 'idle', got: %v", err)
	}
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
