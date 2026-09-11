# Raw 设备终端原语(send/read)实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 支持 raw 设备(交换机等无 unix shell 的 SSH 设备):MCP login 跳过 shell 探测,新增 `send_in_session`/`read_in_session` 终端原语,完成判断与分页处理交给 AI。

**Architecture:** 配置加 `raw` 布尔字段;PtyConn 新增 SendRaw/ReadRaw 原语(quiet 吸收语义);Session 状态机层包装(仅 idle 可用、trace 记录);MCP 层暴露两个新工具并在 login/stat 返回 `mode`/`tags`。服务端只提供能力+信息,不做完成判断策略。

**Tech Stack:** Go (crypto/ssh, golang.org/x/crypto/ssh), mark3labs/mcp-go, 标准库 testing。

**Spec:** `docs/superpowers/specs/2026-09-11-raw-device-terminal-primitives-design.md`

## Global Constraints

- 错误消息一律英文;代码注释中文(随代码库惯例)。
- JSON 字段名 snake_case;login 返回键:`sid`/`server_name`/`sftp_available`/`mode`/`tags`。
- 限额(spec 规定,服务端强制):`wait_ms` 默认 5000 上限 60000;`max_bytes` 默认 131072 上限 1048576;send input 上限 65536;quiet gap 常量 400ms 不可配。
- **不修改**哨兵机制任何逻辑:`runWithToken`/`runPS1Only`/`InjectRC`/`DetectShell`/`BuildRC` 零改动。
- 每个任务 TDD:先写测试→跑红→实现→跑绿→commit。commit message 结尾加 `Co-Authored-By: Claude Code <noreply@anthropic.com>`。
- 测试命令统一 `go test ./internal/...`(根模块路径 `github.com/jim58246/sshmng`)。
- ErrConnLost 哨兵错误放 `internal/ssh/conn` 包(session 和 pty 都能引用,避免 import cycle;与 `conn.ErrSftpUnavailable` 同模式)。

---

### Task 1: config `raw` 字段

**Files:**
- Modify: `internal/config/types.go`(SSHServer struct ~:89、serverJSON ~:184、MarshalJSON ~:199、UnmarshalJSON ~:221)
- Test: `internal/config/types_test.go`

**Interfaces:**
- Produces: `SSHServer.Raw bool`(无 json tag,经 serverJSON 中转);JSON 键 `raw`(omitempty)。

- [x] **Step 1: 写失败测试**

在 `internal/config/types_test.go` 追加:

```go
func TestSSHServerRawFieldRoundtrip(t *testing.T) {
	// raw:true 序列化后包含 "raw":true,反序列化还原
	raw := true
	s := &SSHServer{Name: "sw1", Addr: "10.0.0.1:22", User: "admin", Raw: raw}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	if m["raw"] != true {
		t.Errorf("raw = %v, want true; json: %s", m["raw"], data)
	}
	var back SSHServer
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Raw {
		t.Error("Raw should be true after roundtrip")
	}

	// raw:false(零值)不出现 "raw" 键(omitempty)
	s2 := &SSHServer{Name: "srv1", Addr: "10.0.0.2:22", User: "u"}
	data2, _ := json.Marshal(s2)
	if strings.Contains(string(data2), `"raw"`) {
		t.Errorf("raw:false should be omitted, got: %s", data2)
	}
}
```

(若文件未 import `strings`/`encoding/json` 则补;文件里其他测试大概率已 import。)

- [x] **Step 2: 跑红**

Run: `go test ./internal/config/ -run TestSSHServerRawFieldRoundtrip -v`
Expected: FAIL(编译错误 `s.Raw unknown field`)

- [x] **Step 3: 实现**

`internal/config/types.go`:

1. SSHServer struct(约 :89,`Tags` 字段之前)加一行:

```go
	Raw             bool                   // 无 unix shell(交换机等);经 serverJSON.Raw 序列化
```

2. serverJSON struct(:184)加:

```go
	Raw             bool                   `json:"raw,omitempty"`
```

3. `MarshalJSON`(:199)的 `sj := serverJSON{...}` 字面量中加 `Raw: s.Raw,`;`UnmarshalJSON`(:221)中加 `s.Raw = sj.Raw`。

- [x] **Step 4: 跑绿**

Run: `go test ./internal/config/ -v`
Expected: 全部 PASS(含既有测试,raw 零值不影响现有序列化)

- [x] **Step 5: Commit**

```bash
git add internal/config/types.go internal/config/types_test.go
git commit -m "feat(config): add raw field to SSHServer

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 2: conn.ErrConnLost + PtyConn raw 原语

**Files:**
- Modify: `internal/ssh/conn/known_hosts.go`(或 ErrSftpUnavailable 所在文件,`grep -rn "ErrSftpUnavailable = " internal/ssh/conn/` 定位)
- Modify: `internal/ssh/pty/pty.go`(:502 `Run` 方法加 raw 守卫)
- Create: `internal/ssh/pty/raw.go`
- Test: `internal/ssh/pty/raw_test.go`

**Interfaces:**
- Consumes: PtyConn 现有字段 `stdoutCh chan []byte`、`pushback []byte`、`stdin io.WriteCloser`、`doneCh`、`mu`、`shell`、`closed`。
- Produces(Task 3/4/7 依赖,签名精确):
  - `conn.ErrConnLost = errors.New("connection lost")`
  - `(*PtyConn).MarkRaw()` — 置 `shell="raw"`
  - `(*PtyConn).SendRaw(data []byte) error`
  - `(*PtyConn).ReadRaw(wait time.Duration, maxBytes int) (chunk []byte, more bool, err error)`;通道耗尽且已关闭时返回 `conn.ErrConnLost`
  - PtyConn 新字段 `rawQuietGap time.Duration`(测试可覆盖,0=默认 400ms)

- [x] **Step 1: 写失败测试**

`internal/ssh/pty/raw_test.go`(package pty,白盒):

```go
package pty

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// rawEchoServer 是 SendRaw/ReadRaw 测试用的极简 SSH 服务端:
// 接受 pty-req + shell 后,把客户端发来的字节原样回显(模拟无 shell 设备的 echo 行为)。
type rawEchoServer struct {
	listener net.Listener
	hostKey  gossh.Signer
	wg       sync.WaitGroup
	delay    time.Duration // 每次回显前的延迟(模拟慢速输出)
	closeOn  string        // 收到该子串后关闭连接(模拟 connection lost);空=不关
	closed   bool
	mu       sync.Mutex
}

func newRawEchoServer(t *testing.T) *rawEchoServer {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &rawEchoServer{listener: l, hostKey: signer}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *rawEchoServer) Addr() string { return s.listener.Addr().String() }

func (s *rawEchoServer) serve() {
	defer s.wg.Done()
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(c)
	}
}

