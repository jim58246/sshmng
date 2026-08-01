## Pre-release checklist

Before creating a release tag, verify docs match the code:

1. **CLI subcommands**: `sshmng help` output matches README "Usage" section + `docs/agents.md`. Check command names, flags, positional args.
2. **MCP tools**: tool list + signatures in `docs/agents.md` match `internal/mcp/tools_*.go` (registered in `internal/mcp/server.go`).
3. **Config schema**: `docs/configuration.md` matches `internal/config/types.go` — fields, types, defaults, json tags.
4. **Bilingual sync**: every change to English docs (`README.md`, `docs/*.md`) must be mirrored in `docs/zh-CN/*.md` and `README.zh-CN.md`. Check both directions.
5. **Version refs**: `grep -rn "[0-9]\+\.[0-9]\+\.[0-9]\+" README.md docs/ README.zh-CN.md docs/zh-CN/` — any hardcoded version strings must match the tag being released.

If a doc is out of sync, fix the doc (or the code) before tagging. Do not tag over known doc drift.
