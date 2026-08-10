package progress

import "io"

// CountingReader wraps an io.Reader, invoking Fn with the cumulative byte count
// after each Read. Fn==nil is allowed (no-op). Used to feed Bar.SetBytes during
// uploads without touching the underlying reader's method set (preserves sftp
// pipelining trigger on *io.LimitReader).
type CountingReader struct {
	R  io.Reader
	n  int64
	Fn func(int64)
}

func (c *CountingReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	if n > 0 {
		c.n += int64(n)
		if c.Fn != nil {
			c.Fn(c.n)
		}
	}
	return n, err
}

// CountingWriter wraps an io.Writer, invoking Fn with the cumulative byte count
// after each Write. Fn==nil is allowed.
type CountingWriter struct {
	W  io.Writer
	n  int64
	Fn func(int64)
}

func (c *CountingWriter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	if n > 0 {
		c.n += int64(n)
		if c.Fn != nil {
			c.Fn(c.n)
		}
	}
	return n, err
}