func (s *rawEchoServer) handle(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	cfg := &gossh.ServerConfig{
		PasswordCallback: func(m gossh.ConnMetadata, pass []byte) (*gossh.Permissions, error) {
			if m.User() == "alice" && string(pass) == "wonderland" {
				return nil, nil
			}
			return nil, errors.New("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, chans, reqs, err := gossh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go gossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(gossh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go s.handleSession(ch, chReqs)
	}
}

func (s *rawEchoServer) handleSession(ch gossh.Channel, reqs <-chan *gossh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			s.wg.Add(1)
			go s.echoLoop(ch)
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func (s *rawEchoServer) echoLoop(ch gossh.Channel) {
	defer s.wg.Done()
	buf := make([]byte, 256)
	for {
		n, err := ch.Read(buf)
		if n > 0 {
			data := string(buf[:n])
			s.mu.Lock()
			shouldClose := s.closeOn != "" && strings.Contains(data, s.closeOn) && !s.closed
			if shouldClose {
				s.closed = true
			}
			s.mu.Unlock()
			if shouldClose {
				return // 直接关闭,不回显
			}
			out := make([]byte, n)
			copy(out, buf[:n])
			if s.delay > 0 {
				time.Sleep(s.delay)
			}
			if _, err := ch.Write(out); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// dialRawEcho 建立 PtyConn(不做 DetectShell/InjectRC——raw 用法)。
func dialRawEcho(t *testing.T, srv *rawEchoServer) *PtyConn {
	t.Helper()
	client, err := gossh.Dial("tcp", srv.Addr(), &gossh.ClientConfig{
		User:            "alice",
		Auth:            []gossh.AuthMethod{gossh.Password("wonderland")},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	p, err := OpenPtyConnWithTimeout(client, "testsid", rawTestLogger(), 0)
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func rawTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
```

(import 需补 `io`、`log/slog`、`errors`。)

测试用例:

```go
func TestMarkRawAndRunGuard(t *testing.T) {
	p := dialRawEcho(t, newRawEchoServer(t))
	p.MarkRaw()
	if p.Shell() != "raw" {
		t.Fatalf("Shell() = %q, want raw", p.Shell())
	}
	_, _, _, _, _, _, _, _, err := p.Run("echo hi", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "raw device") {
		t.Errorf("Run on raw conn should error with 'raw device', got: %v", err)
	}
}

func TestSendRawEcho(t *testing.T) {
	p := dialRawEcho(t, newRawEchoServer(t))
	if err := p.SendRaw([]byte("hello\r")); err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	chunk, more, err := p.ReadRaw(2*time.Second, 4096)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if !bytes.Contains(chunk, []byte("hello")) {
		t.Errorf("chunk = %q, want contains hello", chunk)
	}
	if more {
		t.Error("more should be false after echo drained")
	}
}

func TestReadRawQuietAbsorption(t *testing.T) {
	// 回显带 30ms 延迟:quiet gap=200ms 时一次 read 吸收全部字节
	srv := newRawEchoServer(t)
	srv.delay = 30 * time.Millisecond
	p := dialRawEcho(t, srv)
	p.rawQuietGap = 200 * time.Millisecond
	if err := p.SendRaw([]byte("abcdef\r")); err != nil {
		t.Fatalf("SendRaw: %v", err)
	}
	chunk, _, err := p.ReadRaw(2*time.Second, 4096)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if !bytes.Contains(chunk, []byte("abcdef")) {
		t.Errorf("quiet absorption failed: %q", chunk)
	}
}

func TestReadRawMaxBytesSplit(t *testing.T) {
	p := dialRawEcho(t, newRawEchoServer(t))
	p.rawQuietGap = 50 * time.Millisecond
	p.SendRaw([]byte("0123456789"))
	chunk, more, err := p.ReadRaw(2*time.Second, 4)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if string(chunk) != "0123" || !more {
		t.Errorf("chunk=%q more=%v, want 0123/true", chunk, more)
	}
	chunk2, more2, err := p.ReadRaw(2*time.Second, 4096)
	if err != nil {
		t.Fatalf("ReadRaw 2: %v", err)
	}
	if !bytes.Contains(chunk2, []byte("456789")) || more2 {
		t.Errorf("chunk2=%q more2=%v, want remainder/false", chunk2, more2)
	}
}

func TestReadRawWaitTimeoutNoData(t *testing.T) {
	p := dialRawEcho(t, newRawEchoServer(t))
	start := time.Now()
	chunk, more, err := p.ReadRaw(150*time.Millisecond, 4096)
	if err != nil {
		t.Fatalf("ReadRaw should not error on wait timeout: %v", err)
	}
	if len(chunk) != 0 || more {
		t.Errorf("want empty/false, got %q/%v", chunk, more)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Error("ReadRaw should have blocked ~wait duration")
	}
}

func TestReadRawConnLost(t *testing.T) {
	srv := newRawEchoServer(t)
	srv.closeOn = "die"
	p := dialRawEcho(t, srv)
	p.rawQuietGap = 30 * time.Millisecond
	p.SendRaw([]byte("die"))
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, _, err := p.ReadRaw(200*time.Millisecond, 4096)
		if errors.Is(err, conn.ErrConnLost) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ErrConnLost within 3s, last err: %v", err)
		}
	}
}
```

- [x] **Step 2: 跑红**

Run: `go test ./internal/ssh/pty/ -run 'TestMarkRaw|TestSendRaw|TestReadRaw' -v`
Expected: FAIL(编译错误:raw.go 不存在、conn.ErrConnLost 未定义)

- [x] **Step 3: 实现**

`internal/ssh/conn/`(ErrSftpUnavailable 同文件)加:

```go
// ErrConnLost 表示 PTY 输出通道已关闭(SSH channel EOF / 连接断开)。
// Session 层收到后应 Close session。
var ErrConnLost = errors.New("connection lost")
```

`internal/ssh/pty/pty.go` `Run` 方法(:502)在 `p.closed` 检查之后加:

```go
	if p.shell == "raw" {
		return "", "", 0, false, false, false, 0, true, errors.New("raw device: Run not supported, use SendRaw/ReadRaw")
	}
```

PtyConn struct(:102 `ctrlCDrainTimeout` 附近)加字段:

```go
	// rawQuietGap 是 ReadRaw 的静默吸收窗口:相邻字节间隔小于该值时继续吸收。
	// 默认 defaultRawQuietGap(400ms),测试可覆盖。
	rawQuietGap time.Duration
```

`internal/ssh/pty/raw.go` 新建:

```go
package pty

import (
	"bytes"
	"errors"
	"time"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// defaultRawQuietGap 是 ReadRaw 的静默吸收窗口:相邻字节间隔小于该值时
// 继续吸收,视为同一段输出。spec 2026-09-11:内部常量,不可配置。
const defaultRawQuietGap = 400 * time.Millisecond

// MarkRaw 把 conn 标记为 raw 设备(无 unix shell)。
// shell="raw" 时 Run 返回错误,交互只能走 SendRaw/ReadRaw。
func (p *PtyConn) MarkRaw() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shell = "raw"
}

// SendRaw 把 data 原样写入 PTY stdin。不追加换行——回车由调用方自带。
func (p *PtyConn) SendRaw(data []byte) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("connection closed")
	}
	p.mu.Unlock()
	_, err := p.stdin.Write(data)
	return err
}

// ReadRaw 读取 PTY 新输出(顺序游标语义,供 raw 终端原语用):
//   - 先消费 pushback,再从 stdoutCh 读;数据不读不丢
//   - buf 为空时等首字节至多 wait(已有数据立即返回);wait<=0 纯非阻塞轮询
//   - 首字节后静默吸收:字节间隔 < quietGap 继续收;直到 静默 gap / wait 总时限 / maxBytes
//   - maxBytes 截断时剩余字节回存 pushback(不丢),more=true
//   - 无数据可读且通道已关闭 → conn.ErrConnLost(有数据时正常返回,下次调用报错)
//
// 返回 (chunk, more, error)。more=true 表示队列/流中仍有数据,应继续 ReadRaw。
func (p *PtyConn) ReadRaw(wait time.Duration, maxBytes int) ([]byte, bool, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false, errors.New("connection closed")
	}
	quietGap := p.rawQuietGap
	p.mu.Unlock()
	if quietGap <= 0 {
		quietGap = defaultRawQuietGap
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20 // 防御:调用方漏传时按 1MB 上限
	}

	var buf []byte

	// 1. pushback 优先(上次截断的剩余 / LoginFlow trailing)
	p.mu.Lock()
	if len(p.pushback) > 0 {
		buf = append(buf, p.pushback...)
		p.pushback = nil
	}
	p.mu.Unlock()

	// 2. 首字节:已有数据立即返回;无数据等至多 wait;wait<=0 纯非阻塞轮询
	if len(buf) == 0 {
		var first []byte
		var ok bool
		if wait <= 0 {
			select {
			case first, ok = <-p.stdoutCh:
			default:
				return nil, false, nil
			}
		} else {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case first, ok = <-p.stdoutCh:
			case <-timer.C:
				return nil, false, nil
			case <-p.doneCh:
				return nil, false, conn.ErrConnLost
			}
		}
		if !ok {
			return nil, false, conn.ErrConnLost
		}
		buf = append(buf, first...)
	}

	// 3. 静默吸收循环(wait 同时是总时限,quietAt 取 min(quietGap, 剩余时限))
	deadline := time.Now().Add(wait)
	for len(buf) < maxBytes {
		quietAt := time.Now().Add(quietGap)
		if quietAt.After(deadline) {
			quietAt = deadline
		}
		t := time.NewTimer(time.Until(quietAt))
		select {
		case data, ok := <-p.stdoutCh:
			t.Stop()
			if !ok {
				// 通道关闭:返回已收数据,下次调用返回 ErrConnLost
				return p.finalizeRaw(buf, maxBytes)
			}
			buf = append(buf, data...)
		case <-t.C:
			// 静默 gap / 总时限到达:非阻塞探测队列是否还有数据
			select {
			case data, ok := <-p.stdoutCh:
				if !ok {
					return p.finalizeRaw(buf, maxBytes)
				}
				buf = append(buf, data...)
			default:
				return p.finalizeRaw(buf, maxBytes)
			}
		case <-p.doneCh:
			t.Stop()
			return p.finalizeRaw(buf, maxBytes)
		}
	}
	// 4. maxBytes 打满:切分,剩余回存 pushback
	return p.finalizeRaw(buf, maxBytes)
}

// finalizeRaw 把 buf 截到 maxBytes,超出部分回存 pushback(不丢),more=true。
func (p *PtyConn) finalizeRaw(buf []byte, maxBytes int) ([]byte, bool) {
	if len(buf) > maxBytes {
		p.mu.Lock()
		p.pushback = append([]byte{}, buf[maxBytes:]...)
		p.mu.Unlock()
		return buf[:maxBytes], true
	}
	return buf, false
}
```

- [x] **Step 4: 跑绿**

Run: `go test ./internal/ssh/pty/ -v`
Expected: 全部 PASS(含既有 integration 测试,哨兵逻辑零改动)

- [x] **Step 5: Commit**

```bash
git add internal/ssh/conn/ internal/ssh/pty/raw.go internal/ssh/pty/raw_test.go internal/ssh/pty/pty.go
git commit -m "feat(pty): raw terminal primitives SendRaw/ReadRaw with quiet absorption

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 3: Session 层状态机 + trace + stat

**Files:**
- Create: `internal/ssh/session/raw.go`
- Modify: `internal/ssh/session/session.go`(SessionStat :60、Session struct :71、NewSession :115 附近、Manager.Stat :172)
- Test: `internal/ssh/session/raw_test.go`

**Interfaces:**
- Consumes: Task 2 的 `conn.ErrConnLost`;现有 `fakeConn`(session_test.go)。
- Produces(Task 4/7 依赖,签名精确):
  - `type RawConn interface { SendRaw(data []byte) error; ReadRaw(wait time.Duration, maxBytes int) (chunk []byte, more bool, err error) }`
  - `(*Session).SetTags(tags []string)`、`(*Session).SetRaw(raw bool)`
  - `(*Session).SendInSession(input string) (int, error)`
  - `(*Session).ReadInSession(waitMs, maxBytes int) (output string, more bool, idleMs int64, err error)`
  - `SessionStat` 新字段 `Mode string json:"mode"`、`Tags []string json:"tags,omitempty"`

- [x] **Step 1: 写失败测试**

`internal/ssh/session/raw_test.go`(package session,白盒):

```go
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

func newRawSession(t *testing.T, rc *fakeRawConn) (*Session, *Manager) {
	t.Helper()
	m := NewManager()
	s := m.NewSession("sid1", "sw1", rc, time.Minute, nil)
	t.Cleanup(func() { s.Close() })
	return s, m
}

func TestSendInSessionHappyPath(t *testing.T) {
	rc := &fakeRawConn{fakeConn: &fakeConn{}}
	s, _ := newRawSession(t, rc)
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
	rc := &fakeRawConn{fakeConn: &fakeConn{}}
	s, _ := newRawSession(t, rc)
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
	s2 := m.NewSession("sid2", "srv", &fakeConn{}, time.Minute, nil)
	defer s2.Close()
	if _, err := s2.SendInSession("x"); err == nil || !strings.Contains(err.Error(), "raw terminal IO") {
		t.Errorf("want 'does not support raw terminal IO', got %v", err)
	}
	if _, _, _, err := s2.ReadInSession(0, 1024); err == nil || !strings.Contains(err.Error(), "raw terminal IO") {
		t.Errorf("want 'does not support raw terminal IO', got %v", err)
	}
}

func TestSendInSessionWriteErrorCloses(t *testing.T) {
	rc := &fakeRawConn{fakeConn: &fakeConn{}, sendErr: errors.New("broken pipe")}
	s, _ := newRawSession(t, rc)
	if _, err := s.SendInSession("x"); err == nil {
		t.Fatal("want error")
	}
	// session 已关闭:再次操作报 session closed
	if _, err := s.SendInSession("y"); err == nil || !strings.Contains(err.Error(), "session closed") {
		t.Errorf("want session closed after send error, got %v", err)
	}
}

func TestReadInSessionAppendsToSendTrace(t *testing.T) {
	rc := &fakeRawConn{fakeConn: &fakeConn{}, chunks: [][]byte{[]byte("out1"), []byte("out2")}}
	s, _ := newRawSession(t, rc)
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
	rc2 := &fakeRawConn{fakeConn: &fakeConn{}, chunks: [][]byte{[]byte("banner")}}
	s2, _ := newRawSession(t, rc2)
	s2.ReadInSession(0, 4096)
	tr2 := s2.GetTrace(0, 0)
	if len(tr2) != 1 || tr2[0].Cmd != "(read)" || tr2[0].Output != "banner" {
		t.Errorf("orphan read trace = %+v", tr2)
	}
}

func TestReadInSessionConnLostCloses(t *testing.T) {
	rc := &fakeRawConn{fakeConn: &fakeConn{}, lost: true}
	s, _ := newRawSession(t, rc)
	_, _, _, err := s.ReadInSession(0, 4096)
	if !errors.Is(err, conn.ErrConnLost) {
		t.Fatalf("want ErrConnLost, got %v", err)
	}
	if _, err := s.SendInSession("x"); err == nil || !strings.Contains(err.Error(), "session closed") {
		t.Errorf("session should be closed, got %v", err)
	}
}

func TestReadInSessionIdleMs(t *testing.T) {
	rc := &fakeRawConn{fakeConn: &fakeConn{}}
	s, _ := newRawSession(t, rc)
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
	rc := &fakeRawConn{fakeConn: &fakeConn{}}
	s, _ := newRawSession(t, rc)
	s.SetRaw(true)
	_, _, _, _, _, err := s.RunInSession("ls", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "raw device") {
		t.Errorf("want 'raw device' error, got %v", err)
	}
}

func TestSetTagsSetRawAndStat(t *testing.T) {
	rc := &fakeRawConn{fakeConn: &fakeConn{}}
	s, m := newRawSession(t, rc)
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
	s2 := m2.NewSession("sid3", "srv", &fakeConn{}, time.Minute, nil)
	defer s2.Close()
	st2 := m2.Stat()[0]
	if st2.Mode != "shell" {
		t.Errorf("Mode = %q, want shell", st2.Mode)
	}
	if st2.Tags != nil {
		t.Errorf("Tags = %v, want nil(omitempty)", st2.Tags)
	}
}
```

(若 `NewManager` 构造器名字不同,以 session.go 实际为准——`grep -n "func NewManager" internal/ssh/session/session.go`。)

- [x] **Step 2: 跑红**

Run: `go test ./internal/ssh/session/ -run 'TestSendIn|TestReadIn|TestRawSession|TestSetTags' -v`
Expected: FAIL(编译错误:raw.go 不存在、SessionStat 无 Mode/Tags 字段)

- [x] **Step 3: 实现**

`internal/ssh/session/raw.go` 新建:

```go
package session

import (
	"errors"
	"time"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// RawConn 是支持 raw 终端原语的 Conn 扩展接口(真实实现 *pty.PtyConn)。
// 采用可选接口而非并入 Conn:raw 能力是可选的,避免所有测试替身被迫实现。
type RawConn interface {
	// SendRaw 把 data 原样写入 PTY stdin(不追加换行)。
	SendRaw(data []byte) error
	// ReadRaw 读取 PTY 新输出。more=true 表示队列/流中仍有数据。
	// 远端关闭返回 conn.ErrConnLost。
	ReadRaw(wait time.Duration, maxBytes int) (chunk []byte, more bool, err error)
}

// SetTags 存储登录时刻的服务器 tags 快照,供 stat 返回(AI 的提示通道)。
func (s *Session) SetTags(tags []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tags == nil {
		s.tags = nil
		return
	}
	s.tags = append([]string(nil), tags...)
}

// SetRaw 标记该 session 是 raw 设备(无 unix shell)。
// raw session 上 RunInSession 报错,交互只能走 SendInSession/ReadInSession。
func (s *Session) SetRaw(raw bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = raw
}

// SendInSession 把 input 原样写入 PTY stdin(raw 终端原语)。
// 仅 idle 状态可用。trace 记一条 CommandTrace(input 作 cmd);
// 输出由后续 ReadInSession 追加到该条目。写入失败 → conn 已坏,Close session。
func (s *Session) SendInSession(input string) (int, error) {
	s.mu.Lock()
	switch {
	case s.state == StateClosed:
		s.mu.Unlock()
		return 0, errors.New("session closed")
	case s.state == StateRunning:
		s.mu.Unlock()
		return 0, errors.New("session busy")
	}
	rc, ok := s.conn.(RawConn)
	if !ok {
		s.mu.Unlock()
		return 0, errors.New("session does not support raw terminal IO")
	}
	s.state = StateRunning
	s.stopIdleTimer()
	s.mu.Unlock()

	err := rc.SendRaw([]byte(input))

	s.mu.Lock()
	now := time.Now()
	s.lastActivity = now
	s.commandsRun++
	needClose := false
	if s.state != StateClosed {
		s.traces = append(s.traces, CommandTrace{Time: now, Cmd: input})
		if err != nil {
			needClose = true
		} else {
			s.state = StateIdle
			s.resetIdleTimer()
		}
	}
	s.mu.Unlock()

	if needClose {
		s.logger.Warn("send failed, closing session", "server", s.serverName, "err", err)
		s.Close()
	}
	return len(input), err
}

// ReadInSession 读取 session PTY 的新输出(raw 终端原语)。
// 仅 idle 状态可用。返回 (output, more, idleMs, error):
//   - more=true:队列/流中仍有数据,应继续读
//   - idleMs:距上次收到任何输出的毫秒数(信息性信号;本次读到数据时为 0)
//   - 远端关闭 → conn.ErrConnLost,session Close(进 graveyard,get_trace 仍可查)
//
// trace:输出追加到最近一条 trace(send 或 "(read)");无任何 trace 时新建 "(read)" 条目。
func (s *Session) ReadInSession(waitMs, maxBytes int) (string, bool, int64, error) {
	s.mu.Lock()
	switch {
	case s.state == StateClosed:
		s.mu.Unlock()
		return "", false, 0, errors.New("session closed")
	case s.state == StateRunning:
		s.mu.Unlock()
		return "", false, 0, errors.New("session busy")
	}
	rc, ok := s.conn.(RawConn)
	if !ok {
		s.mu.Unlock()
		return "", false, 0, errors.New("session does not support raw terminal IO")
	}
	s.state = StateRunning
	s.stopIdleTimer()
	s.mu.Unlock()

	wait := time.Duration(waitMs) * time.Millisecond
	chunk, more, err := rc.ReadRaw(wait, maxBytes)

	s.mu.Lock()
	now := time.Now()
	idleMs := now.Sub(s.lastOutputAt).Milliseconds()
	s.lastActivity = now
	if len(chunk) > 0 {
		s.lastOutputAt = now
		idleMs = 0
		if n := len(s.traces); n > 0 {
			s.traces[n-1].Output += string(chunk)
		} else {
			s.traces = append(s.traces, CommandTrace{Time: now, Cmd: "(read)", Output: string(chunk)})
		}
	}
	needClose := false
	if s.state != StateClosed {
		if errors.Is(err, conn.ErrConnLost) {
			needClose = true
		} else {
			s.state = StateIdle
			s.resetIdleTimer()
		}
	}
	s.mu.Unlock()

	if needClose {
		s.logger.Warn("conn lost during read, closing session", "server", s.serverName)
		s.Close()
	}
	return string(chunk), more, idleMs, err
}

// modeString 返回 stat 用的 mode 字段值。
func (s *Session) modeString() string {
	if s.raw {
		return "raw"
	}
	return "shell"
}
```

`internal/ssh/session/session.go`:

1. Session struct(:71)加两个字段:

```go
	tags          []string // 登录时刻服务器 tags 快照(stat 返回)
	raw           bool     // raw 设备(无 unix shell)
	lastOutputAt  time.Time // 最近一次收到 PTY 输出的时刻(ReadInSession 的 idle_ms 信号源)
```

2. `NewSession`(Manager 方法,:115 附近)在赋值 `createdAt`/`lastActivity` 处同步加 `lastOutputAt: <同一 now 值>`(保持与 createdAt 同刻)。

3. SessionStat(:60)加:

```go
	Mode         string    `json:"mode"`
	Tags         []string  `json:"tags,omitempty"`
```

4. `Manager.Stat`(:172)的 `out = append(out, SessionStat{...})` 中加:

```go
			Mode:         s.modeString(),
			Tags:         s.tags,
```

- [x] **Step 4: 跑绿**

Run: `go test ./internal/ssh/session/ -v`
Expected: 全部 PASS

- [x] **Step 5: Commit**

```bash
git add internal/ssh/session/raw.go internal/ssh/session/raw_test.go internal/ssh/session/session.go
git commit -m "feat(session): raw terminal primitives with state machine, trace and stat fields

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 4: MCP 工具 + login/stat 接线

**Files:**
- Create: `internal/mcp/tools_raw.go`
- Modify: `internal/mcp/server.go`(工具注册 ~:191 之后)
- Modify: `internal/mcp/tools_session.go`(Login handler :57-118、setupDirect :124-170、setupPatternA :258-331、setupPatternB、Stat 附近无改动)
- Test: `internal/mcp/tools_raw_test.go`;Modify: `internal/mcp/tools_session_test.go`(TestIntegrationLoginRunClose 加 mode 断言)

**Interfaces:**
- Consumes: Task 3 的 `SetTags/SetRaw/SendInSession/ReadInSession`;Task 2 的 `MarkRaw`。
- Produces:
  - `Service.SendInSession(ctx, req, args SendInSessionArgs)`、`Service.ReadInSession(ctx, req, args ReadInSessionArgs)`
  - `SendInSessionArgs{SID, Input string}`;`ReadInSessionArgs{SID string; WaitMs, MaxBytes int}`
  - login 返回 map 增加 `"mode"` 与 `"tags"`

- [x] **Step 1: 写失败测试**

`internal/mcp/tools_raw_test.go`(package mcp):

```go
package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/ssh/conn"
	"github.com/jim58246/sshmng/internal/ssh/session"
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
	m := svc.manager
	s := m.NewSession("sid-raw", "sw1", fc, time.Minute, nil)
	s.SetRaw(raw)
	return svc, "sid-raw"
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] not TextContent")
	}
	return tc.Text
}

