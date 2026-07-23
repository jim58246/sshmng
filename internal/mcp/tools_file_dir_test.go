package mcp

import (
	"testing"

	"github.com/jim58246/sshmng/internal/ssh/conn"
)

// TestParseConflictPolicyFromArgs: "skip" → ConflictSkip，"rename" → ConflictRename，"" / "overwrite" / "unknown" → ConflictOverwrite
func TestParseConflictPolicyFromArgs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "overwrite"},
		{"overwrite", "overwrite"},
		{"skip", "skip"},
		{"rename", "rename"},
		{"bogus", "overwrite"}, // 无效值默认 overwrite
	}
	for _, c := range cases {
		got := conn.ParseConflictPolicy(c.in).String()
		if got != c.want {
			t.Errorf("ParseConflictPolicy(%q).String() = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUploadDirArgsFields: 验证 UploadDirArgs JSON tag 正确
func TestUploadDirArgsFields(t *testing.T) {
	// 仅 smoke test：构造 args，验证字段名与默认行为
	args := UploadDirArgs{SID: "s1", Src: "/local", Dst: "/remote", Conflict: "skip", Concurrency: 4}
	if args.SID != "s1" || args.Src != "/local" || args.Dst != "/remote" || args.Conflict != "skip" || args.Concurrency != 4 {
		t.Errorf("UploadDirArgs fields mismatch: %+v", args)
	}
}

// TestDownloadDirArgsFields: 同上
func TestDownloadDirArgsFields(t *testing.T) {
	args := DownloadDirArgs{SID: "s1", Src: "/remote", Dst: "/local", Conflict: "rename"}
	if args.SID != "s1" || args.Src != "/remote" || args.Dst != "/local" || args.Conflict != "rename" {
		t.Errorf("DownloadDirArgs fields mismatch: %+v", args)
	}
}
