# Relay Transfer (1:N fanout) — Design

**Date:** 2026-08-05
**Status:** Approved (brainstormed 2026-08-05)
**Scope:** New MCP tool `relay_transfer` that streams a file from one SSH session to one or more other sessions through the sshmng process, without landing it on local disk and without waiting for the full download before starting upload. Single-file only; directory relay is out of scope.

## Problem

When two machines cannot talk to each other directly (e.g. a build machine and a deploy machine on isolated networks), sshmng is the relay. Today that means: `download` from the source session to a local file (wait for it to finish), then `upload` that local file to the target session (wait again). Total wall-clock ≈ `download_time + upload_time`, and a full copy lands on local disk.

For the common backend case of N sharded/replicated machines that all need the same artifact, the agent must repeat this N times serially, re-reading the source file N times — the source machine's bandwidth becomes the bottleneck.

## Goal

- Stream the file: download and upload overlap. Wall-clock per destination ≈ `max(download_time, upload_time)` for the 1:1 case; for 1:N the source is read once and fanned out to all destinations concurrently.
- No local disk usage. Memory bounded by a small per-destination buffer.
- 1:N fanout: one source, many destinations, shared destination path (sharded-deploy norm: same artifact at the same path on each host).
- 1:1 is the N=1 special case.

## Why this is feasible on the current architecture

Each session has an independent state machine (`idle → running → idle`, non-idle returns "session busy"). That gating is **per-session**. The source session (sidA) and each destination session (sidB_i) are independent sessions, so `Download` on sidA and `Upload` on each sidB_i can run concurrently — no cross-session lock, no deadlock.

The existing transfer signatures are already stream-oriented:

- `Conn.Download(remotePath string, dst io.Writer, timeoutMs int) (int, bool, error)` — downloads into any `io.Writer`.
- `Conn.Upload(src io.Reader, remotePath string, timeoutMs int) (int, bool, error)` — uploads from any `io.Reader`.

So the relay is just an `io.Pipe` (or N of them) between the two: download goroutine writes the pipe, upload goroutine(s) read it. No new transport, no PTY involvement — pure in-process `io` plumbing on top of the existing sftp path.

## Design

### New MCP tool `relay_transfer`

**Arguments:**

| field | type | notes |
|---|---|---|
| `src_sid` | string | source session (build machine) |
| `src_path` | string | remote file path on the source session |
| `dst_sids` | string[] | destination sessions (deploy machines). 1:1 = single-element array. |
| `dst_path` | string | remote write path shared by **all** destinations |
| `timeout_ms` | int | optional, 0 = default 300s. Each of {download side, every upload side} gets its own budget of this value (they run concurrently; total wall-clock ≤ this value). |

API follows the existing sids pattern: the caller logs into the source and each destination session first, then calls `relay_transfer`. No implicit auto-login by server name — consistent with `upload`/`download`.

`dst_path` is shared across all destinations. Sharded deployments put the same artifact at the same path on each host (e.g. `/opt/app/bin/server`), so per-destination paths are not needed. Different paths ⇒ multiple calls. (YAGNI on per-dest paths.)

**Returns:**

| field | type | notes |
|---|---|---|
| `ok` | bool | download succeeded **and** every destination succeeded |
| `downloaded_bytes` | int | bytes read from the source (== size on full success) |
| `timed_out` | bool | download or any destination timed out |
| `src_server` | string | source server name (readability/logging) |
| `destinations` | [] | one entry per `dst_sids`, in input order: `{ dst_sid, dst_server, ok, bytes, timed_out, error }` |

`destinations[i].bytes` is bytes that arrived at that destination (matches `upload` tool semantics). For 1:1, `destinations[0].bytes` is the answer; `ok` == `destinations[0].ok`.

### Orchestration: `Manager.RelayTransfer`

