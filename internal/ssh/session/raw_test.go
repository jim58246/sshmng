package session

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// fakeRawConn 包装 fakeConn,补 raw 原语。ReadRaw 从预置 chunks 依次弹出(确定性,无真实计时)。
type fakeRawConn struct {
	*fakeConn
	mu      sync.Mutex
	chunks  [][]byte // ReadRaw 依次弹出的输出;弹出后剩余 >0 则 more=true
	sends   [][]byte
	sendErr error
	lost    bool // chunks 耗尽后 ReadRaw 返回 ErrConnLost
}

func (f *fakeRawConn) SendRaw(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, append([]byte(nil), data...))
	return f.sendErr
}

func (f *fakeRawConn) ReadRaw(wait time.Duration, maxBytes int) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.chunks) == 0 {
		if f.lost {
			return nil, false, conn.ErrConnLost
		}
		return nil, false, nil
	}
	chunk := f.chunks[0]
	f.chunks = f.chunks[1:]
	if maxBytes > 0 && len(chunk) > maxBytes {
		// 截断:剩余回队首(模拟 pushback 留队)
		f.chunks = append([][]byte{chunk[maxBytes:]}, f.chunks...)
		chunk = chunk[:maxBytes]
	}
	return chunk, len(f.chunks) > 0, nil
}

