package session

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// TestFanWriterBroadcastsToAll: 数据块并发分发到所有存活目标，全部收到完整内容。
func TestFanWriterBroadcastsToAll(t *testing.T) {
	pr1, pw1 := io.Pipe()
	pr2, pw2 := io.Pipe()
	defer pr1.Close()
	defer pr2.Close()

	fw := newFanWriter([]*io.PipeWriter{pw1, pw2})

	var got1, got2 bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(&got1, pr1) }()
	go func() { defer wg.Done(); io.Copy(&got2, pr2) }()

	if _, err := fw.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := fw.Write([]byte("world")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	pw1.Close()
	pw2.Close()
	wg.Wait()

	if got1.String() != "hello world" {
		t.Errorf("dest1 got %q, want %q", got1.String(), "hello world")
	}
	if got2.String() != "hello world" {
		t.Errorf("dest2 got %q, want %q", got2.String(), "hello world")
	}
}

// TestFanWriterIsolatesDeadDest: 一个目标关闭后标记 dead，其余目标仍收完整内容，
// Write 返回 (len(p), nil)。
func TestFanWriterIsolatesDeadDest(t *testing.T) {
	pr1, pw1 := io.Pipe()
	pr2, pw2 := io.Pipe()
	defer pr1.Close()
	defer pr2.Close()

	fw := newFanWriter([]*io.PipeWriter{pw1, pw2})

	// dest1 读几个字节后关闭 reader → 后续 Write 到 pw1 返回 ErrClosedPipe
	var got1 bytes.Buffer
	go func() {
		io.CopyN(&got1, pr1, 3)
		pr1.Close() // 关闭 reader 端，让 pw1.Write 失败
	}()

	var got2 bytes.Buffer
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() { defer wg.Done(); io.Copy(&got2, pr2) }()

	n, err := fw.Write([]byte("abc")) // dest1 读到 "abc" 后关闭
	if err != nil || n != 3 {
		t.Errorf("Write 1 = (%d, %v), want (3, nil)", n, err)
	}
	n, err = fw.Write([]byte("def")) // dest1 dead，只写 dest2
	if err != nil || n != 3 {
		t.Errorf("Write 2 = (%d, %v), want (3, nil)", n, err)
	}
	pw1.Close()
	pw2.Close()
	wg.Wait()

	if got2.String() != "abcdef" {
		t.Errorf("dest2 got %q, want %q", got2.String(), "abcdef")
	}
}

// TestFanWriterAllDeadReturnsError: 全部目标 dead 后 Write 返回 errAllDestinationsFailed。
// 关闭两个 pipe 的 reader 端后，pw.Write 立即返回 io.ErrClosedPipe；两个 slot 都 dead，
// Write 返回 errAllDestinationsFailed，让 Download 的 io.Copy 提前终止。
func TestFanWriterAllDeadReturnsError(t *testing.T) {
	prA, pwA := io.Pipe()
	prB, pwB := io.Pipe()
	prA.Close() // reader 关闭 → pwA.Write 立即返回 ErrClosedPipe
	prB.Close()
	defer pwA.Close()
	defer pwB.Close()

	fw := newFanWriter([]*io.PipeWriter{pwA, pwB})
	_, err := fw.Write([]byte("x"))
	if !errors.Is(err, errAllDestinationsFailed) {
		t.Errorf("Write err = %v, want errAllDestinationsFailed", err)
	}
}

// relayTransferTestHarness 建一个源 session + N 个目标 session，全部用 fakeConn。
// srcData 是源文件内容；srcSize 用于配置 fakeConn.statFi。
type relayTransferTestHarness struct {
	mgr     *Manager
	srcSid  string
	srcConn *fakeConn
	dst     []struct {
		sid  string
		conn *fakeConn
	}
}

