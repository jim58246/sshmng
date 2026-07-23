# English Documentation Design

> Date: 2026-07-24
> Status: approved → ready for implementation plan

## Goal

Provide English versions of the three most user-facing docs (README, `configuration.md`, `agents.md`) to unlock global adoption of sshmng, while preserving Chinese versions verbatim for existing readers.

The primary audience for sshmng is AI agent developers (Claude Code / Cursor / Hermes / OpenCode) — a global community. English docs are table stakes for adoption. Chinese remains the original authoring language and stays first-class.

## File Structure

```
README.md                              (English, primary — GitHub default)
README.zh-CN.md                        (Chinese — current README content moved)
docs/
  configuration.md                     (English — newly written)
  agents.md                            (English — newly written)
  development.md                       (Chinese — unchanged, contributor doc)
  ssh-session-manager-design.md        (Chinese — unchanged)
  implementation-plan.md               (Chinese — unchanged)
  zh-CN/
    configuration.md                   (Chinese — current content moved)
    agents.md                          (Chinese — current content moved)
```

Naming convention: **no suffix = English (primary)**, `.zh-CN.md` or `zh-CN/` = Chinese. Consistent across README and `docs/`.

## Language Switcher

At the very top of every bilingual file, a single line:

```
[English](./README.md) | [简体中文](./README.zh-CN.md)
```

For `docs/` files, relative paths adjust:

- `docs/configuration.md` top: `[English](./configuration.md) | [简体中文](./zh-CN/configuration.md)`
- `docs/zh-CN/configuration.md` top: `[English](../configuration.md) | [简体中文](./configuration.md)`

Same pattern for `agents.md` and `README.md` ↔ `README.zh-CN.md`.

## Cross-Link Discipline

- **English links to English only; Chinese links to Chinese only.** No mid-browse language hops. A reader starting in English stays in English unless they explicitly click the switcher.
- **One exception**: `README.md` points to `docs/ssh-session-manager-design.md` (Chinese-only design doc, no English version planned). Annotate inline as "(design doc, Chinese only — translations welcome)" so English readers aren't surprised by the language change.

## Translation Philosophy

Faithful translation with light adaptation for global developers:

- **All technical terms preserved verbatim**: MCP, LoginFlow, Pattern A/B, TOFU, RFC 7396, JSON Merge Patch, stdio, PTY, sftp, etc.
- **Punchy Chinese phrases → natural English equivalents**, not literal:
  - "配置自愈" → "Self-healing config"
  - "首次上手辅助" → "First-time setup wizard"
  - "三件套" → "trio" (or just describe the three tools inline)
  - "失败返回 trace 供 Agent 诊断 + 修复配置 + 重试" → "on failure, returns trace for Agent to diagnose → patch config → retry"
- **Unchanged**: commands, file paths, GitHub URLs, code blocks, env var names.
- **Section anchors**: re-translated; internal TOC links verified in both languages. (GitHub auto-generates anchors from headings, so translating headings means anchors change — TOC links must follow.)

## Chinese Version Preservation

Current Chinese content moves **verbatim** to the new `.zh-CN.md` / `zh-CN/` locations:

- `README.md` → `README.zh-CN.md` (content unchanged; only filename + switcher line at top)
- `docs/configuration.md` → `docs/zh-CN/configuration.md` (same)
- `docs/agents.md` → `docs/zh-CN/agents.md` (same)

Existing Chinese readers see zero regression. The only diff they'll notice is the filename and a single switcher line at the top.

## Out of Scope

- **`docs/development.md`, `docs/ssh-session-manager-design.md`, `docs/implementation-plan.md`** — contributor docs, stay Chinese until a non-Chinese-reading contributor actually shows up. Translating now is speculative work.
- **CLI output, error messages, code comments** — already English, no changes needed.
- **i18n framework / translation tooling** (gettext, etc.) — YAGNI for 3 files. Plain Markdown + manual sync is fine.
- **Other docs** (`.superpowers/sdd/` internal task briefs) — internal, not user-facing.

## Verification

After implementation:

1. **Link check**: every internal cross-link resolves in both languages (no 404s). Includes: README → docs, docs → docs, switcher links, TOC anchors.
2. **Anchor check**: TOC links within each file still work after heading translation.
3. **Render check**: view `README.md` on GitHub (or local markdown preview) — English renders as primary, Chinese link visible at top.
4. **Parity check**: Chinese versions have identical content to pre-move originals (diff should show only filename + switcher line).

## Order of Operations

Implementation will proceed in this order (each step is independently committable):

1. **README pair**: move `README.md` → `README.zh-CN.md` (verbatim + switcher); write new English `README.md`.
2. **configuration.md pair**: move `docs/configuration.md` → `docs/zh-CN/configuration.md`; write new English `docs/configuration.md`.
3. **agents.md pair**: move `docs/agents.md` → `docs/zh-CN/agents.md`; write new English `docs/agents.md`.
4. **Verify**: run link check across all bilingual files.

Each step produces one commit. Steps 2–3 can be done in any order; step 1 first because it's the entry point.

## Maintenance

Going forward: when content changes, update both language versions in the same commit (or accept temporary drift and sync explicitly). No tooling enforces this — it's a convention. If drift becomes a problem, revisit with translation tooling.
