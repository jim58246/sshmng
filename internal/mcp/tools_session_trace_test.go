package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"sshmng/internal/config"
	"sshmng/internal/ssh/conn"
)

// --- get_trace ---

// TestGetTraceUnknownSID: get_trace 对未知 sid 报错。
func TestGetTraceUnknownSID(t *testing.T) {
	svc := newTestService(t, &config.Config{Version: "1"})
	res, _, _ := svc.GetTrace(context.Background(), &mcp.CallToolRequest{}, GetTraceArgs{SID: "nope"})
	if !res.IsError {
		t.Errorf("expected IsError=true for unknown sid")
	}
}

// TestGetTraceAfterRun: 跑一条命令后 get_trace 返回该 trace。
func TestGetTraceAfterRun(t *testing.T) {
	srv := newFakeShellServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "s", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	loginRes, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "s"})
	sid := parseJSON(t, resultText(t, loginRes)).(map[string]any)["sid"].(string)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	svc.RunInSession(context.Background(), &mcp.CallToolRequest{}, RunInSessionArgs{SID: sid, Cmd: "echo hello"})

	res, _, err := svc.GetTrace(context.Background(), &mcp.CallToolRequest{}, GetTraceArgs{SID: sid})
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if res.IsError {
		t.Fatalf("GetTrace failed: %s", resultText(t, res))
	}
	traces := parseJSON(t, resultText(t, res)).(map[string]any)["commands"].([]any)
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	tr := traces[0].(map[string]any)
	if tr["cmd"] != "echo hello" {
		t.Errorf("cmd = %v, want %q", tr["cmd"], "echo hello")
	}
}

// TestGetTraceAfterClose: close_session 后 get_trace 仍能取到（走 graveyard）。
func TestGetTraceAfterClose(t *testing.T) {
	srv := newFakeShellServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "s", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	loginRes, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "s"})
	sid := parseJSON(t, resultText(t, loginRes)).(map[string]any)["sid"].(string)

	svc.RunInSession(context.Background(), &mcp.CallToolRequest{}, RunInSessionArgs{SID: sid, Cmd: "echo hello"})
	svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	res, _, err := svc.GetTrace(context.Background(), &mcp.CallToolRequest{}, GetTraceArgs{SID: sid})
	if err != nil {
		t.Fatalf("GetTrace after close: %v", err)
	}
	if res.IsError {
		t.Fatalf("GetTrace after close failed: %s", resultText(t, res))
	}
	traces := parseJSON(t, resultText(t, res)).(map[string]any)["commands"].([]any)
	if len(traces) != 1 {
		t.Errorf("got %d traces, want 1 (from graveyard)", len(traces))
	}
}

// TestGetTraceWithLastN: get_trace(last_n=1) 只返回最近 1 条。
func TestGetTraceWithLastN(t *testing.T) {
	srv := newFakeShellServerForMCP(t)
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	store.Save(&config.Config{
		Version: "1",
		Servers: []*config.SSHServer{
			{Name: "s", Addr: srv.Addr(), User: "alice", Auth: config.SSHAuth{Password: "wonderland"}},
		},
	})
	svc := NewService(store, conn.NewKnownHostsStore(filepath.Join(dir, "known_hosts")), nil)

	loginRes, _, _ := svc.Login(context.Background(), &mcp.CallToolRequest{}, LoginArgs{Name: "s"})
	sid := parseJSON(t, resultText(t, loginRes)).(map[string]any)["sid"].(string)
	defer svc.CloseSession(context.Background(), &mcp.CallToolRequest{}, CloseSessionArgs{SID: sid})

	svc.RunInSession(context.Background(), &mcp.CallToolRequest{}, RunInSessionArgs{SID: sid, Cmd: "echo one"})
	svc.RunInSession(context.Background(), &mcp.CallToolRequest{}, RunInSessionArgs{SID: sid, Cmd: "echo two"})

	res, _, _ := svc.GetTrace(context.Background(), &mcp.CallToolRequest{}, GetTraceArgs{SID: sid, LastN: 1})
	traces := parseJSON(t, resultText(t, res)).(map[string]any)["commands"].([]any)
	if len(traces) != 1 {
		t.Fatalf("got %d traces, want 1", len(traces))
	}
	if traces[0].(map[string]any)["cmd"] != "echo two" {
		t.Errorf("cmd = %v, want %q", traces[0].(map[string]any)["cmd"], "echo two")
	}
}