func newRelayHarness(t *testing.T, nDst int, srcData []byte) *relayTransferTestHarness {
	t.Helper()
	mgr := NewManager()
	srcConn := newFakeConn()
	srcConn.sftpEnabled = true
	srcConn.downloadData = srcData
	srcConn.statFi = fakeFileInfo{size: int64(len(srcData)), mode: 0644}
	mgr.newSessionWithConn("src", "srcsrv", srcConn, time.Minute, nil)
	h := &relayTransferTestHarness{mgr: mgr, srcSid: "src", srcConn: srcConn}
	for i := 0; i < nDst; i++ {
		dc := newFakeConn()
		dc.sftpEnabled = true
		sid := "dst" + string(rune('1'+i))
		mgr.newSessionWithConn(sid, "dstsrv", dc, time.Minute, nil)
		h.dst = append(h.dst, struct {
			sid  string
			conn *fakeConn
		}{sid, dc})
	}
	return h
}

// TestRelayTransferOneToOne: 1:1 流式中转，字节一致、两侧 ok。
func TestRelayTransferOneToOne(t *testing.T) {
	data := bytes.Repeat([]byte("relay\n"), 300) // 1800 bytes
	h := newRelayHarness(t, 1, data)
	dstSid := h.dst[0].sid

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", []string{dstSid}, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("RelayTransfer: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v, want nil", res.Err)
	}
	if res.DownloadedBytes != len(data) {
		t.Errorf("downloaded_bytes = %d, want %d", res.DownloadedBytes, len(data))
	}
	if len(res.Destinations) != 1 {
		t.Fatalf("destinations len = %d, want 1", len(res.Destinations))
	}
	d := res.Destinations[0]
	if !d.OK {
		t.Errorf("dest ok = false, want true; err=%v", d.Err)
	}
	if d.Bytes != len(data) {
		t.Errorf("dest bytes = %d, want %d", d.Bytes, len(data))
	}
	if !bytes.Equal(h.dst[0].conn.uploadedBytes, data) {
		t.Errorf("dest uploaded content mismatch: got %d bytes, want %d", len(h.dst[0].conn.uploadedBytes), len(data))
	}
}

// TestRelayTransferOneToN: 1:3 扇出，源只读一次，三个目标各得完整内容。
func TestRelayTransferOneToN(t *testing.T) {
	data := bytes.Repeat([]byte("fan\n"), 400) // 1600 bytes
	h := newRelayHarness(t, 3, data)
	sids := []string{h.dst[0].sid, h.dst[1].sid, h.dst[2].sid}

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", sids, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("RelayTransfer: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v", res.Err)
	}
	for i, d := range res.Destinations {
		if !d.OK {
			t.Errorf("dest %d ok=false, err=%v", i, d.Err)
		}
		if d.Bytes != len(data) {
			t.Errorf("dest %d bytes = %d, want %d", i, d.Bytes, len(data))
		}
		if !bytes.Equal(h.dst[i].conn.uploadedBytes, data) {
			t.Errorf("dest %d content mismatch", i)
		}
	}
}

// TestRelayTransferSourceMissing: 源文件不存在（download 出错）→ ok=false，根因清晰。
func TestRelayTransferSourceMissing(t *testing.T) {
	h := newRelayHarness(t, 1, []byte("x"))
	h.srcConn.downloadErr = errors.New("open remote /src.bin: no such file")
	dstSid := h.dst[0].sid

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", []string{dstSid}, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if res.Err == nil {
		t.Fatalf("res.Err = nil, want download error")
	}
	if res.Destinations[0].OK {
		t.Errorf("dest ok = true, want false (source missing)")
	}
}