func TestSendInSessionHandler(t *testing.T) {
	fc := &fakeRawConnForMCP{}
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

func TestReadInSessionHandler(t *testing.T) {
	fc := &fakeRawConnForMCP{chunks: [][]byte{[]byte("Switch> "), []byte("more data")}}
	svc, sid := newRawSvc(t, fc, true)
	res, _, err := svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 1, MaxBytes: 4096})
	if err != nil || res.IsError {
		t.Fatalf("ReadInSession failed: %v %s", err, resultText(t, res))
	}
	txt := resultText(t, res)
	if !strings.Contains(txt, `"more": true`) || !strings.Contains(txt, "Switch>") {
		t.Errorf("result = %s", txt)
	}
	// 第二次读排空
	res2, _, _ := svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 1, MaxBytes: 4096})
	if strings.Contains(resultText(t, res2), `"more": true`) {
		t.Errorf("second read should drain queue: %s", resultText(t, res2))
	}
}
```

(`baseFakeConn`/`fakeRawConnForMCP` 已按"嵌入拆分"写好:`shellOnly` 场景直接用 `baseFakeConn`。文件需 import `context`/`errors`/`io`/`os`/`strings`/`sync`/`testing`/`time` + mcp/config/conn/session 包。)

同时修改 `TestIntegrationLoginRunClose`(tools_session_test.go):login 断言后追加:

```go
	if loginResult["mode"] != "shell" {
		t.Errorf("mode = %v, want shell", loginResult["mode"])
	}
	if _, ok := loginResult["tags"]; !ok {
		t.Errorf("login result should contain tags key")
	}
