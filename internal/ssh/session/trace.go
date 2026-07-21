package session

import (
	"fmt"
	"time"
)

// CommandTrace 是单条命令的执行记录，供 get_trace 工具诊断用。
// 字段与设计文档 §3.2 trace 结构一致。
type CommandTrace struct {
	Time      time.Time `json:"time"`
	Cmd       string    `json:"cmd"`
	Output    string    `json:"output"`
	RawOutput string    `json:"raw_output"` // 清洗前的原始 PTY 字节（含 ANSI / sentinel / \r\n），供调试
	ExitCode  int       `json:"exit_code"`
	TimedOut  bool      `json:"timed_out"`
	CtrlCSent bool      `json:"ctrl_c_sent"` // Run 超时后是否发送了 Ctrl-C 中断远程命令
}

// graveTTL 是 close_session 后 trace 在 graveyard 中保留的时长。
const graveTTL = 10 * time.Minute

// defaultTruncOutput 是 get_trace 默认的 Output 截断长度。
const defaultTruncOutput = 200

// GetTrace 返回本 session 的命令 trace。
//   - lastN=0 返回全部；lastN>0 返回最近 N 条（不足时返回全部）
//   - truncOutput=0 不截断；truncOutput>0 截断每条 Output 到该长度
//
// 调用方持有 session 锁外部不需要额外同步。
func (s *Session) GetTrace(lastN, truncOutput int) []CommandTrace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return truncateTraces(s.traces, lastN, truncOutput)
}

// truncateTraces 复制 traces，按 lastN 截取 + truncOutput 截断 Output 和 RawOutput。
// 输入切片不会被修改。
func truncateTraces(traces []CommandTrace, lastN, truncOutput int) []CommandTrace {
	if len(traces) == 0 {
		return []CommandTrace{}
	}
	src := traces
	if lastN > 0 && len(traces) > lastN {
		src = traces[len(traces)-lastN:]
	}
	out := make([]CommandTrace, len(src))
	for i, tr := range src {
		if truncOutput > 0 {
			if len(tr.Output) > truncOutput {
				tr.Output = tr.Output[:truncOutput]
			}
			if len(tr.RawOutput) > truncOutput {
				tr.RawOutput = tr.RawOutput[:truncOutput]
			}
		}
		out[i] = tr
	}
	return out
}

// GetTrace 返回指定 sid 的命令 trace，活 session 与已关闭 session 均可查。
//   - 活 session：从 Session.traces 取
//   - 已关闭 session：从 Manager.graveyard 取，TTL = graveTTL（10min）
//
// sid 不存在（或已超 TTL 清理）返回 error。
// 每次 GetTrace 触发懒清理：扫描 graveyard，移除超 TTL 的条目。
func (m *Manager) GetTrace(sid string, lastN, truncOutput int) ([]CommandTrace, error) {
	// 优先查活 session
	if sess, err := m.Get(sid); err == nil {
		return sess.GetTrace(lastN, truncOutput), nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupGraveyardLocked()
	entry, ok := m.graveyard[sid]
	if !ok {
		return nil, fmt.Errorf("session %q not found (alive or within trace TTL)", sid)
	}
	return truncateTraces(entry.traces, lastN, truncOutput), nil
}

// cleanupGraveyardLocked 移除 graveyard 中超 TTL 的条目。调用者必须持有 m.mu。
func (m *Manager) cleanupGraveyardLocked() {
	now := m.now()
	for sid, entry := range m.graveyard {
		if now.Sub(entry.closedAt) > graveTTL {
			delete(m.graveyard, sid)
		}
	}
}

// buryTraces 把已关闭 session 的 trace 存入 graveyard，10min 后自动清理。
// 由 Session.Close 调用。
func (m *Manager) buryTraces(sid string, traces []CommandTrace) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.graveyard[sid] = graveEntry{traces: traces, closedAt: m.now()}
}

// now 返回当前时间。nowFunc 非空时用之（测试用），否则 time.Now。
func (m *Manager) now() time.Time {
	if m.nowFunc != nil {
		return m.nowFunc()
	}
	return time.Now()
}