// TestRelayTransferOneDestFailsOthersSucceed: 一个目标上传失败，其余仍 ok。
func TestRelayTransferOneDestFailsOthersSucceed(t *testing.T) {
	data := bytes.Repeat([]byte("iso\n"), 500) // 2000 bytes
	h := newRelayHarness(t, 3, data)
	// dst2 上传失败：设 uploadErr
	h.dst[1].conn.uploadErr = errors.New("create remote: permission denied")
	sids := []string{h.dst[0].sid, h.dst[1].sid, h.dst[2].sid}

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", sids, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if res.Err == nil {
		t.Fatalf("res.Err = nil, want failure (one dest failed)")
	}
	if res.Destinations[0].OK != true {
		t.Errorf("dest0 ok = false, want true")
	}
	if res.Destinations[1].OK != false {
		t.Errorf("dest1 ok = true, want false")
	}
	if res.Destinations[2].OK != true {
		t.Errorf("dest2 ok = false, want true")
	}
}

// TestRelayTransferSrcEqualsDst: src_sid == dst_sid → 该 dest 报错 "cannot relay to itself"。
func TestRelayTransferSrcEqualsDst(t *testing.T) {
	h := newRelayHarness(t, 1, []byte("x"))
	res, err := h.mgr.RelayTransfer(h.srcSid, "/s", []string{h.srcSid}, "/d", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if res.Destinations[0].Err == nil || !strings.Contains(res.Destinations[0].Err.Error(), "itself") {
		t.Errorf("dest err = %v, want 'cannot relay a session to itself'", res.Destinations[0].Err)
	}
}

// TestRelayTransferEmptyDsts: 空 dst_sids → 硬错误。
func TestRelayTransferEmptyDsts(t *testing.T) {
	h := newRelayHarness(t, 0, []byte("x"))
	_, err := h.mgr.RelayTransfer(h.srcSid, "/s", nil, "/d", 0)
	if err == nil || !strings.Contains(err.Error(), "no relay destinations") {
		t.Errorf("err = %v, want 'no relay destinations'", err)
	}
}

// TestRelayTransferStatFallback: Stat 失败时降级为 Upload（无 size），仍完成传输。
func TestRelayTransferStatFallback(t *testing.T) {
	data := bytes.Repeat([]byte("fb\n"), 300)
	h := newRelayHarness(t, 1, data)
	h.srcConn.statErr = errors.New("stat: permission denied") // 触发降级
	dstSid := h.dst[0].sid

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", []string{dstSid}, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("RelayTransfer: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v, want nil (fallback should succeed)", res.Err)
	}
	if !res.Destinations[0].OK {
		t.Errorf("dest ok=false, err=%v", res.Destinations[0].Err)
	}
	if !bytes.Equal(h.dst[0].conn.uploadedBytes, data) {
		t.Errorf("content mismatch after stat fallback")
	}
}

// setSftpAvail 直接改 session 的缓存 sftpAvail 字段（同包测试可访问）。
// SftpAvailable() 读的是 s.sftpAvail（创建时从 conn.SftpAvailable() 快照），
// 故创建后改 conn.sftpEnabled 不影响 pre-flight；需直接改缓存值。调用前无 goroutine 运行。
func setSftpAvail(t *testing.T, sess *Session, avail bool) {
	t.Helper()
	sess.mu.Lock()
	sess.sftpAvail = avail
	sess.mu.Unlock()
}

// TestRelayTransferDownloadTimeout: 下载侧超时 → res.TimedOut=true、res.Err!=nil、ok=false。
// fakeConn.Download 在 uploadDelay > timeoutMs 时模拟真实超时返回 (0, true, ctx.Err)。
func TestRelayTransferDownloadTimeout(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 500)
	h := newRelayHarness(t, 1, data)
	// 源下载慢：delay 200ms > timeoutMs 50ms → 触发下载侧超时
	h.srcConn.uploadDelay = 200 * time.Millisecond
	dstSid := h.dst[0].sid

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", []string{dstSid}, "/dst.bin", 50)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("res.TimedOut = false, want true (download timed out)")
	}
	if res.Err == nil {
		t.Errorf("res.Err = nil, want timeout error")
	}
	if res.DownloadedBytes != 0 {
		t.Errorf("downloaded_bytes = %d, want 0 (timed out before write)", res.DownloadedBytes)
	}
	if res.Destinations[0].OK {
		t.Errorf("dest ok = true, want false (download timed out)")
	}
}

