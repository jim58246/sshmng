# English Documentation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add English versions of README, `docs/configuration.md`, and `docs/agents.md`; preserve Chinese versions verbatim at new locations; verify all cross-links.

**Architecture:** Bilingual file pairs. English is primary (no suffix), Chinese is mirrored (`.zh-CN.md` for README, `docs/zh-CN/` for docs). Top-of-file language switcher on every bilingual file. Cross-links stay within-language except one annotated exception (English README → Chinese-only design doc).

**Tech Stack:** Plain Markdown. No build step. Verification via `grep`-based link checks.

## Global Constraints

These apply to every task. Translate consistently across all three files using this guide.

### Naming & Layout

- No suffix = English (primary). `.zh-CN.md` (for README) or `zh-CN/` subdirectory (for docs) = Chinese.
- `docs/development.md`, `docs/ssh-session-manager-design.md`, `docs/implementation-plan.md` stay Chinese-only — do NOT translate, do NOT move.
- No code changes. No CLI output changes. No new dependencies.

### Language Switcher Format

Single line at the very top of each bilingual file, before the `#` heading:

- `README.md`: `[English](./README.md) | [简体中文](./README.zh-CN.md)`
- `README.zh-CN.md`: same as above
- `docs/configuration.md`: `[English](./configuration.md) | [简体中文](./zh-CN/configuration.md)`
- `docs/zh-CN/configuration.md`: `[English](../configuration.md) | [简体中文](./configuration.md)`
- `docs/agents.md`: `[English](./agents.md) | [简体中文](./zh-CN/agents.md)`
- `docs/zh-CN/agents.md`: `[English](../agents.md) | [简体中文](./agents.md)`

Blank line after the switcher, then the `#` heading.

### Cross-Link Discipline

- English links to English only; Chinese links to Chinese only.
- When moving a Chinese file, update its internal links to point to Chinese versions of other docs.
- English README links to Chinese-only docs (`development.md`, `ssh-session-manager-design.md`, `implementation-plan.md`) — annotate each as "(Chinese only — translations welcome)" inline.

### Terminology Guide (translate consistently)

| Chinese | English | Notes |
|---------|---------|-------|
| 会话管理工具 | session manager | lowercase in prose |
| 堡垒机 | bastion | not "jump server" |
| 透明转发 | transparent forwarding | Pattern A |
| 交互式堡垒机 | interactive bastion | Pattern B |
| 决策树 | decision tree | LoginFlow |
| 配置自愈 | self-healing config | |
| 首次上手辅助 | first-time setup wizard | |
| 三件套 | trio | login → run_in_session → close_session |
| 跳板 | jump host | |
| 目标机 | target host | |
| 字段参考 | field reference | |
| 形态约束 | shape constraints | |
| 路径解析顺序 | path resolution order | |
| 文件权限 | file permissions | |
| 验证 setup | verifying setup | |
| 典型调用流程 | typical call flow | |
| 失败循环 | failure loop | |
| 脱敏 | sanitize | (before sharing logs) |
| 降级 | downgrade / degrade | permission check "downgrades to WARN" |
| 优雅降级 | graceful degradation | |
| 拨号 | dial | SSH dial |
| 注入 | inject | into Agent config |
| 骨架 | skeleton | empty config skeleton |
| 模板 | template | |
| 缩写 | abbreviation | |
| 大小写不敏感 | case-insensitive | |
| 脱敏 auth | auth redacted | in list_* responses |
| 子串匹配 | substring-match | |
| 唯一标识 | unique identifier | |
| 引用 | reference | name reference |
| 嵌套对象 | nested object | |
| 保留字符串 | reserved string | "success" |
| 死循环 | infinite loop | max_steps prevents |
| 轮转 | rotation | log rotation |
| 权限 | permissions | file perms |
| 凭据 | credentials | |

### Preserved Verbatim (do NOT translate)

- All `code` spans: tool names (`upload_dir`, `run_in_session`, etc.), field names (`ssh_j`, `login_flow`, `host_key_verify`), env vars (`SSHMNG_HOME`), CLI flags (`--config`, `--yes`), file paths (`~/.sshmng/config.json`), JSON keys.
- All code blocks (JSON examples, bash commands).
- All technical terms: MCP, LoginFlow, Pattern A/B, TOFU, RFC 7396, JSON Merge Patch, stdio, PTY, sftp, PEM, PAM, NTFS, ACL, SOCKS5, HTTP CONNECT, direct-tcpip.
- GitHub URLs, link paths.
- Section anchors are auto-generated from headings by GitHub; translating a heading changes its anchor, so any TOC links must be updated to match.

