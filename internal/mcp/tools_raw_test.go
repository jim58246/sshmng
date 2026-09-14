package mcp

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// baseFakeConn 只实现 session.Conn(无 raw 原语)——用于"不支持 raw"降级测试。
type baseFakeConn struct {
	mu     sync.Mutex
	closed bool
}

func (f *baseFakeConn) Close() error { f.mu.Lock(); f.closed = true; f.mu.Unlock(); return nil }

func (f *baseFakeConn) Run(cmd string, timeoutMs int, maxOutputBytes int) (string, string, int, bool, bool, bool, int, bool, error) {
	return "", "", 0, false, false, false, 0, false, nil
}

func (f *baseFakeConn) SftpAvailable() bool { return false }

func (f *baseFakeConn) Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	return 0, false, conn.ErrSftpUnavailable
}

func (f *baseFakeConn) UploadSized(src io.Reader, size int64, remotePath string, timeoutMs int) (int, bool, error) {
	return 0, false, conn.ErrSftpUnavailable
}

func (f *baseFakeConn) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error) {
	return 0, false, conn.ErrSftpUnavailable
}

func (f *baseFakeConn) Stat(path string) (os.FileInfo, error) { return nil, conn.ErrSftpUnavailable }

func (f *baseFakeConn) UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
	return conn.DirTransferResult{}, conn.ErrSftpUnavailable
}

func (f *baseFakeConn) DownloadDir(remoteDir, localDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
	return conn.DirTransferResult{}, conn.ErrSftpUnavailable
}

// fakeRawConnForMCP = baseFakeConn + raw 原语(确定性:chunks 预置,无真实计时)。
type fakeRawConnForMCP struct {
	*baseFakeConn
	chunks  [][]byte
	sends   [][]byte
	sendErr error
}

func (f *fakeRawConnForMCP) SendRaw(data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("connection closed")
	}
	f.sends = append(f.sends, append([]byte(nil), data...))
	return f.sendErr
}

func (f *fakeRawConnForMCP) ReadRaw(wait time.Duration, maxBytes int) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.chunks) == 0 {
		return nil, false, nil
	}
	ch := f.chunks[0]
	f.chunks = f.chunks[1:]
	return ch, len(f.chunks) > 0, nil
}

func newRawSvc(t *testing.T, fc *fakeRawConnForMCP, raw bool) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	store := config.NewStore(dir + "/config.json")
	store.Save(&config.Config{Version: "1", Servers: []*config.SSHServer{}})
	knownHosts := conn.NewKnownHostsStore(dir + "/known_hosts")
	svc := NewService(store, knownHosts, nil)
	s := svc.manager.NewSession("sid-raw", "sw1", fc, time.Minute, nil)
	s.SetRaw(raw)
	return svc, "sid-raw"
}

func TestSendInSessionHandler(t *testing.T) {
	fc := &fakeRawConnForMCP{baseFakeConn: &baseFakeConn{}}
	svc, sid := newRawSvc(t, fc, true)
	res, _, err := svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "show version\r"})
	if err != nil || res.IsError {
		t.Fatalf("SendInSession failed: %v %s", err, resultText(t, res))
	}
	if !strings.Contains(resultText(t, res), `"sent_bytes"`) {
		t.Errorf("result = %s", resultText(t, res))
	}
	if string(fc.sends[0]) != "show version\r" {
		t.Errorf("conn input = %q", fc.sends[0])
	}

	// input 超 64KB 报错
	big := strings.Repeat("a", 65537)
	res2, _, _ := svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: big})
	if !res2.IsError {
		t.Error("input >64KB should be rejected")
	}
}

func TestSendInSessionOnNonRawConn(t *testing.T) {
	dir := t.TempDir()
	store := config.NewStore(dir + "/config.json")
	store.Save(&config.Config{Version: "1"})
	svc := NewService(store, conn.NewKnownHostsStore(dir+"/known_hosts"), nil)
	svc.manager.NewSession("sid-shell", "srv", &baseFakeConn{}, time.Minute, nil)
	res, _, _ := svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: "sid-shell", Input: "x"})
	if !res.IsError || !strings.Contains(resultText(t, res), "raw terminal IO") {
		t.Errorf("want raw terminal IO error, got %s", resultText(t, res))
	}
}

// TestSendInSessionEscapeInterpretation 验证 send_in_session 的收敛式转义:
// 服务端解释 \r \n \t \e \uXXXX(及 \\ 逃逸),其余 verbatim。
// 背景:v0.2.0 纯 verbatim 契约下,模型可能把 \r 当字面 backslash-r 双转义发出
// (真实故障:回车不生效)。收敛式解释让"JSON 转义的 CR"与"字面 \r"殊途同归。
func TestSendInSessionEscapeInterpretation(t *testing.T) {
	cases := []struct {
		name  string
		input string // Go 字面量 = 服务端收到的原始字节
		want  string // 期望写入 PTY 的字节
	}{
		// 字面 \r(模型双转义形态)→ 解释为 CR
		{"literal backslash-r", "show version\\r", "show version\r"},
		// 真 CR(JSON 单转义形态)→ 原样通过,与上一条收敛
		{"real CR passes through", "show version\r", "show version\r"},
		{"non-escape set verbatim", "st\\a", "st\\a"}, // \a 不在转义集内,保持 verbatim
		{"tab", "a\\tb", "a\tb"},
		{"newline", "a\\nb", "a\nb"},
		{"escape", "\\e[B", "\x1b[B"},
		{"unicode ctrl-c", "\\u0003", "\x03"},
		{"real ctrl-c passes through", "q\x03", "q\x03"},
		{"escaped backslash then r", "\\\\r", "\\r"}, // 用户要字面 \r 文本
		{"lone trailing backslash", "dir\\", "dir\\"},
		{"no escapes", "display clock", "display clock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fc := &fakeRawConnForMCP{baseFakeConn: &baseFakeConn{}}
			svc, sid := newRawSvc(t, fc, true)
			res, _, err := svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: c.input})
			if err != nil || res.IsError {
				t.Fatalf("SendInSession failed: %v %s", err, resultText(t, res))
			}
			if len(fc.sends) != 1 || string(fc.sends[0]) != c.want {
				t.Errorf("input %q → conn got %q, want %q", c.input, fc.sends, c.want)
			}
		})
	}
}

func TestReadInSessionHandler(t *testing.T) {
	fc := &fakeRawConnForMCP{baseFakeConn: &baseFakeConn{}, chunks: [][]byte{[]byte("Switch> "), []byte("more data")}}
	svc, sid := newRawSvc(t, fc, true)
	res, _, err := svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 1, MaxBytes: 4096})
	if err != nil || res.IsError {
		t.Fatalf("ReadInSession failed: %v %s", err, resultText(t, res))
	}
	txt := resultText(t, res)
	// 注意:textResult 的 JSON 会把 '>' 转义为 >,断言避开裸 '>'
	if !strings.Contains(txt, `"more": true`) || !strings.Contains(txt, "Switch") {
		t.Errorf("result = %s", txt)
	}
	// 第二次读排空
	res2, _, _ := svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 1, MaxBytes: 4096})
	if strings.Contains(resultText(t, res2), `"more": true`) {
		t.Errorf("second read should drain queue: %s", resultText(t, res2))
	}
}
