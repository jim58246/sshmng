package mcp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jim58246/sshmng/internal/config"
	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// fakeSwitchServer 模拟无 shell 交换机:接受 pty-req + shell,拒绝 sftp subsystem。
// t 存在 struct 里:runFakeSwitchCLI 需要它做守护断言(t.Errorf goroutine 安全)。
type fakeSwitchServer struct {
	t        *testing.T
	listener net.Listener
	hostKey  cryptossh.Signer
	wg       sync.WaitGroup
}

func newFakeSwitchServer(t *testing.T) *fakeSwitchServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	signer, err := cryptossh.NewSignerFromKey(priv)
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
	cfg := &cryptossh.ServerConfig{
		PasswordCallback: func(m cryptossh.ConnMetadata, pass []byte) (*cryptossh.Permissions, error) {
			if m.User() == "admin" && string(pass) == "switch" {
				return nil, nil
			}
			return nil, errors.New("permission denied")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sshConn, chans, reqs, err := cryptossh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sshConn.Close()
	go cryptossh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(cryptossh.UnknownChannelType, "only session")
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

func (s *fakeSwitchServer) handleSession(ch cryptossh.Channel, reqs <-chan *cryptossh.Request) {
	defer s.wg.Done()
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			runFakeSwitchCLI(s.t, ch) // 同步运行:返回后 defer ch.Close() 才关通道
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
func runFakeSwitchCLI(t *testing.T, ch cryptossh.Channel) {
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
				// 分页状态:按键即时生效(真交换机 More 下空格/q 无需回车),不走行缓冲
				if paging {
					switch b {
					case 'q':
						paging = false
						fmt.Fprint(ch, "\r\nSwitch> ")
					case ' ', '\r', '\n':
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
				if b != '\r' && b != '\n' {
					line = append(line, b)
					continue
				}
				cmd := strings.TrimSpace(string(line))
				line = line[:0]
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

// TestIntegrationRawSwitchFullChain 端到端:raw 交换机 login → send/read → 分页 →
// reboot 断连 → graveyard trace。测试扮演 AI 角色,驱动终端原语。
func TestIntegrationRawSwitchFullChain(t *testing.T) {
	srv := newFakeSwitchServer(t)
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

	// 2. stat 含 mode/sftp_available=false
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

	// 5. 分页:show run → More → 空格续页完成;再次 show run → More → q 中止
	// (翻页键不带 \r,More 提示下即时生效)
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "show run\r"})
	svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 2000, MaxBytes: 65536})
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: " "})
	svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 2000, MaxBytes: 65536})
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "show run\r"})
	svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 2000, MaxBytes: 65536})
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "q"})
	svc.ReadInSession(context.Background(), &mcp.CallToolRequest{}, ReadInSessionArgs{SID: sid, WaitMs: 2000, MaxBytes: 65536})

	// 6. reboot → connection lost → session closed
	svc.SendInSession(context.Background(), &mcp.CallToolRequest{}, SendInSessionArgs{SID: sid, Input: "reboot\r"})
	lostErr := ""
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
