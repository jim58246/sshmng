package session

import (
	"errors"
	"io"
	"sync"
)

// errAllDestinationsFailed 在 fanWriter 的所有目标都 dead 时由 Write 返回，
// 让 Download 的 io.Copy 终止（不再无谓地读源）。
var errAllDestinationsFailed = errors.New("all relay destinations failed")

// fanSlot 是一个目标的 pipe writer 端。dead=true 表示该目标已失败，
// 后续 Write 跳过它（失败隔离）。
type fanSlot struct {
	w    *io.PipeWriter
	dead bool
}

// fanWriter 把下载侧的数据块并发分发到 N 个目标的 io.PipeWriter。
// 每次 Write 并发写所有存活目标，等全部完成；任一目标 Write 失败则标记 dead，
// 不影响其他目标。全部 dead 时返回 errAllDestinationsFailed。
type fanWriter struct {
	mu    sync.Mutex
	slots []*fanSlot
}

// newFanWriter 用给定的 pipe writers 构造 fanWriter。
func newFanWriter(pws []*io.PipeWriter) *fanWriter {
	slots := make([]*fanSlot, len(pws))
	for i, pw := range pws {
		slots[i] = &fanSlot{w: pw}
	}
	return &fanWriter{slots: slots}
}

// Write 并发把 p 写入所有存活目标，等全部完成。
// 返回 (len(p), nil) 只要仍有存活目标；全部 dead 返回 (0, errAllDestinationsFailed)。
func (fw *fanWriter) Write(p []byte) (int, error) {
	fw.mu.Lock()
	live := make([]*fanSlot, 0, len(fw.slots))
	for _, s := range fw.slots {
		if !s.dead {
			live = append(live, s)
		}
	}
	fw.mu.Unlock()

	if len(live) == 0 {
		return 0, errAllDestinationsFailed
	}

	// 并发写每个存活目标；per-chunk 墙钟 = max(各目标接受延迟)，而非 sum。
	var wg sync.WaitGroup
	for _, s := range live {
		wg.Add(1)
		go func(s *fanSlot) {
			defer wg.Done()
			if _, err := s.w.Write(p); err != nil {
				fw.mu.Lock()
				s.dead = true
				fw.mu.Unlock()
			}
		}(s)
	}
	wg.Wait()

	// 重新统计存活数；若本轮全部失败则通知 Download 终止
	fw.mu.Lock()
	remain := 0
	for _, s := range fw.slots {
		if !s.dead {
			remain++
		}
	}
	fw.mu.Unlock()
	if remain == 0 {
		return 0, errAllDestinationsFailed
	}
	return len(p), nil
}
