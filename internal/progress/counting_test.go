package progress

import (
	"bytes"
	"io"
	"testing"
)

func TestCountingReaderCumulative(t *testing.T) {
	var seen []int64
	r := &CountingReader{R: bytes.NewReader([]byte("hello world")), Fn: func(n int64) { seen = append(seen, n) }}
	buf := make([]byte, 5)
	n, err := r.Read(buf)
	if err != nil || n != 5 || string(buf) != "hello" {
		t.Fatalf("Read = (%d, %v, %q), want (5, nil, hello)", n, err, buf)
	}
	n2, _ := r.Read(buf)
	if seen[len(seen)-1] != int64(n+n2) {
		t.Errorf("last cumulative = %d, want %d", seen[len(seen)-1], n+n2)
	}
}

func TestCountingReaderNilFnNoPanic(t *testing.T) {
	r := &CountingReader{R: bytes.NewReader([]byte("x"))}
	if _, err := r.Read(make([]byte, 1)); err != nil {
		t.Fatalf("Read with nil Fn: %v", err)
	}
}

func TestCountingWriterCumulative(t *testing.T) {
	var seen []int64
	var dst bytes.Buffer
	w := &CountingWriter{W: &dst, Fn: func(n int64) { seen = append(seen, n) }}
	n, err := w.Write([]byte("abc"))
	if err != nil || n != 3 || dst.String() != "abc" {
		t.Fatalf("Write = (%d, %v, %q), want (3, nil, abc)", n, err, dst.String())
	}
	w.Write([]byte("de"))
	if seen[len(seen)-1] != 5 {
		t.Errorf("cumulative after 2nd write = %d, want 5", seen[len(seen)-1])
	}
}

func TestCountingWriterPassesUnderlyingWriteError(t *testing.T) {
	w := &CountingWriter{W: &errWriter{}, Fn: func(int64) {}}
	if _, err := w.Write([]byte("x")); err != errSentinel {
		t.Fatalf("expected underlying error to pass through, got %v", err)
	}
}

type errWriter struct{}

var errSentinel = io.ErrShortWrite

func (*errWriter) Write([]byte) (int, error) { return 0, errSentinel }