Lives in `internal/ssh/session` (the `Manager`'s package). Coordinates two or more sessions; does **not** touch the per-session state machine — each `Session.Upload`/`Session.Download` call drives its own `idle → running → idle` transition and its own idle timer.

```
func (m *Manager) RelayTransfer(srcSid, srcPath string, dstSids []string, dstPath string, timeoutMs int) (RelayResult, error)

RelayResult {
    DownloadedBytes int
    TimedOut        bool
    SrcServer       string
    Destinations    []RelayDest   // len == len(dstSids), input order
    Err             error         // nil iff download ok and every dest ok
}

RelayDest {
    DstSid    string
    DstServer string
    OK        bool
    Bytes     int
    TimedOut  bool
    Err       error   // nil on success
}
```

Flow:

1. `Get(srcSid)` and `Get` each `dstSid`. Reject `srcSid == any dstSid` ("cannot relay a session to itself").
2. Validate: source sftp available; each destination sftp available and idle (non-idle → "session busy"); Pattern B target without sftp → clear error. Collect pre-flight failures into per-dest results; destinations that fail pre-flight are marked dead and skipped — they do not abort the others.
3. `srcSess.Stat(srcPath)` once → `size`; verify the source is a regular file (reject directory / symlink → early failure before any transfer). On `Stat` failure, fall back: no size known → upload side uses existing `Upload` (serial path); transfer still completes, just slower. Graceful degradation.
4. For each **live** destination `i`, `io.Pipe()` → `(pr_i, pw_i)`.
5. Spawn N upload goroutines (one per live dest):
   `n_i, tOut_i, err_i := dstSess_i.UploadSized(pr_i, size, dstPath, timeoutMs)`
   (or `Upload(pr_i, dstPath, timeoutMs)` if no size.)
6. Spawn 1 download goroutine:
   `n, tOut, err := srcSess.Download(srcPath, fanWriter, timeoutMs)`
   where `fanWriter.Write(p)` concurrently writes `p` to every **still-live** `pw_i`, waiting for all to accept; if a `pw_i` is already closed (that dest failed), mark dead and skip; if **all** dead, return error so `Download` terminates early (stop reading the source for nothing).
7. When the download goroutine finishes, `CloseWithError(err)` every `pw_i` (nil → EOF → upload sides see clean EOF and finish; non-nil → unblocks upload sides on failure).
8. Wait for all N+1 goroutines. Aggregate results.

Each side runs on its own session; idle timers are stopped on entry and reset on exit inside the existing `Session.Upload`/`Session.Download`, so a long relay does not trip idle timeout.

### Upload-side concurrent pipelining (critical for performance)

The existing `Download` uses `*sftp.File.WriteTo` with `UseConcurrentReads(true)` — already concurrent. But `Upload` uses `dst.ReadFrom(src)`, which only takes the concurrent-pipelining path (`readFromWithConcurrency`) when `src` exposes `Stat()`/`Size()`/`Len()` or is a `*io.LimitReader`; otherwise it degrades to **serial** `writeChunkAt` (each 64KB packet blocks on ack). At 100ms cross-region RTT, serial ≈ 640KB/s vs concurrent ≈ 10MB/s — an order of magnitude.

`io.Pipe`'s reader exposes no size, so passing it straight to `Upload` falls into the serial path and partly defeats the relay. Two new `Conn` methods fix this:

- **`Conn.Stat(path string) (os.FileInfo, error)`** — `PtyConn` implements via `sftpClient.Stat`. Also used in step 3 to verify the source is a regular file. General-purpose, reusable.
- **`Conn.UploadSized(src io.Reader, size int64, remotePath string, timeoutMs int) (int, bool, error)`** — internally `io.Copy(dst, io.LimitReader(newCtxReader(src, ctx), size))`. `*io.LimitReader` is recognized by `ReadFrom` → concurrent pipelining; `newCtxReader` preserves ctx-cancellation responsiveness; `LimitReader` bounds the read to the known size. The existing `Upload` (used with `*os.File`, relies on `Stat()`) is unchanged — no blast radius outside the relay.

One `Stat`, N `UploadSized` calls — every upload side takes the fast path; benefit scales with N.

### fanWriter (the tee)

```
type fanWriter struct {
    mu    sync.Mutex
    pipes []*pipeSlot   // one per live destination
}

type pipeSlot struct {
    w    *io.PipeWriter
    dead bool
    err  error          // first error seen on this dest
}

func (fw *fanWriter) Write(p []byte) (int, error) {
    fw.mu.Lock()
    defer fw.mu.Unlock()
    liveCount := 0
    for _, s := range fw.pipes {
        if s.dead { continue }
        liveCount++
        if _, err := s.w.Write(p); err != nil {
            s.dead = true; s.err = err   // this dest failed; isolate
        }
    }
    if liveCount == 0 {
        return 0, errors.New("all destinations failed")  // Download terminates early
    }
    return len(p), nil   // always report full write to Download
}
```

- Always returns `len(p), nil` to the download side as long as ≥1 dest is live — the source keeps streaming. Per-dest failures are captured in `slot.err`, surfaced in `RelayDest.Err`, and do **not** propagate as the download's error.
- Synchronous handoff: download speed is gated by the slowest live dest. This is the cost of no-disk + overlapping download/upload, and matches the "stream as you go" intent. Same-spec sharded hosts are not perceptible; an extreme slow disk could drag the whole relay. If that ever becomes real, a per-dest bounded buffer can decouple them — deferred (YAGNI now).
- Closing: after download finishes, the orchestrator calls `pw_i.CloseWithError(dlErr)` on each pipe; upload sides then see EOF or a closed reader and finish.

### Error / timeout / byte-count aggregation

Each side captures its own `(n, timedOut, err)`. The pipe's `CloseWithError` unblocks the peer, but **results are taken from each side's own capture, not from the error propagated through the pipe** (so "peer closed the pipe" is not mistaken for the root cause):

- `destinations[i].bytes = uploadN_i` (bytes arrived at dest i).
- `downloaded_bytes = downloadN`.
- `timed_out = dlTimedOut || any upTimedOut_i`.
- Per-dest root cause: that dest's own `err` if non-nil and not `io.ErrClosedPipe`; `ErrClosedPipe` (peer-closed) is ignored, and a real timeout/error from that dest is preserved.
- Top-level `ok`: `download ok && downloadN == size && every dest ok && every dest bytes == size`.
- Download-side early termination ("all destinations failed") sets `ok=false` with a clear top-level error; `downloaded_bytes` reflects what was read before termination.

### Constraints & edge cases

- Source file must not be modified during transfer (`LimitReader` uses the initial `size`). Build-artifact scenario satisfies this; documented.
- Destination parent directory must already exist (inherits `upload`'s `sftp.Create` semantics — no auto-mkdir).
- `src_sid == any dst_sid`, source not a regular file, any side sftp-unavailable or non-idle → clear pre-flight error; no transfer started.
- Empty `dst_sids` → error ("no destinations").
- Duplicate `dst_sids` → each entry streamed independently (no dedup; the caller asked for it twice, they get it twice).

## Files touched

- `internal/ssh/session/relay.go` (new): `Manager.RelayTransfer`, `RelayResult`, `RelayDest`, `fanWriter`.
- `internal/ssh/session/session.go`: add `Stat` and `UploadSized` to the `Conn` interface; add `Session.Stat` / `Session.UploadSized` forwarding methods (symmetric with existing `Session.Upload`).
- `internal/ssh/pty/sftp.go`: `PtyConn.Stat`, `PtyConn.UploadSized`.
- `internal/mcp/tools_relay.go` (new): `relay_transfer` handler + `RelayTransferArgs`.
- `internal/mcp/server.go`: register `relay_transfer` in the "File transfer tools" block.
- `internal/ssh/session/session_test.go` (and any other `fakeConn` sites): implement `Stat` / `UploadSized` on `fakeConn` (the standard `Conn` stand-in used across session tests).
- Docs (bilingual, per CLAUDE.md checklist): `docs/agents.md`, `README.md`, `docs/zh-CN/agents.md`, `README.zh-CN.md`. MCP tool list + signatures must match `tools_*.go` / `server.go`.

## Tests (fake-conn unit tests, `internal/ssh/session`)

Reuse the `fakeConn` pattern in `session_test.go`:

- **1:1 happy path**: assert `ok=true`, `downloaded_bytes == destinations[0].bytes == size`.
- **1:N happy path** (e.g. N=3): all dests `ok=true`, each `bytes == size`, source read once (fakeConn download call count == 1).
- **Source file missing**: download side errors; root cause is the source error, not pipe-closed; `ok=false`.
- **Download-side timeout**: `timed_out=true`, `downloaded_bytes < size`, destinations get partial bytes / closed pipe; `ok=false`.
- **One dest fails mid-stream**: that dest `ok=false` with its own error; other dests still `ok=true` and receive full file. Download does not abort.
- **All dests fail**: download terminates early ("all destinations failed"); `ok=false`; `downloaded_bytes` reflects pre-termination read.
- **`Stat` failure fallback**: upload side falls back to serial `Upload`; transfer still completes (`ok=true`), verifying graceful degradation.
- **Pre-flight rejections**: `src_sid == dst_sid`, sftp-unavailable side, non-idle side, empty `dst_sids` → clear error, no transfer.

## Out of scope (explicit)

- Directory-tree relay (`relay_transfer_dir`). Single file is the unit; directory relay can be built on top later if needed.
- Per-destination paths. Shared `dst_path`; different paths ⇒ multiple calls.
- Per-dest bounded buffer to decouple a slow dest from the download pace. Deferred unless slow-disk drag becomes a real problem.
- Progress reporting / streaming partial results to the caller. Single synchronous result, like `upload`/`download`.
