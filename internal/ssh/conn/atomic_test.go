package conn

import (
	"strings"
	"testing"
)

func TestAtomicRemotePath(t *testing.T) {
	got := AtomicRemotePath("/root/abc.txt")
	if !strings.HasPrefix(got, "/root/abc.txt.sshmng-tmp-") {
		t.Errorf("AtomicRemotePath(%q) = %q, want prefix %q", "/root/abc.txt", got, "/root/abc.txt.sshmng-tmp-")
	}
	// 6 hex chars after the dash.
	suffix := got[len("/root/abc.txt.sshmng-tmp-"):]
	if len(suffix) != 6 {
		t.Errorf("random suffix len = %d, want 6 (%q)", len(suffix), suffix)
	}
	for _, c := range suffix {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("random suffix %q must be hex", suffix)
		}
	}
	// Different calls produce different paths (randomness).
	a, b := AtomicRemotePath("/x"), AtomicRemotePath("/x")
	if a == b {
		t.Errorf("two calls identical: %q == %q (randomness broken)", a, b)
	}
}
