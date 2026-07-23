package mcp

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/jim58246/sshmng/internal/version"
)

// TestSessionLoggerReturnsBaseLoggerWithSid 验证 sessionLogger 始终返回 baseLogger
// （走 config.log_path 指定的文件或 io.Discard），不走 MCP notifications/message 通道。
//
// 修复前：req.Session 非 nil 时返回 mcp.NewLoggingHandler 包裹的 logger，
// 日志同步写 stdout JSON-RPC 通道，堵塞 tool result + 污染 Agent 上下文。
// 修复后：始终返回 baseLogger.With("sid", sid)，日志走文件（或 discard）。
func TestSessionLoggerReturnsBaseLoggerWithSid(t *testing.T) {
	var buf bytes.Buffer
	baseLogger := slog.New(slog.NewTextHandler(&buf, nil))
	svc := &Service{baseLogger: baseLogger}

	logger := svc.sessionLogger(&mcp.CallToolRequest{}, "sid123")
	logger.Info("test message")

	out := buf.String()
	if !strings.Contains(out, "test message") {
		t.Errorf("expected log in stderr buffer, got: %s", out)
	}
	if !strings.Contains(out, "sid=sid123") {
		t.Errorf("expected sid=sid123 in log, got: %s", out)
	}

	// handler 不应是 *mcp.LoggingHandler（修复前 req.Session 非 nil 时会是）
	if _, ok := logger.Handler().(*mcp.LoggingHandler); ok {
		t.Errorf("sessionLogger should not return *mcp.LoggingHandler (must use baseLogger/stderr)")
	}
}

// TestNewServerSetsInstructions 验证 NewServer 把 serverInstructions 传给 MCP server，
// 使 client（Agent）在 initialize 响应中能看到工作流 / session 生命周期 / 失败诊断路径。
//
// 缺失 Instructions 时 Agent 只能从各 tool description 拼凑工作流，容易漏掉
// "失败时调 get_trace"、"session 复用"、"idle timeout" 等关键约束。
//
// 通过 InMemoryTransport 连 client + server，读 ClientSession.InitializeResult()
// 拿到真实的 initialize 响应——这是 Agent 实际看到的内容。
func TestNewServerSetsInstructions(t *testing.T) {
	svc := &Service{}
	server := NewServer(svc)

	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	clientSession, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()

	result := clientSession.InitializeResult()
	if result == nil {
		t.Fatalf("InitializeResult() = nil; initialize not completed")
	}
	got := result.Instructions
	if got == "" {
		t.Fatalf("Instructions = empty; NewServer must set serverInstructions")
	}

	// 关键工作流关键词必须出现——这些是 Agent 正确使用 MCP 的最小集合。
	wantKeywords := []string{
		"login",           // 入口工具
		"run_in_session",  // 核心工具
		"close_session",   // 收尾
		"get_trace",       // 失败诊断
		"stat",            // 状态检查
		"ssh_j",           // jumphost 两种形态的关键字段
		"login_flow",      // LoginFlow 语义
		"idle timeout",    // 生命周期
		"loginflow error", // 失败恢复路径
		"raw_output",      // 诊断字段
		"NOPASSWD",        // 安全建议
	}
	for _, kw := range wantKeywords {
		if !strings.Contains(got, kw) {
			t.Errorf("Instructions missing keyword %q\n--- instructions ---\n%s", kw, got)
		}
	}
}

// TestNewServerReportsVersion 验证 NewServer 把 version.Version 传给 MCP server，
// 使 Agent 在 initialize 响应的 serverInfo.version 中看到真实构建版本
// （goreleaser 通过 ldflags 注入；dev 构建为 "dev"）。
//
// 修复前：server.go 硬编码 Version: "v1"，与 git tag 脱节，self-update 无法
// 比对当前版本，Agent 也看不到真实版本号。
// 修复后：Version: version.Version。
//
// 临时覆盖 version.Version 后通过 InMemoryTransport 跑真实 initialize 握手，
// 从 ClientSession.InitializeResult().ServerInfo.Version 读回——这是 Agent 实际看到的值。
// （*mcp.Server.impl 是非导出字段、无 Info() 访问器，只能通过握手验证。）
func TestNewServerReportsVersion(t *testing.T) {
	// Override version for test isolation; restore after.
	orig := version.Version
	version.Version = "v9.9.9-test"
	defer func() { version.Version = orig }()

	svc := &Service{}
	server := NewServer(svc)

	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	clientSession, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()

	result := clientSession.InitializeResult()
	if result == nil {
		t.Fatalf("InitializeResult() = nil; initialize not completed")
	}
	if result.ServerInfo == nil {
		t.Fatalf("InitializeResult().ServerInfo = nil")
	}
	if got, want := result.ServerInfo.Version, version.Version; got != want {
		t.Errorf("serverInfo.Version = %q, want %q", got, want)
	}
	if result.ServerInfo.Name != "sshmng" {
		t.Errorf("serverInfo.Name = %q, want %q", result.ServerInfo.Name, "sshmng")
	}
}