### Translation Style

- Faithful, not literal. Punchy Chinese → natural English.
- Preserve table structure (column counts, alignment).
- Preserve list ordering and bullet counts.
- Preserve blockquote `>` callouts.
- Comments inside code blocks stay as-is (they're often English already).

---

## File Structure

After all tasks complete:

```
README.md                              (English — new, Task 1)
README.zh-CN.md                        (Chinese — moved from README.md, Task 1)
docs/
  configuration.md                     (English — new, Task 2)
  agents.md                            (English — new, Task 3)
  development.md                       (Chinese — unchanged)
  ssh-session-manager-design.md        (Chinese — unchanged)
  implementation-plan.md               (Chinese — unchanged)
  zh-CN/
    configuration.md                   (Chinese — moved from docs/configuration.md, Task 2)
    agents.md                          (Chinese — moved from docs/agents.md, Task 3)
```

---

## Task 1: README Pair

**Files:**
- Move: `README.md` → `README.zh-CN.md`
- Modify: `README.zh-CN.md` (add switcher, fix 2 cross-links)
- Create: `README.md` (English version)

**Interfaces:**
- Produces: `README.md` (English) — referenced by GitHub as default landing page.
- Produces: `README.zh-CN.md` (Chinese) — referenced by `README.md` switcher.

- [ ] **Step 1: Move Chinese README to its new location**

```bash
git mv README.md README.zh-CN.md
```

- [ ] **Step 2: Add language switcher to top of `README.zh-CN.md`**

Edit `README.zh-CN.md`. The file currently starts with `# sshmng`. Insert the switcher line + blank line before it.

Old first line:
```
# sshmng
```

New first three lines:
```
[English](./README.md) | [简体中文](./README.zh-CN.md)

# sshmng
```

- [ ] **Step 3: Fix cross-links in `README.zh-CN.md`**

Two links in `README.zh-CN.md` currently point to `docs/configuration.md` and `docs/agents.md`, which will become English after Tasks 2–3. Update them to point to the Chinese versions.

Edit 1 — find:
```
手动配置 fallback 与各 Agent 详细集成步骤见 [docs/agents.md](docs/agents.md)。
```
replace with:
```
手动配置 fallback 与各 Agent 详细集成步骤见 [docs/agents.md](docs/zh-CN/agents.md)。
```

Edit 2 — find the docs list near the bottom:
```
- [配置参考](docs/configuration.md) — 完整 config.json 字段参考、Pattern A/B 形态约束、示例
- [Agent 集成指南](docs/agents.md) — Claude Code / Hermes Agent / OpenCode / Claude Desktop 详细配置、MCP Inspector 调试、首次配置流程、典型调用流程
```
replace with:
```
- [配置参考](docs/zh-CN/configuration.md) — 完整 config.json 字段参考、Pattern A/B 形态约束、示例
- [Agent 集成指南](docs/zh-CN/agents.md) — Claude Code / Hermes Agent / OpenCode / Claude Desktop 详细配置、MCP Inspector 调试、首次配置流程、典型调用流程
```

Links to `docs/development.md`, `docs/ssh-session-manager-design.md`, `docs/implementation-plan.md` stay as-is (those docs remain Chinese-only at their original paths).

- [ ] **Step 4: Write new English `README.md`**

Create `README.md` with English content. Translate the Chinese `README.zh-CN.md` section-by-section using the Terminology Guide. Structure (sections in this exact order):

1. Switcher line: `[English](./README.md) | [简体中文](./README.zh-CN.md)` + blank line
2. `# sshmng` — one-paragraph intro: "SSH session manager exposed as an MCP (Model Context Protocol) server. Lets AI agents (Claude Code / Claude Desktop / Hermes Agent / OpenCode / Cursor, etc.) manage SSH connections, run commands, and transfer files through a unified tool interface — with support for interactive bastions and LoginFlow decision trees."
3. Blockquote callout: v1 stage note, link to design doc with annotation "(Chinese only — translations welcome)"
4. `## Features` — 9 bullets, one per feature. Translate each bullet's prose; preserve all `code` spans and technical terms per terminology guide. The sftp bullet must mention both single-file (`upload`/`download`) and directory (`upload_dir`/`download_dir`) tools.
5. `## Install & Build` — 3 install options (release binary / go install / clone+build), same code blocks. Note that `sshmng install` creates `~/.sshmng/` and injects into Agents; link to `#quick-start`.
6. Run commands block (same as Chinese).
7. `## Quick Start` — 4-step bash block + non-interactive variant + link to `docs/agents.md`.
8. `## MCP Tools Overview` — "18 tools total:" + the tools table. Translate the "Description" column; preserve tool names and signatures. Add the blockquote about no `send_input`/`send_special`.
9. `## Security Notes` — 5 bullets (plaintext storage, TOFU host key, trace sensitivity, stdout logging, auth scope).
10. `## Testing & Development` — `go test -race ./...` block + link to `docs/development.md` with "(Chinese only — translations welcome)" annotation.
11. `## Documentation` — 5 bullets linking to: `docs/configuration.md`, `docs/agents.md`, `docs/development.md` (annotated Chinese-only), `docs/ssh-session-manager-design.md` (annotated), `docs/implementation-plan.md` (annotated).
12. `## Contributing` — link to GitHub issues, "PRs not accepted at this time."
13. `## License` — MIT, Copyright (c) 2026 jim58246.

- [ ] **Step 5: Verify links in both README files**

```bash
# All internal links in README.md and README.zh-CN.md must point to existing files
grep -oE '\]\([^)]+\)' README.md README.zh-CN.md | sort -u
```

Expected: every path inside `(...)` resolves to a file that exists or will exist after Tasks 2–3. Specifically:
- `./README.md` ✓ (exists)
- `./README.zh-CN.md` ✓ (exists)
- `docs/configuration.md` ✓ (exists, will be English after Task 2)
- `docs/agents.md` ✓ (exists, will be English after Task 3)
- `docs/zh-CN/configuration.md` — will exist after Task 2
- `docs/zh-CN/agents.md` — will exist after Task 3
- `docs/development.md` ✓ (exists, unchanged)
- `docs/ssh-session-manager-design.md` ✓ (exists, unchanged)
- `docs/implementation-plan.md` ✓ (exists, unchanged)
- `LICENSE` ✓ (exists)
- External URLs (github.com/...) — not checked locally.

Note: links to `docs/zh-CN/*` will be broken until Tasks 2–3 complete. That's expected — verify they resolve after Task 3.

- [ ] **Step 6: Commit**

```bash
git add README.md README.zh-CN.md
git commit -m "docs: add English README, move Chinese to README.zh-CN.md

Bilingual pair with top-of-file language switcher. English is
primary (GitHub default landing). Chinese README links updated
to point to Chinese versions of configuration.md and agents.md."
```

---

## Task 2: `configuration.md` Pair

**Files:**
- Create dir: `docs/zh-CN/`
- Move: `docs/configuration.md` → `docs/zh-CN/configuration.md`
- Modify: `docs/zh-CN/configuration.md` (add switcher, fix 1 relative link)
- Create: `docs/configuration.md` (English version)

**Interfaces:**
- Produces: `docs/configuration.md` (English) — linked from `README.md`.
- Produces: `docs/zh-CN/configuration.md` (Chinese) — linked from `README.zh-CN.md`.

- [ ] **Step 1: Create `docs/zh-CN/` and move Chinese file**

```bash
mkdir -p docs/zh-CN
git mv docs/configuration.md docs/zh-CN/configuration.md
```

- [ ] **Step 2: Add language switcher to top of `docs/zh-CN/configuration.md`**

The file currently starts with `# 配置参考`. Insert switcher + blank line before it.

Old first line:
```
# 配置参考
```

New first three lines:
```
[English](../configuration.md) | [简体中文](./configuration.md)

# 配置参考
```

- [ ] **Step 3: Fix relative link in `docs/zh-CN/configuration.md`**

The file has one internal link to the design doc. After moving from `docs/` to `docs/zh-CN/`, the relative path must go up one directory.

Find:
```
详见 [设计文档 3.7](ssh-session-manager-design.md) 的"Send 字节约定"
```
replace with:
```
详见 [设计文档 3.7](../ssh-session-manager-design.md) 的"Send 字节约定"
```

- [ ] **Step 4: Write new English `docs/configuration.md`**

Create `docs/configuration.md` with English content. Translate `docs/zh-CN/configuration.md` section-by-section using the Terminology Guide. Structure:

1. Switcher: `[English](./configuration.md) | [简体中文](./zh-CN/configuration.md)` + blank line
2. `# Configuration Reference` — 2-paragraph intro: config file path (`~/.sshmng/config.json`), override mechanisms (`--config`, `$SSHMNG_HOME`), what this doc covers. Recommend `sshmng install` for first-time users.
3. `## Path Resolution Order` — 3-item numbered list (CLI arg / `$SSHMNG_HOME` / `$HOME`).
4. `## File Permissions` — Unix 0600 requirement, Windows NTFS ACL note, install/doctor WARN behavior on Windows.
5. `## Examples` — two JSON examples (Pattern B and Pattern A). **Preserve JSON verbatim** — only translate the surrounding prose and the "Differences from Pattern B" bullet list.
6. `## Field Reference` — 7 subsections, each with a table: Top-level Config, Proxy, Jumphost, SSHServer, SSHAuth, LoginAction, Expect. Preserve all field names, types, and defaults verbatim. Translate only the "Description" column. Keep the inline `ProxyAuth structure` note and the Pattern B `SSHServer.auth` note.
7. `## Shape and Usage Constraints` — 3 subsections: "Two jumphost shapes" (ssh_j=true/false), "Direct-connect server", "Behavioral conventions" (4 bullets).

Special translation notes for configuration.md:
- "形态" → "shape" (not "form")
- "形态约束" → "shape constraints"
- "凭据" → "credentials"
- "脱敏" → "redacted" (when referring to list_* output) or "sanitize" (when referring to logs before sharing)
- "降级为 WARN" → "downgrade to WARN"
- The design doc link in the LoginAction `send` field description: link to `ssh-session-manager-design.md` (same directory, since English configuration.md is at `docs/configuration.md`), annotate "(Chinese only — translations welcome)".

- [ ] **Step 5: Verify links in both configuration.md files**

```bash
grep -oE '\]\([^)]+\)' docs/configuration.md docs/zh-CN/configuration.md | sort -u
```

Expected:
- `./configuration.md` ✓
- `./zh-CN/configuration.md` ✓ (exists after Step 1)
- `../configuration.md` ✓
- `ssh-session-manager-design.md` ✓ (from English file, same dir)
- `../ssh-session-manager-design.md` ✓ (from Chinese file, up one dir)

- [ ] **Step 6: Commit**

```bash
git add docs/configuration.md docs/zh-CN/configuration.md
git commit -m "docs: add English configuration.md, move Chinese to docs/zh-CN/

Bilingual pair with language switcher. Chinese version's link
to design doc updated to use ../ prefix for new location."
```

---

## Task 3: `agents.md` Pair

**Files:**
- Move: `docs/agents.md` → `docs/zh-CN/agents.md`
- Modify: `docs/zh-CN/agents.md` (add switcher; no internal links to fix)
- Create: `docs/agents.md` (English version)

**Interfaces:**
- Produces: `docs/agents.md` (English) — linked from `README.md`.
- Produces: `docs/zh-CN/agents.md` (Chinese) — linked from `README.zh-CN.md`.

- [ ] **Step 1: Move Chinese file**

```bash
git mv docs/agents.md docs/zh-CN/agents.md
```

(`docs/zh-CN/` already exists from Task 2.)

- [ ] **Step 2: Add language switcher to top of `docs/zh-CN/agents.md`**

The file currently starts with `# Agent 集成指南`. Insert switcher + blank line before it.

Old first line:
```
# Agent 集成指南
```

New first three lines:
```
[English](../agents.md) | [简体中文](./agents.md)

# Agent 集成指南
```

- [ ] **Step 3: Verify `docs/zh-CN/agents.md` has no internal links to fix**

```bash
grep -E '\]\([^)]+\)' docs/zh-CN/agents.md
```

Expected: no internal doc links (the Chinese agents.md has no links to other docs). If any appear, update them to use `../` prefix for docs at the parent level, or `./` for docs within `zh-CN/`.

- [ ] **Step 4: Write new English `docs/agents.md`**

Create `docs/agents.md` with English content. Translate `docs/zh-CN/agents.md` section-by-section using the Terminology Guide. Structure:

1. Switcher: `[English](./agents.md) | [简体中文](./zh-CN/agents.md)` + blank line
2. `# Agent Integration Guide` — 2-paragraph intro: sshmng is a stdio MCP server, any MCP client can connect. Recommend `sshmng install`. Note the `"args": ["mcp"]` subcommand requirement.
3. `## Recommended: sshmng install` — bash block + non-interactive variant + `--agents` values explanation.
4. `## Claude Code` — JSON config block + CLI registration alternative + the "Note" about CLI not auto-adding `args: ["mcp"]` + `/mcp` verification.
5. `## Hermes Agent` — YAML config block (Unix and Windows paths) + note about schema matching Claude Code's.
6. `## OpenCode` — JSON config block + 4-bullet list of schema differences (top-level key, command array, environment field, type+enabled).
7. `## Claude Desktop (macOS)` — JSON config block + note about not being in install scope.
8. `## MCP Inspector (for debugging)` — bash block + Inspector description + note about logs going to `config.log_path` not MCP notifications.
9. `### Log Configuration` — JSON block + 4 bullets (log_level, log_path, bootstrap errors, DEBUG log sensitivity warning).
10. `### login_trace Diagnostics` — paragraph on login failure → login_trace → Agent patches config → retry; and get_trace's login_flow field for post-hoc debugging.
11. `## First-Time Setup Flow` — recommend install wizard + 4-step numbered list of what the wizard does + non-interactive variant + 5-step manual fallback.
12. `## Verifying Setup` — `sshmng doctor` block + checks description + exit codes (0/1/2) + Windows WARN downgrade note.
13. `## Typical Agent Call Flow` — 5-step code block + "Failure loop with LoginFlow diagnostics" 4-step code block.

Special translation notes for agents.md:
- "向导" → "wizard"
- "勾选" → "select" (not "check")
- "注入" → "inject" (into Agent config)
- "fallback" → keep as "fallback" (already English in Chinese text)
- "面板" → "panel" (Inspector panel, tools panel)
- "推送" → "push" (logs)
- "脱敏" → "sanitize"
- Code blocks (JSON, YAML, bash) preserved verbatim — only the prose around them is translated.
- The `"Verifying setup"` heading in the Chinese file is already English — keep it as `## Verifying Setup` (title case).

- [ ] **Step 5: Verify links in both agents.md files**

```bash
grep -oE '\]\([^)]+\)' docs/agents.md docs/zh-CN/agents.md | sort -u
```

Expected: only switcher links (`./agents.md`, `./zh-CN/agents.md`, `../agents.md`). No internal doc links.

- [ ] **Step 6: Commit**

```bash
git add docs/agents.md docs/zh-CN/agents.md
git commit -m "docs: add English agents.md, move Chinese to docs/zh-CN/

Bilingual pair with language switcher. No internal links to
fix in this file (Chinese agents.md has no doc-to-doc links)."
```

---

## Task 4: Final Verification

**Files:** Read-only check across all 6 bilingual files.

- [ ] **Step 1: Verify all switchers are bidirectional and correct**

```bash
echo "=== README.md ===" && head -1 README.md
echo "=== README.zh-CN.md ===" && head -1 README.zh-CN.md
echo "=== docs/configuration.md ===" && head -1 docs/configuration.md
echo "=== docs/zh-CN/configuration.md ===" && head -1 docs/zh-CN/configuration.md
echo "=== docs/agents.md ===" && head -1 docs/agents.md
echo "=== docs/zh-CN/agents.md ===" && head -1 docs/zh-CN/agents.md
```

Expected output:
```
=== README.md ===
[English](./README.md) | [简体中文](./README.zh-CN.md)
=== README.zh-CN.md ===
[English](./README.md) | [简体中文](./README.zh-CN.md)
=== docs/configuration.md ===
[English](./configuration.md) | [简体中文](./zh-CN/configuration.md)
=== docs/zh-CN/configuration.md ===
[English](../configuration.md) | [简体中文](./configuration.md)
=== docs/agents.md ===
[English](./agents.md) | [简体中文](./zh-CN/agents.md)
=== docs/zh-CN/agents.md ===
[English](../agents.md) | [简体中文](./agents.md)
```

- [ ] **Step 2: Verify all internal links resolve (no 404s)**

```bash
# Extract every relative link target from the 6 bilingual files
# and check each resolves to an existing file
for f in README.md README.zh-CN.md docs/configuration.md docs/zh-CN/configuration.md docs/agents.md docs/zh-CN/agents.md; do
  dir=$(dirname "$f")
  grep -oE '\]\(([^)]+)\)' "$f" | sed 's/^](//; s/)$//' | while read -r link; do
    # skip external URLs
    case "$link" in
      http*|https*) continue ;;
    esac
    # resolve relative to the file's directory
    target="$dir/$link"
    if [ ! -e "$target" ]; then
      echo "BROKEN: $f -> $link (resolved: $target)"
    fi
  done
done
echo "link check complete"
```

Expected: no `BROKEN:` lines, only `link check complete`.

- [ ] **Step 3: Verify Chinese versions match pre-translation originals (parity check)**

The Chinese versions should be identical to their pre-move originals, except for: (a) the switcher line + blank line added at top, (b) the link path updates documented in Tasks 1–3.

```bash
# Use git to diff the moved Chinese files against their pre-move originals
git diff HEAD~3 -- README.zh-CN.md docs/zh-CN/configuration.md docs/zh-CN/agents.md
```

Expected: only the switcher line addition and the documented link path changes.

- [ ] **Step 4: Verify tool count and tool list are consistent across languages**

```bash
echo "=== English README tool count ==="
grep -E '^\| (Config|Session|Diagnostics|File)' README.md | wc -l
echo "=== Chinese README tool count ==="
grep -E '^\| (配置|会话|诊断|文件)' README.zh-CN.md | wc -l
```

Expected: both print `10` (the tools table has 10 rows in both languages, covering 18 tools — some rows group 3 tools like `list_*` / `get_*` / `update_*`).

Also verify the "18 tools" count line exists in both:
```bash
grep -E '(18 个工具|18 tools total)' README.md README.zh-CN.md
```

Expected: one match in each file.

- [ ] **Step 5: Visual render check (manual)**

View `README.md` in a markdown renderer (GitHub web UI or local preview). Confirm:
- English renders as primary.
- Language switcher visible at top.
- All internal links navigate to existing files.
- Tables render correctly.
- Code blocks render with syntax highlighting.

If any rendering issues, fix and re-commit. Otherwise, no commit needed for this step.

- [ ] **Step 6: Final commit (only if fixes were needed in Steps 1–5)**

If any verification step surfaced issues that were fixed:
```bash
git add -A
git commit -m "docs: fix bilingual link/switcher issues found in verification"
```

Otherwise, skip — Tasks 1–3 already committed.

---

## Self-Review Notes

**Spec coverage:**
- README pair ✓ (Task 1)
- configuration.md pair ✓ (Task 2)
- agents.md pair ✓ (Task 3)
- Language switcher on every bilingual file ✓ (Tasks 1–3, Step 2 of each)
- Cross-link discipline (English→English, Chinese→Chinese) ✓ (Tasks 1–3, Step 3 of each + Verification Step 2)
- Exception: English README → Chinese-only design doc annotated ✓ (Task 1, Step 4, section 10–11 instructions)
- Translation philosophy (terminology guide, preserve technical terms) ✓ (Global Constraints)
- Chinese version preservation (verbatim + switcher + link fixes only) ✓ (Tasks 1–3, Step 6 of Task 4 verifies parity)
- Out of scope: development.md / design doc / implementation-plan.md unchanged ✓ (no task touches them)
- Verification ✓ (Task 4)

**Placeholder scan:** No "TBD"/"TODO"/"implement later". Translation steps reference the Terminology Guide and Chinese source rather than pre-writing English content — this is intentional for a translation task where the executor (Claude) translates with terminology guidance. Structural operations are all exact.

**Type consistency:** File paths, link formats, and switcher strings are consistent across tasks. Verified in Task 4 Step 1.

**Scope check:** Single focused goal (English docs for 3 files). Appropriately scoped for one plan.
