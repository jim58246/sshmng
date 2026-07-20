package ssh

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// SessionState 表示 session 的当前状态。
type SessionState int

const (
	StateIdle    SessionState = iota // 空闲，可 run_in_session
	StateRunning                     // 命令执行中，run_in_session 报错 "session busy"
	StateClosed                      // 已关闭，所有操作报错 "session closed"
)

func (s SessionState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateRunning:
		return "running"
	case StateClosed:
		return "closed"
	}
	return "unknown"
}

// Conn 抽象 SSH 连接的命令执行 + sftp 文件传输能力，便于测试用 fake 替身。
// 真实实现是 ptyConn（dialer.go + pty.go 组合）。
type Conn interface {
	Close() error
	Run(cmd string, timeoutMs int, maxOutputBytes int) (output string, exitCode int, timedOut bool, truncated bool, totalBytes int, err error)
	SendInput(text string) error
	SendSpecial(key string) error
	SftpAvailable() bool
	Upload(src io.Reader, remotePath string, timeoutMs int) (bytes int, timedOut bool, err error)
	Download(remotePath string, dst io.Writer, timeoutMs int) (bytes int, timedOut bool, err error)
}

// SessionStat 是 stat() 工具返回的单条 session 摘要。
// JSON tag 与设计文档 §3.3 stat() 返回字段名一致。
type SessionStat struct {
	SID          string    `json:"sid"`
	ServerName   string    `json:"name"`
	State        string    `json:"state"`
	SftpAvail    bool      `json:"sftp_available"`
	LastActivity time.Time `json:"last_activity"`
	CommandsRun  int       `json:"commands_run"`
	UptimeS      int       `json:"uptime_s"`
}

// Session 是单个 SSH 连接的状态机。
type Session struct {
	sid          string
	serverName   string
	state        SessionState
	conn         Conn
	sftpAvail    bool
	createdAt    time.Time
	lastActivity time.Time
	commandsRun  int
	idleTimeout  time.Duration
	idleTimer    *time.Timer
	logger       *slog.Logger // 操作日志（idle timeout、异步事件）；nil 时退化为 discard
	manager      *Manager     // 反向引用，用于 Close 时从 Manager 移除
	traces       []CommandTrace
	currentTrace *CommandTrace // Running 期间非 nil，SendInput 追加 Inputs
	mu           sync.Mutex
}

// Manager 持有所有活跃 session。
type Manager struct {
	sessions  map[string]*Session
	graveyard map[string]graveEntry // close_session 后的 trace 存储，TTL = graveTTL
	nowFunc   func() time.Time      // 测试用 fake clock；nil = time.Now
	mu        sync.Mutex
}

// graveEntry 是已关闭 session 的 trace 记录。
type graveEntry struct {
	traces   []CommandTrace
	closedAt time.Time
}

// NewManager 创建空 Manager。
func NewManager() *Manager {
	return &Manager{
		sessions:  make(map[string]*Session),
		graveyard: make(map[string]graveEntry),
	}
}

// NewSession 是生产代码入口：用已有 Conn 创建 session 并加入 Manager。
// 调用方负责装配 Conn（Dialer + PtyConn），Manager 只管状态机与 idle timeout。
// logger 用于 idle timeout 等异步事件；nil 退化为 discard。
func (m *Manager) NewSession(sid, serverName string, conn Conn, idleTimeout time.Duration, logger *slog.Logger) *Session {
	return m.newSessionWithConn(sid, serverName, conn, idleTimeout, logger)
}

// newSessionWithConn 用已有 Conn 创建一个 session 并加入 Manager。测试用入口。
// 真实创建走 Login 方法（dialer + pty 装配 Conn）。
func (m *Manager) newSessionWithConn(sid, serverName string, conn Conn, idleTimeout time.Duration, logger *slog.Logger) *Session {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Session{
		sid:          sid,
		serverName:   serverName,
		state:        StateIdle,
		conn:         conn,
		sftpAvail:    conn.SftpAvailable(),
		createdAt:    time.Now(),
		lastActivity: time.Now(),
		idleTimeout:  idleTimeout,
		logger:       logger,
		manager:      m,
	}
	if idleTimeout > 0 {
		// 先把 timer 字段置 nil，再在锁内创建并赋值，确保 timer 回调读 idleTimer 时
		// 看到的是已赋值状态（回调也可能在 AfterFunc 返回前就触发）。
		s.mu.Lock()
		s.idleTimer = time.AfterFunc(idleTimeout, func() {
			s.logger.Info("idle timeout fired, closing session", "sid", s.sid, "server", s.serverName, "idle_timeout", s.idleTimeout.String())
			s.Close()
		})
		s.mu.Unlock()
	}
	m.mu.Lock()
	m.sessions[sid] = s
	m.mu.Unlock()
	return s
}

// Get 返回指定 sid 的 session，不存在返回 error。
func (m *Manager) Get(sid string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sid]
	if !ok {
		return nil, fmt.Errorf("session %q not found", sid)
	}
	return s, nil
}

