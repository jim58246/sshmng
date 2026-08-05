package session

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
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
