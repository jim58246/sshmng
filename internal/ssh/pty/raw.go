package pty

import (
	"errors"
	"time"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// defaultRawQuietGap 是 ReadRaw 的静默吸收窗口：相邻字节间隔小于该值时
// 继续吸收，视为同一段输出。spec 2026-09-11：内部常量，不可配置。
const defaultRawQuietGap = 400 * time.Millisecond

// MarkRaw 把 conn 标记为 raw 设备（无 unix shell，交换机等）。
// shell="raw" 时 Run 返回错误，交互只能走 SendRaw/ReadRaw。
// 由 login 的 setup 路径在 raw 设备上调用（替代 DetectShell/InjectRC）。
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

// ReadRaw 读取 PTY 新输出（顺序游标语义，供 raw 终端原语用）：
//   - 先消费 pushback，再从 stdoutCh 读；数据不读不丢
//   - buf 为空时等首字节至多 wait（已有数据立即返回）；wait<=0 纯非阻塞轮询
//   - 首字节后静默吸收：字节间隔 < quietGap 继续收；直到 静默 gap / wait 总时限 / maxBytes
//   - maxBytes 截断时剩余字节回存 pushback（不丢），more=true
//   - 无数据可读且通道已关闭 → conn.ErrConnLost（有数据时正常返回，下次调用报错）
//
// 返回 (chunk, more, error)。more=true 表示队列/流中仍有数据，应继续 ReadRaw。
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
		maxBytes = 1 << 20 // 防御：调用方漏传时按 1MB 上限
	}

	var buf []byte

	// 1. pushback 优先（上次截断的剩余 / LoginFlow trailing）
	p.mu.Lock()
	if len(p.pushback) > 0 {
		buf = append(buf, p.pushback...)
		p.pushback = nil
	}
	p.mu.Unlock()

	// 2. 首字节：已有数据立即返回；无数据等至多 wait；wait<=0 纯非阻塞轮询
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

	// 3. 静默吸收循环（wait 同时是总时限，quietAt 取 min(quietGap, 剩余时限)）
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
				// 通道关闭：返回已收数据，下次调用返回 ErrConnLost
				return p.finalizeRaw(buf, maxBytes)
			}
			buf = append(buf, data...)
		case <-t.C:
			// 静默 gap / 总时限到达：非阻塞探测队列是否还有数据
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
	// 4. maxBytes 打满：切分，剩余回存 pushback
	return p.finalizeRaw(buf, maxBytes)
}

// finalizeRaw 把 buf 截到 maxBytes，超出部分回存 pushback（不丢），more=true。
// err 恒为 nil——单独成函数只为统一各退出路径的返回。
func (p *PtyConn) finalizeRaw(buf []byte, maxBytes int) ([]byte, bool, error) {
	if len(buf) > maxBytes {
		p.mu.Lock()
		p.pushback = append([]byte{}, buf[maxBytes:]...)
		p.mu.Unlock()
		return buf[:maxBytes], true, nil
	}
	return buf, false, nil
}
