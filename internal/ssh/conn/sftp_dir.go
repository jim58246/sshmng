package conn

// ConflictPolicy 定义目标文件已存在时的处理策略。
type ConflictPolicy int

const (
	// ConflictOverwrite 覆盖目标文件（sftp.Create / os.Create 语义，truncate）。
	ConflictOverwrite ConflictPolicy = iota
	// ConflictSkip 跳过已存在的目标文件，不计入失败。
	ConflictSkip
	// ConflictRename 自动重命名源文件为 name_1、name_2... 直到无冲突。
	ConflictRename
)

// String 把 ConflictPolicy 转为字符串，用于 MCP 工具 args 解析与日志。
func (c ConflictPolicy) String() string {
	switch c {
	case ConflictOverwrite:
		return "overwrite"
	case ConflictSkip:
		return "skip"
	case ConflictRename:
		return "rename"
	}
	return "unknown"
}

// ParseConflictPolicy 把字符串转为 ConflictPolicy，无效值默认 ConflictOverwrite。
func ParseConflictPolicy(s string) ConflictPolicy {
	switch s {
	case "skip":
		return ConflictSkip
	case "rename":
		return ConflictRename
	case "", "overwrite":
		fallthrough
	default:
		return ConflictOverwrite
	}
}

// DirTransferOptions 是文件夹传输的选项。
type DirTransferOptions struct {
	Conflict    ConflictPolicy // 0 = ConflictOverwrite
	Concurrency int            // 0 = 默认 4
	TimeoutMs   int            // 0 = 默认 300000（300s），per file
}

// DirTransferResult 是文件夹传输的汇总结果。
type DirTransferResult struct {
	Bytes    int64 // 成功传输的字节总数
	Files    int   // 成功传输的文件数
	Skipped  int   // 因 ConflictSkip 跳过的文件数
	Renamed  int   // 因 ConflictRename 重命名的文件数
	TimedOut int   // per-file 超时的文件数
}