func newRawSession(t *testing.T, rc *fakeRawConn) *Session {
	t.Helper()
	m := NewManager()
	s := m.NewSession("sid1", "sw1", rc, time.Minute, nil)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSendInSessionHappyPath(t *testing.T) {
	rc := &fakeRawConn{fakeConn: newFakeConn()}
	s := newRawSession(t, rc)
	sent, err := s.SendInSession("show version\r")
	if err != nil {
		t.Fatalf("SendInSession: %v", err)
	}
	if sent != len("show version\r") {
		t.Errorf("sent = %d", sent)
	}
	if string(rc.sends[0]) != "show version\r" {
		t.Errorf("conn got %q", rc.sends[0])
	}
	// trace 记录 send
	tr := s.GetTrace(0, 0)
	if len(tr) != 1 || tr[0].Cmd != "show version\r" {
		t.Errorf("trace = %+v", tr)
	}
}

func TestSendInSessionStateGuards(t *testing.T) {
	rc := &fakeRawConn{fakeConn: newFakeConn()}
	s := newRawSession(t, rc)
	// 白盒置 Running(同包测试,避免阻塞 Run 的时序 gymnastics)
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()
	if _, err := s.SendInSession("x"); err == nil || !strings.Contains(err.Error(), "session busy") {
		t.Errorf("want session busy, got %v", err)
	}
	if _, _, _, err2 := s.ReadInSession(0, 1024); err2 == nil || !strings.Contains(err2.Error(), "session busy") {
		t.Errorf("read want session busy, got %v", err2)
	}
	s.mu.Lock()
	s.state = StateClosed
	s.mu.Unlock()
	if _, err := s.SendInSession("x"); err == nil || !strings.Contains(err.Error(), "session closed") {
		t.Errorf("want session closed, got %v", err)
	}
}

func TestSendInSessionNoRawConn(t *testing.T) {
	// 纯 fakeConn(未实现 RawConn)的 session:send/read 应报不支持
	m := NewManager()
	s2 := m.NewSession("sid2", "srv", newFakeConn(), time.Minute, nil)
	defer s2.Close()
	if _, err := s2.SendInSession("x"); err == nil || !strings.Contains(err.Error(), "raw terminal IO") {
		t.Errorf("want 'does not support raw terminal IO', got %v", err)
	}
	if _, _, _, err := s2.ReadInSession(0, 1024); err == nil || !strings.Contains(err.Error(), "raw terminal IO") {
		t.Errorf("want 'does not support raw terminal IO', got %v", err)
	}
}

func TestSendInSessionWriteErrorCloses(t *testing.T) {
	rc := &fakeRawConn{fakeConn: newFakeConn(), sendErr: errors.New("broken pipe")}
	s := newRawSession(t, rc)
	if _, err := s.SendInSession("x"); err == nil {
		t.Fatal("want error")
	}
	// session 已关闭:再次操作报 session closed
	if _, err := s.SendInSession("y"); err == nil || !strings.Contains(err.Error(), "session closed") {
		t.Errorf("want session closed after send error, got %v", err)
	}
}

func TestReadInSessionAppendsToSendTrace(t *testing.T) {
	rc := &fakeRawConn{fakeConn: newFakeConn(), chunks: [][]byte{[]byte("out1"), []byte("out2")}}
	s := newRawSession(t, rc)
	s.SendInSession("show run\r")
	out, more, _, err := s.ReadInSession(0, 4096)
	if err != nil || out != "out1" || !more {
		t.Fatalf("read1 = %q more=%v err=%v", out, more, err)
	}
	out, more, _, _ = s.ReadInSession(0, 4096)
	if out != "out2" || more {
		t.Fatalf("read2 = %q more=%v", out, more)
	}
	tr := s.GetTrace(0, 0)
	if len(tr) != 1 || tr[0].Output != "out1out2" {
		t.Errorf("read output should append to send trace, got %+v", tr)
	}

	// 无 send 的 read 记 "(read)" 条目
	rc2 := &fakeRawConn{fakeConn: newFakeConn(), chunks: [][]byte{[]byte("banner")}}
	s2 := newRawSession(t, rc2)
	s2.ReadInSession(0, 4096)
	tr2 := s2.GetTrace(0, 0)
	if len(tr2) != 1 || tr2[0].Cmd != "(read)" || tr2[0].Output != "banner" {
		t.Errorf("orphan read trace = %+v", tr2)
	}
}

func TestReadInSessionConnLostCloses(t *testing.T) {
	rc := &fakeRawConn{fakeConn: newFakeConn(), lost: true}
	s := newRawSession(t, rc)
	_, _, _, err := s.ReadInSession(0, 4096)
	if !errors.Is(err, conn.ErrConnLost) {
		t.Fatalf("want ErrConnLost, got %v", err)
	}
	if _, err := s.SendInSession("x"); err == nil || !strings.Contains(err.Error(), "session closed") {
		t.Errorf("session should be closed, got %v", err)
	}
}

func TestReadInSessionIdleMs(t *testing.T) {
	rc := &fakeRawConn{fakeConn: newFakeConn()}
	s := newRawSession(t, rc)
	time.Sleep(25 * time.Millisecond)
	_, _, idleMs, err := s.ReadInSession(0, 4096)
	if err != nil {
		t.Fatalf("ReadInSession: %v", err)
	}
	if idleMs < 20 {
		t.Errorf("idleMs = %d, want >= 20 (since creation)", idleMs)
	}
}

func TestRawSessionRejectsRunInSession(t *testing.T) {
	rc := &fakeRawConn{fakeConn: newFakeConn()}
	s := newRawSession(t, rc)
	s.SetRaw(true)
	_, _, _, _, _, err := s.RunInSession("ls", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "raw device") {
		t.Errorf("want 'raw device' error, got %v", err)
	}
}

func TestSetTagsSetRawAndStat(t *testing.T) {
	rc := &fakeRawConn{fakeConn: newFakeConn()}
	s := newRawSession(t, rc)
	m := s.manager
	s.SetRaw(true)
	s.SetTags([]string{"huawei", "CE12800"})
	stats := m.Stat()
	if len(stats) != 1 {
		t.Fatalf("stat len = %d", len(stats))
	}
	st := stats[0]
	if st.Mode != "raw" {
		t.Errorf("Mode = %q, want raw", st.Mode)
	}
	if len(st.Tags) != 2 || st.Tags[0] != "huawei" {
		t.Errorf("Tags = %v", st.Tags)
	}

	// shell 模式 + nil tags
	m2 := NewManager()
	s2 := m2.NewSession("sid3", "srv", newFakeConn(), time.Minute, nil)
	defer s2.Close()
	st2 := m2.Stat()[0]
	if st2.Mode != "shell" {
		t.Errorf("Mode = %q, want shell", st2.Mode)
	}
	if st2.Tags != nil {
		t.Errorf("Tags = %v, want nil(omitempty)", st2.Tags)
	}
}
