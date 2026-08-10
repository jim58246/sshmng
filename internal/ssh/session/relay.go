package session

import (
	"errors"
	"io"
	"sync"

	"github.com/jim58246/sshmng/internal/progress"
	"github.com/jim58246/sshmng/internal/ssh/conn"
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

// RelayDest 是单个目标的 relay 结果。
type RelayDest struct {
	DstSid    string
	DstServer string
	OK        bool
	Bytes     int
	TimedOut  bool
	Err       error
}

// RelayResult 是 RelayTransfer 的汇总结果。
//   - Err 非 nil 表示下载或某目标失败（部分失败）；硬错误（src 不存在等）经 Go error 返回。
//   - Destinations 按 dstSids 输入顺序，每项含 per-dest ok/bytes/timed_out/err。
type RelayResult struct {
	DownloadedBytes int
	TimedOut        bool
	SrcServer       string
	Destinations    []RelayDest
	Err             error
}

// RelayTransfer 把 srcSid session 上的 srcPath 文件流式中转到 dstSids 各 session 的 dstPath。
// 1:N fanout：源文件只读一次，fanWriter 并发分发到所有存活目标。1:1 是 N=1 特例。
//
// 返回 (RelayResult, error)：
//   - Go error 仅用于硬错误（srcSid 不存在、dstSids 为空）。
//   - 部分失败（下载失败、某目标失败、源非普通文件）返回 (res, nil)，res.Err 非 nil，
//     各 dest 的 OK 反映自身成败——一个目标失败不连累其他目标。
//
// 它是 RelayTransferWithProgress(nil, nil) 的薄委托层：保留原签名与语义，
// 不改变 fanWriter 屏障扇出模型。MCP (tools_relay.go) 继续调用本方法。
func (m *Manager) RelayTransfer(srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int) (RelayResult, error) {
	res, err := m.RelayTransferWithProgress(srcSid, srcPath, dstSids, dstPath, timeoutMs, nil, nil)
	if err != nil {
		return RelayResult{}, err
	}
	return *res, nil
}

// RelayTransferWithProgress 是带进度回调的 RelayTransfer。它不改变屏障扇出模型：
// 存活目标按下载侧节奏锁步推进，onDownload 反映的是共享下载进度（累计字节，高频）；
// onDestDone 在每个目标上传完成时触发一次（低频）。任一回调可为 nil。
//
// 返回 (*RelayResult, error)：
//   - Go error 仅用于硬错误（srcSid 不存在、dstSids 为空）。
//   - 部分失败返回 (res, nil)，res.Err 非 nil，各 dest 的 OK 反映自身成败。
//
// 进度机制：下载侧的 fanWriter 包一层 progress.CountingWriter，每次 Write 累计字节
// 并触发 onDownload；这对 sftp 读侧流水线透明（只包装 dst，不改 *sftp.File.WriteTo
// 的并发路径）。onDestDone 在每个上传 goroutine 末尾 + 每个预检失败分支触发，nil-guarded。
func (m *Manager) RelayTransferWithProgress(
	srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int,
	onDownload func(bytes int64),
	onDestDone func(dstSid string, ok bool, bytes int64, err error),
) (*RelayResult, error) {
	srcSess, err := m.Get(srcSid)
	if err != nil {
		return nil, err
	}
	if len(dstSids) == 0 {
		return nil, errors.New("no relay destinations")
	}

	// M-2: 预检源 sftp 通道。源 sftp 不可用则整个传输必然失败（所有目标都拿不到数据），
	// 与"源非普通文件"同构：返回 (res, nil)，res.Err = conn.ErrSftpUnavailable，
	// 每个目标记 per-dest 错误——避免无谓 spawn N+1 个立即失败的 goroutine。
	if !srcSess.SftpAvailable() {
		dests := make([]RelayDest, len(dstSids))
		for i, sid := range dstSids {
			dests[i] = RelayDest{DstSid: sid, Err: errors.New("source sftp unavailable")}
			if onDestDone != nil {
				onDestDone(sid, false, 0, errors.New("source sftp unavailable"))
			}
		}
		return &RelayResult{
			SrcServer:    srcSess.ServerName(),
			Destinations: dests,
			Err:          conn.ErrSftpUnavailable,
		}, nil
	}

	dests := make([]RelayDest, len(dstSids))

	type liveEntry struct {
		idx  int
		sid  string
		sess *Session
		pr   *io.PipeReader
		pw   *io.PipeWriter
	}
	var live []*liveEntry

	// 预检：解析每个目标、校验 sftp/idle，失败的记入 dests[idx].Err 但不中止其他目标。
	for i, sid := range dstSids {
		dests[i] = RelayDest{DstSid: sid}
		if sid == srcSid {
			dests[i].Err = errors.New("cannot relay a session to itself")
			if onDestDone != nil {
				onDestDone(sid, false, 0, errors.New("cannot relay a session to itself"))
			}
			continue
		}
		ds, gerr := m.Get(sid)
		if gerr != nil {
			dests[i].Err = gerr
			if onDestDone != nil {
				onDestDone(sid, false, 0, gerr)
			}
			continue
		}
		dests[i].DstServer = ds.ServerName()
		if !ds.SftpAvailable() {
			dests[i].Err = conn.ErrSftpUnavailable
			if onDestDone != nil {
				onDestDone(sid, false, 0, conn.ErrSftpUnavailable)
			}
			continue
		}
		if ds.State() != StateIdle {
			dests[i].Err = errors.New("session busy")
			if onDestDone != nil {
				onDestDone(sid, false, 0, errors.New("session busy"))
			}
			continue
		}
		pr, pw := io.Pipe()
		live = append(live, &liveEntry{idx: i, sid: sid, sess: ds, pr: pr, pw: pw})
	}

	if len(live) == 0 {
		return &RelayResult{
			SrcServer:    srcSess.ServerName(),
			Destinations: dests,
			Err:          errors.New("no live relay destinations (all failed pre-flight)"),
		}, nil
	}

	// Stat 源文件：拿 size + 校验普通文件。失败则降级为 Upload（无 size，串行）。
	var size int64 = -1
	useSized := false
	if fi, statErr := srcSess.Stat(srcPath); statErr == nil {
		if !fi.Mode().IsRegular() {
			for _, e := range live {
				e.pw.CloseWithError(errors.New("source not a regular file"))
				dests[e.idx].Err = errors.New("source not a regular file")
				if onDestDone != nil {
					onDestDone(e.sid, false, 0, errors.New("source not a regular file"))
				}
			}
			return &RelayResult{
				SrcServer:    srcSess.ServerName(),
				Destinations: dests,
				Err:          errors.New("source not a regular file"),
			}, nil
		}
		size = fi.Size()
		useSized = true
	}
	// statErr != nil：降级，useSized=false

	// fanWriter 持所有存活目标的 pw
	pws := make([]*io.PipeWriter, len(live))
	for i, e := range live {
		pws[i] = e.pw
	}
	fw := newFanWriter(pws)

	// 包一层 CountingWriter：每次下载侧 Write 累计字节并触发 onDownload。
	// 对 sftp 读侧流水线透明（只包装 dst io.Writer，不改 *sftp.File.WriteTo 并发路径）。
	var dlSink io.Writer = fw
	if onDownload != nil {
		dlSink = &progress.CountingWriter{W: fw, Fn: onDownload}
	}

	// 下载侧 goroutine
	var dlN int
	var dlTimedOut bool
	var dlErr error
	dlDone := make(chan struct{})
	go func() {
		defer close(dlDone)
		dlN, dlTimedOut, dlErr = srcSess.Download(srcPath, dlSink, timeoutMs)
		// 下载结束：关闭所有 pw，让上传侧收尾（nil → EOF，非 nil → 传播错误）
		for _, e := range live {
			e.pw.CloseWithError(dlErr)
		}
	}()

	// 上传侧 N 个 goroutine
	var upWg sync.WaitGroup
	for _, e := range live {
		upWg.Add(1)
		go func(e *liveEntry) {
			defer upWg.Done()
			var n int
			var tOut bool
			var uerr error
			if useSized {
				n, tOut, uerr = e.sess.UploadSized(e.pr, size, dstPath, timeoutMs)
			} else {
				n, tOut, uerr = e.sess.Upload(e.pr, dstPath, timeoutMs)
			}
			// 上传结束：关闭 pr，解除 fanWriter.Write 对 pw 的阻塞（失败隔离）
			e.pr.CloseWithError(uerr)
			dests[e.idx].OK = uerr == nil && !tOut
			dests[e.idx].Bytes = n
			dests[e.idx].TimedOut = tOut
			dests[e.idx].Err = uerr
			if onDestDone != nil {
				onDestDone(e.sid, dests[e.idx].OK, int64(n), uerr)
			}
		}(e)
	}

	<-dlDone
	upWg.Wait()

	// 聚合
	res := &RelayResult{
		DownloadedBytes: dlN,
		TimedOut:        dlTimedOut,
		SrcServer:       srcSess.ServerName(),
		Destinations:    dests,
	}
	// I-1: 顶层 timed_out = 下载超时 || 任一上传超时。
	// dests[i].TimedOut 只反映该目标；顶层需汇总，否则上传侧超时（慢目标磁盘）时
	// 顶层 timed_out=false 而 destinations[i].timed_out=true，与文档"download or any
	// destination timed out"不符，误导调用方。
	for i := range dests {
		if dests[i].TimedOut {
			res.TimedOut = true
		}
	}
	allOK := dlErr == nil && !dlTimedOut
	for i := range dests {
		if !dests[i].OK {
			allOK = false
		}
	}
	// 已知 size 时校验字节一致
	if allOK && useSized {
		if dlN != int(size) {
			allOK = false
		}
		for i := range dests {
			if dests[i].Bytes != int(size) {
				dests[i].OK = false
				allOK = false
			}
		}
	}
	if !allOK {
		root := dlErr
		if root == nil {
			root = errors.New("one or more relay destinations failed")
		}
		res.Err = root
	}
	return res, nil
}