```

- [x] **Step 2: 跑红**

Run: `go test ./internal/mcp/ -run 'TestSendInSession|TestReadInSession|TestIntegrationLoginRunClose' -v`
Expected: FAIL(SendInSessionArgs 未定义、mode 键不存在)

- [x] **Step 3: 实现**

`internal/mcp/tools_raw.go` 新建:

```go
package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// raw 终端原语的限额(spec 2026-09-11,服务端强制,不信任 AI 传参)。
const (
	defaultReadWaitMs   = 5000
	maxReadWaitMs       = 60000
	defaultReadMaxBytes = 128 * 1024
	maxReadMaxBytes     = 1 << 20
	maxSendInputBytes   = 64 * 1024
)

// SendInSessionArgs 是 send_in_session 工具的入参。
type SendInSessionArgs struct {
	SID   string `json:"sid"`
	Input string `json:"input" jsonschema:"raw input written verbatim to the PTY; include Enter ('\r'), Ctrl-C ('\u0003'), pager keys yourself. Max 64KB"`
}

// ReadInSessionArgs 是 read_in_session 工具的入参。
type ReadInSessionArgs struct {
	SID      string `json:"sid"`
	WaitMs   int    `json:"wait_ms,omitempty" jsonschema:"optional, default 5000 (max 60000). Blocks until first output byte or timeout; use 1 for non-blocking poll. Do NOT poll with small values"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"optional, default 131072 (max 1048576). Excess stays queued, more=true"`
}