// removeSession 从 map 移除指定 sid。Close 内部调用。
func (m *Manager) removeSession(sid string) {
	m.mu.Lock()
	delete(m.sessions, sid)
	m.mu.Unlock()
}

// Stat 返回所有活跃 session 的摘要。已关闭的 session 不在结果中。
func (m *Manager) Stat() []SessionStat {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionStat, 0, len(m.sessions))
	for _, s := range m.sessions {
		s.mu.Lock()
		out = append(out, SessionStat{
			SID:          s.sid,
			ServerName:   s.serverName,
			State:        s.state.String(),
			SftpAvail:    s.sftpAvail,
			LastActivity: s.lastActivity,
			CommandsRun:  s.commandsRun,
			UptimeS:      int(time.Since(s.createdAt).Seconds()),
		})
		s.mu.Unlock()
	}
	return out
}

// State 返回当前状态（线程安全）。
func (s *Session) State() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// SID 返回 session ID。
func (s *Session) SID() string { return s.sid }

// ServerName 返回绑定的 SSHServer name。
func (s *Session) ServerName() string { return s.serverName }

// RunInSession 执行一条命令。会经历 idle → running → idle 转换。
//   - 当前非 idle：返回 "session busy" / "session closed" 错误
//   - 命令执行期间不算空闲，idle timer 不会触发
//   - 执行完毕后重置 idle timer
//   - 命令的 cmd / output / exit_code / timed_out / send_input 调用记入 traces，供 get_trace 取用
func (s *Session) RunInSession(cmd string, timeoutMs int, maxOutputBytes int) (string, int, bool, bool, int, error) {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return "", 0, false, false, 0, errors.New("session closed")
	}
	if s.state == StateRunning {
		s.mu.Unlock()
		return "", 0, false, false, 0, errors.New("session busy")
	}
	s.state = StateRunning
	s.stopIdleTimer()
	tr := &CommandTrace{Time: time.Now(), Cmd: cmd}
	s.currentTrace = tr
	s.mu.Unlock()

	output, exitCode, timedOut, truncated, totalBytes, err := s.conn.Run(cmd, timeoutMs, maxOutputBytes)

	s.mu.Lock()
	tr.Output = output
	tr.ExitCode = exitCode
	tr.TimedOut = timedOut
	s.commandsRun++
	s.lastActivity = time.Now()
	if s.state != StateClosed {
		s.state = StateIdle
		s.resetIdleTimer()
		s.traces = append(s.traces, *tr)
	}
	s.currentTrace = nil
	s.mu.Unlock()
	return output, exitCode, timedOut, truncated, totalBytes, err
}

// SendInput 在 running 状态下向 PTY 发送任意文本，并记入当前 trace 的 Inputs。
// idle / closed 状态下报错。
func (s *Session) SendInput(text string) error {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if s.state != StateRunning {
		s.mu.Unlock()
		return errors.New("session idle, use run_in_session")
	}
	s.mu.Unlock()

	if err := s.conn.SendInput(text); err != nil {
		return err
	}

	s.mu.Lock()
	if s.state == StateRunning && s.currentTrace != nil {
		s.currentTrace.Inputs = append(s.currentTrace.Inputs, text)
	}
	s.mu.Unlock()
	return nil
}

// SendSpecial 在 running 状态下发送控制字符。
// idle / closed 状态下报错。
func (s *Session) SendSpecial(key string) error {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if s.state != StateRunning {
		s.mu.Unlock()
		return errors.New("session idle, use run_in_session")
	}
	s.mu.Unlock()
	return s.conn.SendSpecial(key)
}

// SftpAvailable 返回 login 时 sftp 通道是否建立成功。
func (s *Session) SftpAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sftpAvail
}

// Upload 把 src 上传到远端 remotePath。透传到底层 Conn；session 不存在或已关闭时
// manager.Get 已返回 error。
func (s *Session) Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	return s.conn.Upload(src, remotePath, timeoutMs)
}

// Download 把远端 remotePath 下载到 dst。
func (s *Session) Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error) {
	return s.conn.Download(remotePath, dst, timeoutMs)
}

// Close 强制关闭 session，无论状态。停止 idle timer、关闭 conn、从 Manager 移除。
// trace 复制到 Manager.graveyard 保留 10min 供 get_trace 诊断。
// 重复调用是 no-op。
func (s *Session) Close() error {
	s.mu.Lock()
	if s.state == StateClosed {
		s.mu.Unlock()
		return nil
	}
	s.state = StateClosed
	s.stopIdleTimer()
	tracesCopy := append([]CommandTrace(nil), s.traces...)
	s.mu.Unlock()

	err := s.conn.Close()
	if s.manager != nil {
		s.manager.removeSession(s.sid)
		s.manager.buryTraces(s.sid, tracesCopy)
	}
	return err
}

// resetIdleTimer 重新计时 idle timeout。调用者必须持有 s.mu。
func (s *Session) resetIdleTimer() {
	if s.idleTimer != nil {
		s.idleTimer.Reset(s.idleTimeout)
	}
}

// stopIdleTimer 停止 idle timer。调用者必须持有 s.mu。
func (s *Session) stopIdleTimer() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
}
