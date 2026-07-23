package pty

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"

	"sshmng/internal/ssh/conn"
)

// UploadDir 把本地 localDir 整树上传到远端 remoteDir。
//   - opts.Concurrency=0 默认 4；本 Task 1 先实现顺序版本（Concurrency 强制 1）
//   - opts.Conflict=0 默认 ConflictOverwrite；本 Task 1 只实现 overwrite
//   - per-file 错误不中断整树传输，用 errors.Join 聚合返回
//   - symlink 跳过（不计入 Skipped，因为 Skipped 是 ConflictSkip 计数；symlink 不计入任何计数）
//
// 返回 DirTransferResult 汇总。
func (p *PtyConn) UploadDir(localDir, remoteDir string, opts conn.DirTransferOptions) (conn.DirTransferResult, error) {
	p.logger.Debug("sftp upload_dir start", "sid", p.sid, "local", localDir, "remote", remoteDir, "opts", opts)

	p.mu.Lock()
	sftpClient := p.sftpClient
	p.mu.Unlock()
	if sftpClient == nil {
		return conn.DirTransferResult{}, conn.ErrSftpUnavailable
	}

	// 本地目录必须存在
	info, err := os.Stat(localDir)
	if err != nil {
		return conn.DirTransferResult{}, err
	}
	if !info.IsDir() {
		return conn.DirTransferResult{}, errors.New("local path is not a directory")
	}

	var result conn.DirTransferResult
	var errs []error

	walkErr := filepath.Walk(localDir, func(localPath string, fi os.FileInfo, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil // continue walking
		}

		// 相对路径 → 远端路径
		rel, err := filepath.Rel(localDir, localPath)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))

		// symlink：跳过（v1 YAGNI）
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		if fi.IsDir() {
			if err := sftpClient.MkdirAll(remotePath); err != nil {
				errs = append(errs, err)
			}
			return nil
		}

		// 文件：本 Task 1 只实现 overwrite
		f, err := os.Open(localPath)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		n, timedOut, err := p.uploadOne(f, remotePath, opts.TimeoutMs)
		f.Close()
		result.Bytes += int64(n)
		if err != nil {
			errs = append(errs, err)
		}
		if timedOut {
			result.TimedOut++
		}
		// 注：uploadOne 内部用 p.Upload（已实现），ConflictOverwrite 直接覆盖
		result.Files++
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}

	p.logger.Debug("sftp upload_dir done",
		"sid", p.sid, "files", result.Files, "bytes", result.Bytes, "errors", len(errs))

	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// uploadOne 是单文件上传的 helper，复用 p.Upload 的逻辑但用独立 timeout。
// 本 Task 1 直接调 p.Upload。Task 2 加 skip/rename 时会扩展。
func (p *PtyConn) uploadOne(src io.Reader, remotePath string, timeoutMs int) (int, bool, error) {
	return p.Upload(src, remotePath, timeoutMs)
}