// SendInSession 把 input 原样写入 session PTY(终端原语,详见 server instructions)。
func (s *Service) SendInSession(ctx context.Context, req *mcp.CallToolRequest, args SendInSessionArgs) (*mcp.CallToolResult, any, error) {
	if len(args.Input) > maxSendInputBytes {
		return errorResult("input too large: %d bytes (max %d)", len(args.Input), maxSendInputBytes)
	}
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	s.sessionLogger(req, args.SID).Debug("send_in_session",
		"server", sess.ServerName(), "input_bytes", len(args.Input))
	sent, err := sess.SendInSession(args.Input)
	if err != nil {
		return errorResult("%v", err)
	}
	return textResult(map[string]any{"sid": args.SID, "sent_bytes": sent})
}

// ReadInSession 读取 session PTY 的新输出(quiet 吸收,顺序游标)。
func (s *Service) ReadInSession(ctx context.Context, req *mcp.CallToolRequest, args ReadInSessionArgs) (*mcp.CallToolResult, any, error) {
	waitMs := args.WaitMs
	if waitMs <= 0 {
		waitMs = defaultReadWaitMs
	}
	if waitMs > maxReadWaitMs {
		waitMs = maxReadWaitMs
	}
	maxBytes := args.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultReadMaxBytes
	}
	if maxBytes > maxReadMaxBytes {
		maxBytes = maxReadMaxBytes
	}
	sess, err := s.manager.Get(args.SID)
	if err != nil {
		return errorResult("%v", err)
	}
	s.sessionLogger(req, args.SID).Debug("read_in_session",
		"server", sess.ServerName(), "wait_ms", waitMs, "max_bytes", maxBytes)
	output, more, idleMs, err := sess.ReadInSession(waitMs, maxBytes)
	if err != nil {
		return errorResult("%v", err)
	}
	return textResult(map[string]any{
		"output":  output,
		"more":    more,
		"idle_ms": idleMs,
	})
}
```

`internal/mcp/server.go`:`get_trace` 注册块(:191)之后插入:

```go
	mcp.AddTool(server, &mcp.Tool{
		Name:        "send_in_session",
		Description: "Send raw input to a session's PTY (like typing in a terminal). Input is written verbatim: include the Enter key yourself ('\\r'), control chars ('\\u0003' = Ctrl-C), pager keys (' ', 'q'). Only works when session is idle (check stat). Works on ALL sessions: raw devices (mode=raw, no unix shell — the primary way to run commands) and unix shells (for interactive/persistent programs like vim/top/tail -f, between run_in_session calls). Max input 64KB. Returns {sid, sent_bytes}. State (view mode, pager position, full-screen app) persists across calls.",
	}, svc.SendInSession)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_in_session",
		Description: "Read new output produced by a session's PTY since the last read (sequential cursor; unread output stays queued, nothing is lost). Absorbs output while bytes keep arriving (<400ms gaps) and returns {output, more, idle_ms}. more=true: queue still has data — call again to drain. idle_ms: ms since the last output byte (informational completion signal; large idle_ms + self-consistent content = command likely done, no confirm read needed). Defaults: wait_ms 5000 (max 60000) blocks until first byte; max_bytes 131072 (max 1048576), excess stays queued. Prefer large wait_ms (3-10s); do NOT poll with small values. Pager prompts (e.g. '---- More ----') appear in output — handle per device conventions (send pager keys or a disable-paging command).",
	}, svc.ReadInSession)