// TestRelayTransferUploadTimeoutTopLevel: 一个目标上传超时（其余成功）→ 顶层 timed_out=true。
// 这是 I-1 的回归测试：顶层 timed_out 必须 OR 入 dests[i].TimedOut，不能只看下载侧。
// 下载侧不超时（srcConn 无 delay）；仅 dst1 上传超时。
func TestRelayTransferUploadTimeoutTopLevel(t *testing.T) {
	data := bytes.Repeat([]byte("u"), 500)
	h := newRelayHarness(t, 3, data)
	// dst1 上传慢：delay 200ms > timeoutMs 50ms → 上传侧超时；其余目标正常
	h.dst[1].conn.uploadDelay = 200 * time.Millisecond
	sids := []string{h.dst[0].sid, h.dst[1].sid, h.dst[2].sid}

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", sids, "/dst.bin", 50)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	// I-1 核心：顶层 timed_out 必须为 true（上传侧超时 OR 入顶层）
	if !res.TimedOut {
		t.Errorf("res.TimedOut = false, want true (one dest upload timed out — I-1 regression)")
	}
	// 超时目标自身
	if !res.Destinations[1].TimedOut {
		t.Errorf("dest1 timed_out = false, want true")
	}
	if res.Destinations[1].OK {
		t.Errorf("dest1 ok = true, want false (timed out)")
	}
	// 其余目标仍成功
	if !res.Destinations[0].OK {
		t.Errorf("dest0 ok = false, want true; err=%v", res.Destinations[0].Err)
	}
	if !res.Destinations[2].OK {
		t.Errorf("dest2 ok = false, want true; err=%v", res.Destinations[2].Err)
	}
	if res.Err == nil {
		t.Errorf("res.Err = nil, want failure (one dest timed out)")
	}
}

// TestRelayTransferDestSftpUnavailable: 一个目标 sftp 未启用 → 该目标 pre-flight 失败、
// 其余目标仍成功、整体 ok=false。
func TestRelayTransferDestSftpUnavailable(t *testing.T) {
	data := bytes.Repeat([]byte("s"), 500)
	h := newRelayHarness(t, 3, data)
	// dst1 sftp 不可用：改 session 缓存的 sftpAvail（创建后改 conn 字段无效）
	ds1, _ := h.mgr.Get(h.dst[1].sid)
	setSftpAvail(t, ds1, false)
	sids := []string{h.dst[0].sid, h.dst[1].sid, h.dst[2].sid}

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", sids, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if res.Destinations[1].OK {
		t.Errorf("dest1 ok = true, want false (sftp unavailable)")
	}
	if res.Destinations[1].Err == nil || !strings.Contains(res.Destinations[1].Err.Error(), "sftp") {
		t.Errorf("dest1 err = %v, want sftp-unavailable error", res.Destinations[1].Err)
	}
	if !res.Destinations[0].OK {
		t.Errorf("dest0 ok = false, want true; err=%v", res.Destinations[0].Err)
	}
	if !res.Destinations[2].OK {
		t.Errorf("dest2 ok = false, want true; err=%v", res.Destinations[2].Err)
	}
	if res.Err == nil {
		t.Errorf("res.Err = nil, want failure (one dest sftp-unavailable)")
	}
}

