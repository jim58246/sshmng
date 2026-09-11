package session

import (
	"errors"
	"time"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// RawConn 是支持 raw 终端原语的 Conn 扩展接口（真实实现 *pty.PtyConn）。
// 采用可选接口而非并入 Conn：raw 能力是可选的，避免所有测试替身被迫实现。
type RawConn interface {
	// SendRaw 把 data 原样写入 PTY stdin（不追加换行）。
	SendRaw(data []byte) error
	// ReadRaw 读取 PTY 新输出。more=true 表示队列/流中仍有数据。
	// 远端关闭返回 conn.ErrConnLost。
	ReadRaw(wait time.Duration, maxBytes int) (chunk []byte, more bool, err error)
}

// SetTags 存储登录时刻的服务器 tags 快照，供 stat 返回（AI 的提示通道）。
// 由 Login handler 在 login 成功后调用。nil 清空字段。
func (s *Session) SetTags(tags []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tags == nil {
		s.tags = nil
		return
	}
	s.tags = append([]string(nil), tags...)
}

// SetRaw 标记该 session 是 raw 设备（无 unix shell）。
// raw session 上 RunInSession 报错，交互只能走 SendInSession/ReadInSession。
func (s *Session) SetRaw(raw bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = raw
}

// SendInSession 把 input 原样写入 PTY stdin（raw 终端原语，见 spec 2026-09-11）。
// 仅 idle 状态可用。trace 记一条 CommandTrace（input 作 cmd）；
// 输出由后续 ReadInSession 追加到该条目。写入失败 → conn 已坏，Close session。
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
			// 写 stdin 失败说明连接已坏：close 决策在状态机层，不转回 Idle。
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

// ReadInSession 读取 session PTY 的新输出（raw 终端原语）。
// 仅 idle 状态可用。返回 (output, more, idleMs, error)：
//   - more=true：队列/流中仍有数据，应继续读
//   - idleMs：距上次收到任何输出的毫秒数（信息性信号；本次读到数据时为 0）
//   - 远端关闭 → conn.ErrConnLost，session Close（进 graveyard，get_trace 仍可查）
//
// trace：输出追加到最近一条 trace（send 或 "(read)"）；无任何 trace 时新建 "(read)" 条目。
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