```

同时更新 `login` 的 Description(:174):把 `Returns {sid, server_name, sftp_available}.` 改为

```
Returns {sid, server_name, sftp_available, mode, tags}. mode: 'shell' = unix shell (use run_in_session); 'raw' = no unix shell, e.g. network switch (run_in_session is rejected; use send_in_session/read_in_session). tags mirror the server's configured tags (human→AI hints, e.g. vendor/model).
```

`stat` 的 Description(:186)末尾追加 ` Each entry includes mode (raw/shell) and tags.`

`internal/mcp/tools_session.go` `Login` handler:在 `sess := s.manager.NewSession(...)` 与 `sess.SetLoginFlowTrace(loginTrace)` 之后加:

```go
	sess.SetRaw(srv.Raw)
	sess.SetTags(srv.Tags)
```

返回 map 改为:

```go
	mode := "shell"
	if srv.Raw {
		mode = "raw"
	}
	tags := srv.Tags
	if tags == nil {
		tags = []string{}
	}
	return textResult(map[string]any{
		"sid":            sid,
		"server_name":    srv.Name,
		"sftp_available": sess.SftpAvailable(),
		"mode":           mode,
		"tags":           tags,
	})
```

三条 setup 路径的 DetectShell+InjectRC 块改为 raw 条件执行。`setupDirect`(:153-163)把:

```go
	if err := ptyConn.DetectShell(); err != nil {
		...
	}
	rcTrace, err := ptyConn.InjectRC()
	if err != nil {
		...
	}
	trace = append(trace, rcTrace...)
```

替换为:

```go
	if srv.Raw {
		// raw 设备(交换机等):无 unix shell,跳过探测与 RC 注入。
		ptyConn.MarkRaw()
	} else {
		if err := ptyConn.DetectShell(); err != nil {
			ptyConn.Close()
			return nil, trace, fmt.Errorf("detect shell: %w", err)
		}
		rcTrace, err := ptyConn.InjectRC()
		if err != nil {
			ptyConn.Close()
			trace = append(trace, rcTrace...)
			return nil, trace, fmt.Errorf("direct: %w", &pty.LoginFlowError{Stage: "rc_inject", Trace: trace, Err: err})
		}
		trace = append(trace, rcTrace...)
	}
```

`setupPatternA`(:312-323)同样结构,把 DetectShell+InjectRC 包进 `if srv.Raw { ptyConn.MarkRaw() } else { ...原逻辑... }`(错误前缀保持 `patternA:` 不变)。`setupPatternB` 的 target 段做同样处理(grep `DetectShell()` 定位,错误前缀随原函数)。

- [x] **Step 4: 跑绿**

Run: `go test ./internal/mcp/ -v`
Expected: 全部 PASS

- [x] **Step 5: Commit**

```bash
git add internal/mcp/tools_raw.go internal/mcp/tools_raw_test.go internal/mcp/server.go internal/mcp/tools_session.go internal/mcp/tools_session_test.go
git commit -m "feat(mcp): send_in_session/read_in_session tools, login/stat mode+tags, raw login skip

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 5: server instructions 更新

**Files:**
- Modify: `internal/mcp/server.go`(:36 serverInstructions)
- Test: `internal/mcp/server_test.go`(TestNewServerSetsInstructions :50)

**Interfaces:**
- Consumes: Task 4 的工具名。
- Produces: instructions 含 raw/send/read 使用模式说明。

- [x] **Step 1: 写失败测试**

`TestNewServerSetsInstructions` 中追加断言(照现有断言风格):

```go
	for _, want := range []string{"send_in_session", "read_in_session", "mode: 'raw'", "idle_ms", "---- More ----"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions missing %q", want)
		}
	}
```

- [x] **Step 2: 跑红**

Run: `go test ./internal/mcp/ -run TestNewServerSetsInstructions -v`
Expected: FAIL

- [x] **Step 3: 实现**

`serverInstructions` 在 `== Session semantics ==` 小节后插入新小节(注意 const 拼接的反引号转义,文内代码片段用 `+ "`...`" +` 拼接,与现有 :54 行同模式):

```
== Raw devices & terminal primitives ==
- login returns mode: 'shell' = unix shell, prefer run_in_session; 'raw' = no unix shell (network switches etc.) — run_in_session is REJECTED; use send_in_session/read_in_session for everything.
- send_in_session(sid, input) types input verbatim into the PTY (Enter='\r', Ctrl-C=\u0003). read_in_session(sid, wait_ms, max_bytes) returns new output {output, more, idle_ms}.
- Typical loop: send("show version\r") → read(wait_ms=5000) → judge from content + idle_ms → next command. more=true → keep reading to drain the queue.
- Pager prompts (e.g. '---- More ----') pause output: send ' ' to page on, 'q' to abort, or disable paging per device convention — tags and output tell you the vendor; the server ships no vendor recipes.
- Do NOT poll read_in_session with small wait_ms; use 3-10s. Large idle_ms + complete content = done, no confirm read.
- The primitives also work on shell sessions for persistent programs (tail -f, top, vim); state is rejected while run_in_session is running (session busy).
```

- [x] **Step 4: 跑绿**

Run: `go test ./internal/mcp/ -run TestNewServerSetsInstructions -v`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat(mcp): server instructions for raw devices and terminal primitives

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 6: CLI 非交互模式对 raw 设备明确报错

**Files:**
- Modify: `internal/cli/ssh_cmd.go`(runSSHCmd :65 附近)
- Test: `internal/cli/ssh_cmd_test.go`

**Interfaces:**
- Consumes: Task 1 的 `srv.Raw`。
- Produces: `sshmng ssh <raw-server> <cmd>` 以 exit 1 报错,不发起 SSH 拨号。

- [x] **Step 1: 写失败测试**

`internal/cli/ssh_cmd_test.go` 追加(参照该文件现有 runSSHCmd 测试的 config 构造方式;若现有测试用临时 config 文件,同法):