// TestRelayTransferSourceNotRegular: 源是目录（非普通文件）→ res.Err!=nil、无传输、ok=false。
func TestRelayTransferSourceNotRegular(t *testing.T) {
	data := []byte("dir-data")
	h := newRelayHarness(t, 2, data)
	// 源 stat 返回目录（IsRegular()=false）
	h.srcConn.statFi = fakeFileInfo{size: 0, mode: os.ModeDir | 0755}
	sids := []string{h.dst[0].sid, h.dst[1].sid}

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", sids, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "regular") {
		t.Errorf("res.Err = %v, want 'source not a regular file'", res.Err)
	}
	for i, d := range res.Destinations {
		if d.OK {
			t.Errorf("dest %d ok = true, want false", i)
		}
		if d.Bytes != 0 {
			t.Errorf("dest %d bytes = %d, want 0 (no transfer)", i, d.Bytes)
		}
	}
	for i, d := range h.dst {
		if len(d.conn.uploadedBytes) != 0 {
			t.Errorf("dest %d uploaded %d bytes, want 0 (no transfer spawned)", i, len(d.conn.uploadedBytes))
		}
	}
}

// TestRelayTransferWithProgressCallbacks: 1:2 relay, asserts onDownload fires
// with nonzero cumulative byte counts and onDestDone fires once per destination
// with ok=true. Reuses newRelayHarness (fakeConn) from the existing relay tests.
func TestRelayTransferWithProgressCallbacks(t *testing.T) {
	data := bytes.Repeat([]byte("prog\n"), 400) // 2000 bytes
	h := newRelayHarness(t, 2, data)
	sids := []string{h.dst[0].sid, h.dst[1].sid}

	var mu sync.Mutex
	var dlCounts []int64
	type destDoneEvent struct {
		sid string
		ok  bool
		n   int64
		err error
	}
	var destDone []destDoneEvent
	onDownload := func(n int64) {
		mu.Lock()
		dlCounts = append(dlCounts, n)
		mu.Unlock()
	}
	onDestDone := func(sid string, ok bool, n int64, err error) {
		mu.Lock()
		destDone = append(destDone, destDoneEvent{sid: sid, ok: ok, n: n, err: err})
		mu.Unlock()
	}

	res, err := h.mgr.RelayTransferWithProgress(h.srcSid, "/src.bin", sids, "/dst.bin", 0, onDownload, onDestDone)
	if err != nil {
		t.Fatalf("RelayTransferWithProgress: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v, want nil", res.Err)
	}

	mu.Lock()
	defer mu.Unlock()

	// onDownload: at least one nonzero cumulative byte count, and the final
	// cumulative must equal the full data length (barrier model: download
	// completes with all bytes flushed through fanWriter).
	nonzero := false
	for _, c := range dlCounts {
		if c > 0 {
			nonzero = true
		}
	}
	if !nonzero {
		t.Errorf("onDownload never received a nonzero byte count; got %v", dlCounts)
	}
	if len(dlCounts) == 0 {
		t.Fatalf("onDownload never invoked")
	}
	if last := dlCounts[len(dlCounts)-1]; last != int64(len(data)) {
		t.Errorf("last onDownload cumulative = %d, want %d", last, len(data))
	}

	// onDestDone: exactly len(sids) calls, all ok=true, each with full byte count.
	if len(destDone) != len(sids) {
		t.Fatalf("onDestDone called %d times, want %d", len(destDone), len(sids))
	}
	okCount := 0
	for _, d := range destDone {
		if d.ok {
			okCount++
		}
		if d.n != int64(len(data)) {
			t.Errorf("onDestDone(%q) bytes = %d, want %d", d.sid, d.n, len(data))
		}
	}
	if okCount != len(sids) {
		t.Errorf("onDestDone ok count = %d, want %d", okCount, len(sids))
	}
}

