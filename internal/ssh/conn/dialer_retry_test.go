package conn

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"syscall"
	"testing"
	"time"
)

// fakeDialConn 只占位满足 net.Conn 接口；测试不调用其方法。
type fakeDialConn struct{ net.Conn }

// withShortEDRDelay 临时缩短 EDR 重试间隔，测试结束恢复。
func withShortEDRDelay(t *testing.T) {
	t.Helper()
	orig := edrRetryDelay
	edrRetryDelay = 10 * time.Microsecond
	t.Cleanup(func() { edrRetryDelay = orig })
}

func TestIsLocalPolicyReject(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"wsaeaccess bare", syscall.Errno(10013), true},
		{"wsaeaccess wrapped", fmt.Errorf("dial proxy 10.0.0.1:8080: connectex: %w", syscall.Errno(10013)), true},
		{"connrefused", syscall.ECONNREFUSED, false},
		{"generic", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := isLocalPolicyReject(tc.err); got != tc.want {
			t.Errorf("%s: isLocalPolicyReject = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDialTCPWithRetry(t *testing.T) {
	withShortEDRDelay(t)
	logger := slog.New(slog.DiscardHandler)

	t.Run("success first attempt, no retry", func(t *testing.T) {
		var attempts int
		conn, err := dialTCPWithRetry(func(addr string, timeout time.Duration) (net.Conn, error) {
			attempts++
			return fakeDialConn{}, nil
		}, "1.2.3.4:22", time.Second, logger)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if conn == nil {
			t.Fatal("conn = nil")
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1", attempts)
		}
	})

	t.Run("wsaeaccess once then success", func(t *testing.T) {
		var attempts int
		conn, err := dialTCPWithRetry(func(addr string, timeout time.Duration) (net.Conn, error) {
			attempts++
			if attempts == 1 {
				return nil, fmt.Errorf("dial tcp 10.0.0.1:8080: connectex: %w", syscall.Errno(10013))
			}
			return fakeDialConn{}, nil
		}, "10.0.0.1:8080", time.Second, logger)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if conn == nil {
			t.Fatal("conn = nil")
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2", attempts)
		}
	})

	t.Run("wsaeaccess exhausts retries", func(t *testing.T) {
		var attempts int
		_, err := dialTCPWithRetry(func(addr string, timeout time.Duration) (net.Conn, error) {
			attempts++
			return nil, fmt.Errorf("dial tcp 10.0.0.1:8080: connectex: %w", syscall.Errno(10013))
		}, "10.0.0.1:8080", time.Second, logger)
		if err == nil {
			t.Fatal("err = nil, want wsaeaccess")
		}
		if attempts != 1+edrMaxRetries {
			t.Fatalf("attempts = %d, want %d", attempts, 1+edrMaxRetries)
		}
		if !errors.Is(err, syscall.Errno(10013)) {
			t.Fatalf("err = %v, want wrapped WSAEACCES", err)
		}
	})

	t.Run("non-policy error not retried", func(t *testing.T) {
		var attempts int
		_, err := dialTCPWithRetry(func(addr string, timeout time.Duration) (net.Conn, error) {
			attempts++
			return nil, fmt.Errorf("dial tcp 10.0.0.1:22: connect: %w", syscall.ECONNREFUSED)
		}, "10.0.0.1:22", time.Second, logger)
		if err == nil {
			t.Fatal("err = nil, want connrefused")
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (no retry on non-policy error)", attempts)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("err = %v, want wrapped ECONNREFUSED", err)
		}
	})

	t.Run("policy then non-policy stops at non-policy", func(t *testing.T) {
		var attempts int
		_, err := dialTCPWithRetry(func(addr string, timeout time.Duration) (net.Conn, error) {
			attempts++
			if attempts == 1 {
				return nil, fmt.Errorf("dial tcp 10.0.0.1:8080: connectex: %w", syscall.Errno(10013))
			}
			return nil, fmt.Errorf("dial tcp 10.0.0.1:8080: connect: %w", syscall.ECONNREFUSED)
		}, "10.0.0.1:8080", time.Second, logger)
		if err == nil {
			t.Fatal("err = nil, want connrefused")
		}
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2", attempts)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("err = %v, want wrapped ECONNREFUSED", err)
		}
	})
}