```go
func TestSSHCmdRawNonInteractiveRejected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := `{"version":"1","servers":[{"name":"sw1","addr":"127.0.0.1:1","user":"u","auth":{"password":"p"},"raw":true}]}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := runSSHCmd(context.Background(), []string{"--config", cfgPath, "sw1", "show version"}, &out)
	if code != 1 {
		t.Errorf("exit = %d, want 1; output: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "raw device: no unix shell, non-interactive mode not supported") {
		t.Errorf("output = %s", out.String())
	}
}
```

(命令行参数形式以现有测试为准:`--config` 在 name 之前;若现有测试把 `--config` 放最后,保持一致。)

- [x] **Step 2: 跑红**

Run: `go test ./internal/cli/ -run TestSSHCmdRawNonInteractiveRejected -v`
Expected: FAIL(现在会先拨号 → 连接失败,错误信息不同)

- [x] **Step 3: 实现**

`runSSHCmd`(:65 `resolveSSHTarget` 之后、`setupSSH` 之前)插入:

```go
	if srv != nil && command != "" && srv.Raw {
		fmt.Fprintln(out, "Error: raw device: no unix shell, non-interactive mode not supported; use interactive mode")
		return 1
	}
```

- [x] **Step 4: 跑绿**

Run: `go test ./internal/cli/ -v`
Expected: 全部 PASS

- [x] **Step 5: Commit**

```bash
git add internal/cli/ssh_cmd.go internal/cli/ssh_cmd_test.go
git commit -m "feat(cli): clear early error for non-interactive command on raw devices

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 7: 假交换机 fixture + 全链路集成测试

**Files:**
- Create: `internal/mcp/tools_session_raw_test.go`

**Interfaces:**
- Consumes: Task 1-4 全部产出。
- Produces: `fakeSwitchServer`(测试 fixture)+ 全链路守护测试(含"raw login 不得触发 DetectShell"的守护断言)。

- [x] **Step 1: 写 fixture + 测试(此任务整体是测试,先写全再跑)**

`internal/mcp/tools_session_raw_test.go`(package mcp)。fixture 骨架照抄 `fakeShellServerForMCP`(tools_session_test.go)的 serve/handle/handleSession,`shell` 请求后进入 `runFakeSwitchCLI`;**不做 sftp subsystem**(Reply false)。核心 CLI 循环:

```go
// fakeSwitchServer 模拟无 shell 交换机:接受 pty-req + shell,拒绝 sftp subsystem。
// 骨架与 rawEchoServer 同型,shell 请求后进入 runFakeSwitchCLI。
// t 存在 struct 里:runFakeSwitchCLI 需要它做守护断言(t.Errorf goroutine 安全)。
type fakeSwitchServer struct {
	t        *testing.T
	listener net.Listener
	hostKey  gossh.Signer
	wg       sync.WaitGroup
}

func newFakeSwitchServer(t *testing.T) *fakeSwitchServer {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSwitchServer{t: t, listener: l, hostKey: signer}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() {
		l.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeSwitchServer) Addr() string { return s.listener.Addr().String() }

func (s *fakeSwitchServer) serve() {
	defer s.wg.Done()
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(c)
	}
}

func (s *fakeSwitchServer) handle(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	cfg := &gossh.ServerConfig{
		PasswordCallback: func(m gossh.ConnMetadata, pass []byte) (*gossh.Permissions, error) {
			if m.User() == "admin" && string(pass) == "switch" {
				return nil, nil
			}
			return nil, errors.New("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, chans, reqs, err := gossh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go gossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(gossh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go s.handleSession(ch, chReqs)
	}
}

func (s *fakeSwitchServer) handleSession(ch gossh.Channel, reqs <-chan *gossh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			runFakeSwitchCLI(s.t, ch)
			return
		case "subsystem":
			req.Reply(false, nil) // 交换机无 sftp
		default:
			req.Reply(false, nil)
		}
	}
}

// runFakeSwitchCLI 模拟无 shell 交换机 CLI:回显输入、Switch> 提示符、
// show version / show run(带 ---- More ---- 分页)/ reboot(断连)。
// 收到 shell 探测/RC 注入特征即 t.Errorf —— 守护 raw login 不触发 DetectShell/InjectRC。
func runFakeSwitchCLI(t *testing.T, ch gossh.Channel) {
	fmt.Fprint(ch, "\r\nWelcome to FakeSwitch\r\nSwitch> ")
	var line []byte
	pagesLeft := 0
	paging := false
	buf := make([]byte, 256)
	for {
		n, err := ch.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				ch.Write([]byte{b}) // 逐字节回显(模拟设备 echo)
				if b != '\r' && b != '\n' {
					line = append(line, b)
					continue
				}
				raw := string(line)
				line = line[:0]
				// 分页分支优先:任意键(含空格/回车)都可能触发,不能用 TrimSpace 后的空串判断
				if paging {
					if strings.HasPrefix(raw, "q") {
						paging = false
						fmt.Fprint(ch, "\r\nSwitch> ")
					} else { // 空格/回车 = 续页
						if pagesLeft > 0 {
							pagesLeft--
							fmt.Fprint(ch, "\r\nline1\r\nline2\r\nline3\r\nline4")
							if pagesLeft == 0 {
								paging = false
								fmt.Fprint(ch, "\r\nSwitch> ")
							} else {
								fmt.Fprint(ch, "\r\n---- More ----")
							}
						} else {
							paging = false
							fmt.Fprint(ch, "\r\nSwitch> ")
						}
					}
					continue
				}
				cmd := strings.TrimSpace(raw)
				if cmd == "" {
					fmt.Fprint(ch, "\r\nSwitch> ")
					continue
				}
				if strings.Contains(cmd, "__SHELL_DETECT__") || strings.Contains(cmd, "PS1=") || strings.Contains(cmd, "__sshmng_dr") {
					t.Errorf("fake switch received shell-probe/RC content: %q — raw login must skip DetectShell/InjectRC", cmd)
				}
				switch {
				case strings.HasPrefix(cmd, "show version"):
					fmt.Fprint(ch, "\r\nFakeSwitch Version 1.0\r\nUptime: 1d\r\nSwitch> ")
				case strings.HasPrefix(cmd, "show run"):
					paging = true
					pagesLeft = 1
					fmt.Fprint(ch, "\r\nline1\r\nline2\r\nline3\r\nline4\r\n---- More ----")
				case strings.HasPrefix(cmd, "reboot"):
					return // 关闭 channel → 客户端 ErrConnLost
				default:
					fmt.Fprintf(ch, "\r\n%% Invalid input: %s\r\nSwitch> ", cmd)
				}
			}
		}
		if err != nil {
			return
		}
	}
}
```

全链路测试:

```go
// TestIntegrationRawSwitchFullChain 端到端:raw 交换机 login → send/read → 分页 →
// Ctrl-C → reboot 断连 → graveyard trace。
func TestIntegrationRawSwitchFullChain(t *testing.T) {
	srv := newFakeSwitchServer(t) // 骨架同 newFakeShellServerForMCP,shell 分支调 runFakeSwitchCLI(t, ch)
	dir := t.TempDir()
	store := config.NewStore(dir + "/config.json")
	store.Save(&config.Config{
		Version:      "1",
		IdleTimeoutS: 300,
		Servers: []*config.SSHServer{
			{Name: "sw1", Addr: srv.Addr(), User: "admin", Auth: config.SSHAuth{Password: "switch"},
				Raw: true, Tags: []string{"huawei", "CE12800"}},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	// 1. login:mode=raw,tags 透传
	res, _, err := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "sw1"})
	if err != nil || res.IsError {
		t.Fatalf("login: %v %s", err, resultText(t, res))
	}
	lr := parseJSON(t, resultText(t, res)).(map[string]any)
	if lr["mode"] != "raw" {
		t.Fatalf("mode = %v, want raw", lr["mode"])
	}
	tags, _ := lr["tags"].([]any)
	if len(tags) != 2 || tags[0] != "huawei" {
		t.Errorf("tags = %v", tags)
	}
	sid := lr["sid"].(string)

	// 2. stat 含 mode/tags
	stRes, _, _ := svc.Stat(context.Background(), &mcp.CallToolRequest{}, StatArgs{})
	st := parseJSON(t, resultText(t, stRes)).([]any)[0].(map[string]any)
	if st["mode"] != "raw" || st["sftp_available"] != false {
		t.Errorf("stat = %v", st)
	}

	// 3. run_in_session 必须被拒
	runRes, _, _ := svc.RunInSession(context.Background(), &mcp.CallToolRequest{}, RunInSessionArgs{SID: sid, Cmd: "ls"})
	if !runRes.IsError || !strings.Contains(resultText(t, runRes), "raw device") {
		t.Errorf("run_in_session on raw should fail, got: %s", resultText(t, runRes))
	}

	// 4. send + read:show version
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "show version\r"})
	rdRes, _, err := svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 2000, MaxBytes: 65536})
	if err != nil || rdRes.IsError {
		t.Fatalf("read: %v %s", err, resultText(t, rdRes))
	}
	rd := parseJSON(t, resultText(t, rdRes)).(map[string]any)
	if !strings.Contains(rd["output"].(string), "FakeSwitch Version 1.0") {
		t.Errorf("output = %q", rd["output"])
	}

	// 5. 分页:show run → More → 空格续页 → q
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "show run\r"})
	svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 2000, MaxBytes: 65536})
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: " "})
	svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 2000, MaxBytes: 65536})
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "q"})
	svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 2000, MaxBytes: 65536})

	// 6. reboot → connection lost → session closed
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "reboot\r"})
	var lostErr string
	for i := 0; i < 20; i++ {
		rdRes, _, _ := svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 500, MaxBytes: 65536})
		lostErr = resultText(t, rdRes)
		if strings.Contains(lostErr, "connection lost") {
			break
		}
	}
	if !strings.Contains(lostErr, "connection lost") {
		t.Fatalf("no connection lost after reboot, last: %s", lostErr)
	}

	// 7. graveyard:get_trace 仍可查,含 send 的 trace
	trRes, _, _ := svc.GetTrace(context.Background(), &mcp.CallToolRequest{}, GetTraceArgs{SID: sid})
	if trRes.IsError {
		t.Errorf("get_trace after close: %s", resultText(t, trRes))
	}
	if !strings.Contains(resultText(t, trRes), "show version") {
		t.Errorf("trace missing send history: %s", resultText(t, trRes))
	}
}
```

(`GetTraceArgs` 字段名以 tools_session.go 实际为准:应为 `SID`/`LastN`/`TruncOutput`。host key 走 known_hosts TOFU,与现有 integration 测试一致,无需显式关闭。)

- [x] **Step 2: 跑**

Run: `go test ./internal/mcp/ -run TestIntegrationRawSwitchFullChain -v`
Expected: PASS(fixture 与测试同任务交付;若失败按输出修 fixture 时序,如提示符先于首条命令送达)

- [x] **Step 3: 全量回归**

Run: `go test ./internal/... -v`
Expected: 全部 PASS

- [x] **Step 4: Commit**

```bash
git add internal/mcp/tools_session_raw_test.go
git commit -m "test(mcp): fake switch fixture and raw-device full-chain integration test

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 8: 双语文档

**Files:**
- Modify: `README.md`、`README.zh-CN.md`、`docs/agents.md`、`docs/agents.md` 的 zh 镜像 `docs/zh-CN/agents.md`、`docs/configuration.md`、`docs/zh-CN/configuration.md`

**Interfaces:**
- Consumes: Task 1-7 的最终行为与限额数值。
- Produces: 与代码同步的双语文档。

- [x] **Step 1: 盘点硬编码数字与工具数**

Run: `grep -rn "19 个工具\|19 tools\|19 MCP" README.md README.zh-CN.md docs/ | grep -v superpowers`
把所有 "19" 工具计数改为 "21"。

- [x] **Step 2: 英文文档更新**

1. `docs/configuration.md` SSHServer 字段表加一行(位置随 `tags`):

```markdown
| `raw` | bool | `false` | No unix shell (network switch, etc.): login skips shell detection and RC injection; `run_in_session` is rejected, use `send_in_session`/`read_in_session`. |
```

2. `docs/agents.md` MCP 工具清单加两节(签名+语义,数值与 Task 4 description 一致,包含使用模式示例 send→read 循环、more 排空、idle_ms 判完成、分页自适应、限额表);`login` 返回字段更新为 `{sid, server_name, sftp_available, mode, tags}`;`stat` 字段表加 `mode`/`tags`;`get_trace` 一节补一句:raw 会话的 send/read 也记录 trace(send 作 cmd,read 输出追加到最近条目);unix session 混用提示:send/read 未读尽的残留输出会被下一次 `run_in_session` 的 setup 步骤当作噪声丢弃。

3. `README.md` MCP 工具概览表加 `send_in_session`/`read_in_session` 两行;若有 raw 概念提及处,补一句 raw 设备用法。

- [x] **Step 3: 中文镜像**

`README.zh-CN.md`、`docs/zh-CN/agents.md`、`docs/zh-CN/configuration.md` 逐段镜像英文改动(术语:终端原语 / 静默吸收 / 分页自适应)。

- [x] **Step 4: 双向一致性自查**

Run:
```bash
grep -c "send_in_session" README.md docs/agents.md README.zh-CN.md docs/zh-CN/agents.md
grep -c '"raw"' docs/configuration.md docs/zh-CN/configuration.md
```
Expected: 4 个文件都 >0;中英文数值(5000/60000/131072/1048576/65536/400ms)一致。

- [x] **Step 5: Commit**

```bash
git add README.md README.zh-CN.md docs/agents.md docs/zh-CN/agents.md docs/configuration.md docs/zh-CN/configuration.md
git commit -m "docs: raw devices and terminal primitives (EN + zh-CN)

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```

---

### Task 9: 终验

**Files:**
- 无新改动(验证任务;发现问题则最小修复后重跑)

- [x] **Step 1: 全量构建 + vet + 测试**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全部通过

- [x] **Step 2: pre-release checklist 预检(CLAUDE.md)**

Run:
```bash
grep -rn "send_in_session\|read_in_session" docs/agents.md | wc -l
grep -rn "raw" docs/configuration.md | wc -l
grep -rn "[0-9]\+\.[0-9]\+\.[0-9]\+" README.md docs/ README.zh-CN.md docs/zh-CN/ | grep -v superpowers | grep -v "127.0.0" || true
```
Expected: 工具签名/字段/示例双语文档齐全;版本号 grep 无新引入的硬编码版本。

- [x] **Step 3: 工具计数核对**

Run: `go test ./internal/mcp/ -run TestNewServerSetsInstructions -v && grep -c "mcp.AddTool" internal/mcp/server.go`
Expected: PASS;AddTool 计数 = 21。

- [x] **Step 4: 收尾 commit(如有修复)**

```bash
git status --short
# 若有修复:
git add -A && git commit -m "fix: final verification fixes for raw device support

Co-Authored-By: Claude Code <noreply@anthropic.com>"
```