// TestRelayTransferWithProgressNilCallbacks: nil callbacks must not panic.
// Also a delegation regression guard: RelayTransfer delegates to
// RelayTransferWithProgress(nil, nil), so this exercises the nil path.
func TestRelayTransferWithProgressNilCallbacks(t *testing.T) {
	data := bytes.Repeat([]byte("nil\n"), 300)
	h := newRelayHarness(t, 1, data)
	dstSid := h.dst[0].sid

	res, err := h.mgr.RelayTransferWithProgress(h.srcSid, "/src.bin", []string{dstSid}, "/dst.bin", 0, nil, nil)
	if err != nil {
		t.Fatalf("RelayTransferWithProgress: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("res.Err = %v, want nil", res.Err)
	}
	if !res.Destinations[0].OK {
		t.Errorf("dest ok=false, err=%v", res.Destinations[0].Err)
	}
	if !bytes.Equal(h.dst[0].conn.uploadedBytes, data) {
		t.Errorf("content mismatch with nil callbacks")
	}
}

// TestRelayTransferWithProgressPreFlightFailure: a pre-flight failure (dest sftp
// unavailable) must still fire onDestDone for that dest with ok=false, and the
// successful dest fires with ok=true.
func TestRelayTransferWithProgressPreFlightFailure(t *testing.T) {
	data := bytes.Repeat([]byte("pf\n"), 300)
	h := newRelayHarness(t, 2, data)
	ds1, _ := h.mgr.Get(h.dst[1].sid)
	setSftpAvail(t, ds1, false)
	sids := []string{h.dst[0].sid, h.dst[1].sid}

	var mu sync.Mutex
	type destDoneEvent struct {
		sid string
		ok  bool
	}
	var destDone []destDoneEvent
	onDestDone := func(sid string, ok bool, n int64, err error) {
		mu.Lock()
		destDone = append(destDone, destDoneEvent{sid: sid, ok: ok})
		mu.Unlock()
	}

	res, err := h.mgr.RelayTransferWithProgress(h.srcSid, "/src.bin", sids, "/dst.bin", 0, nil, onDestDone)
	if err != nil {
		t.Fatalf("RelayTransferWithProgress: %v", err)
	}
	if res.Err == nil {
		t.Fatalf("res.Err = nil, want failure (one dest sftp unavailable)")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(destDone) != len(sids) {
		t.Fatalf("onDestDone called %d times, want %d", len(destDone), len(sids))
	}
	bySid := map[string]bool{}
	for _, d := range destDone {
		bySid[d.sid] = d.ok
	}
	if !bySid[h.dst[0].sid] {
		t.Errorf("dest0 onDestDone ok=false, want true")
	}
	if bySid[h.dst[1].sid] {
		t.Errorf("dest1 onDestDone ok=true, want false (sftp unavailable)")
	}
}

// TestRelayTransferSourceSftpUnavailable: 源 sftp 不可用 → M-2 pre-flight 直接返回，
// res.Err=ErrSftpUnavailable、每个目标记 per-dest 错误、不 spawn 任何传输 goroutine。
func TestRelayTransferSourceSftpUnavailable(t *testing.T) {
	data := []byte("x")
	h := newRelayHarness(t, 2, data)
	// 源 sftp 不可用：改 session 缓存的 sftpAvail
	srcSess, _ := h.mgr.Get(h.srcSid)
	setSftpAvail(t, srcSess, false)
	sids := []string{h.dst[0].sid, h.dst[1].sid}

	res, err := h.mgr.RelayTransfer(h.srcSid, "/src.bin", sids, "/dst.bin", 0)
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if !errors.Is(res.Err, conn.ErrSftpUnavailable) {
		t.Errorf("res.Err = %v, want conn.ErrSftpUnavailable", res.Err)
	}
	if len(res.Destinations) != 2 {
		t.Fatalf("destinations len = %d, want 2", len(res.Destinations))
	}
	for i, d := range res.Destinations {
		if d.OK {
			t.Errorf("dest %d ok = true, want false", i)
		}
		if d.Err == nil || !strings.Contains(d.Err.Error(), "source sftp unavailable") {
			t.Errorf("dest %d err = %v, want 'source sftp unavailable'", i, d.Err)
		}
	}
	// M-2 核心收益：不 spawn 传输 goroutine，目标端零字节
	for i, d := range h.dst {
		if len(d.conn.uploadedBytes) != 0 {
			t.Errorf("dest %d uploaded %d bytes, want 0 (no transfer spawned)", i, len(d.conn.uploadedBytes))
		}
	}
}
