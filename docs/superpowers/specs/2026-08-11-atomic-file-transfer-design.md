# 原子文件传输:临时文件 + 重命名 (#4)

日期: 2026-08-11

## 背景

当前 `sshmng file` 的 upload/download 直接写目标文件:
- **Upload**:`sftpClient.Create(remotePath)` 直接写远端目标;传输中断/超时留下**不完整的远端文件**,且文件名伪装成完整(`/root/abc.txt` 看起来正常,实为半截)。
- **Download**:`os.Create(local)` 直接写本地目标;同样留半截本地文件,不可识别、易误用。

这与 scp 的问题一样;rsync/apt/pip/rclone 等成熟工具都用 **temp + 原子 rename** 规避:写临时文件,成功后原子重命名到目标;失败/超时则删除临时文件(或保留 `.part` 供判断)。

## 目标

让 upload/download/upload-dir/download-dir/relay 的单文件传输**原子化**:传输期间写临时文件,成功后原子重命名到最终目标;失败/超时删除临时文件,不留半截"伪完整"文件。MCP 路径同样受益(走同一 Conn 方法)。

## 非目标

- 不改 `--timeout` / 进度条 / 路径简写 / conflict policy 的现有语义。
- 不为 dir 传输加整树原子性(那是事务,超出范围)——per-file 原子即可。
- 不加断点续传(temp 文件不保留;`rsync --partial` 那套不在范围)。

## 设计

### 核心机制

每个单文件传输包一层"temp → 成功后 rename / 失败后 remove":

```
Upload/UploadSized:
  tmpPath = remotePath + ".sshmng-tmp-" + random(6 hex)
  Create(tmpPath) → io.Copy → Close
    成功:  PosixRename(tmpPath, remotePath)   # 原子覆盖(openssh 扩展)
    失败/超时: Remove(tmpPath)                 # 不留半截
  PosixRename 失败 → Remove(tmpPath) + 返回 rename 错误

Download:
  tmpPath = local + ".sshmng-tmp-" + random + ".tmp"  # 在目标同目录(os.CreateTemp)
  Create(tmpPath) → io.Copy → Close
    成功:  os.Rename(tmpPath, local)   # 同文件系统原子
    失败/超时: os.Remove(tmpPath)
```

### 关键决策

1. **临时文件命名**:远端 `<remote>.sshmng-tmp-<randhex>`,本地用 `os.CreateTemp(dir, base+".sshmng-tmp-*")`。同目录保证 rename 同文件系统(跨文件系统 rename 非原子,会退化为 copy+delete)。

2. **原子性保证**:
   - 远端:`PosixRename`(`posix-rename@openssh.com` 扩展)原子覆盖。OpenSSH server 原生支持,sshmng 只连自己控制的 OpenSSH server。**若 server 不支持该扩展**(`PosixRename` 返回 unsupported),降级到标准 `Rename` + 预先 `Remove(target)`(非原子窗口极小,但可用)。
   - 本地:`os.Rename` 同文件系统原子(Go 保证)。
   - **`Sync()` best-effort**:Close 前尝试 `sftp.File.Sync()`(`fsync@openssh.com`);不支持则跳过(返回 unsupported 错误,忽略)。`Close()` 本身发 `SSH_FXP_CLOSE`,OpenSSH sftp-server 在 close 时 flush 数据到磁盘并对后续 open 可见——这已足够保证 rename 后读到完整数据。

3. **失败/超时清理**:无论错误还是超时,`Remove(tmpPath)`。`timedOut` 语义不变(返回已传字节 + timed_out=true),只是不再留半截目标文件。

4. **MCP 路径**:MCP `tools_file.go` 的 Upload/Download 走 `Session.Upload/Download` → `Conn.Upload/Download`。改动在 `PtyConn` 的 `Upload`/`UploadSized`/`Download` 方法体,MCP 自动受益。**MCP 返回字段不变**(`bytes`/`timed_out`/`ok`),只是底层原子化。

5. **Dir 传输**:`UploadDir`/`DownloadDir` 的 per-file 调用从 `p.Upload(f, finalPath, ...)`/`p.Download(remote, f, ...)` 改为走新的原子方法。conflict policy 交互:
   - `overwrite`:temp + rename 覆盖(默认,原子)。
   - `skip`:目标存在则跳过(现状,不进 temp 路径)。
   - `rename`:现有 conflict-rename 逻辑(目标重命名),源文件仍走 temp+rename 到最终路径。

6. **Relay**:`RelayTransferWithProgress` 的上传 goroutine 调 `UploadSized(pr, size, dstPath, ...)` 改为原子版本。barrier 模型不变。源下载侧不原子化(relay 源只读,不写本地,无需 temp)。

7. **pipelining 不降速**:temp 改的是 `Create` 的路径参数,reader 包装(`io.LimitReader`)和 `ReadFrom` 并发路径完全不变。`Sync()`/`PosixRename` 在 `io.Copy` 返回 + `Close()` 之后调用,不打断 pipelining。基准验证(Task 4 的 bench)仍需通过。

### 不改的部分

- `Conn` 接口签名不变(`Upload`/`UploadSized`/`Download` 签名不变,方法体内部原子化)。
- `Session.Upload/Download` 透传,不改状态机。
- 超时返回值语义不变(`bytes` + `timed_out=true`)。
- 进度条 / 路径简写 / `connecting to` 提示 不受影响。

## 测试

- 单元:`PtyConn.Upload`/`Download` 原子性 —— 用 fake sftp server(`newFakeShellServerWithSftp`)传一个会中断的 reader(中途返回 error),断言目标文件**不存在**、temp 文件已清理;正常传输断言目标存在、内容正确、temp 不残留。
- PosixRename 降级:fake server 不支持 posix-rename 扩展时,降级路径(标准 Rename + Remove)仍成功(若能模拟;否则记为需集成验证)。
- 基准:`Upload` vs `UploadSized+temp+rename` 吞吐同一量级(确认 Sync/Rename 不降速)。
- Dir/relay:现有测试回归(签名不变,行为叠加)。
- MCP:现有 `tools_file_test.go` 回归。

## 风险

1. **PosixRename 不支持**:极少数非 OpenSSH server 可能不支持。降级到标准 Rename + 预 Remove(非原子窗口小)。需集成验证或至少单元覆盖降级路径。
2. **Sync 不支持**:`fsync@openssh.com` 不支持时跳过。Close 已保证可见性,rename 后读完整。可接受。
3. **同名 temp 冲突**:random(6 hex) 碰撞概率 ~16M 分之一,可忽略;且 Create 会失败报错(不静默覆盖)。
4. **磁盘空间**:temp + target 短暂双份(rename 前目标旧文件 + temp 并存)。大文件 + 满盘可能 rename 失败 —— 返回错误,不损坏数据。可接受(同 rsync 行为)。
5. **跨文件系统**:本地 temp 用 `os.CreateTemp(dir, ...)`,`dir` = 目标所在目录,保证同文件系统。远端 temp 同目录同理。

## 文档

CLI 签名不变,无需改 README/usage。`docs/agents.md` 的 MCP 工具描述可补一句"原子写入"(可选,非阻塞)。CLAUDE.md checklist 不涉及。
