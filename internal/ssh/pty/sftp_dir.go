package pty

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"sync"

	"sshmng/internal/ssh/conn"
)

// UploadDir 把本地 localDir 整树上传到远端 remoteDir。
//   - opts.Concurrency<=0 默认 4；Task 3 起使用并发 worker pool
//   - opts.Conflict=0 默认 ConflictOverwrite；Task 2 加 skip/rename
//   - per-file 错误不中断整树传输，用 errors.Join 聚合返回
//   - symlink 跳过（不计入 Skipped，因为 Skipped 是 ConflictSkip 计数；symlink 不计入任何计数）
//
// 两遍扫描：第一遍 Walk 收集文件任务（目录在 Walk 内即时 MkdirAll）；
// 第二遍 N 个 worker 从 channel 拉任务并发传文件，结果用 mutex 保护的 DirTransferResult 聚合。
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

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	// 第一遍：Walk 收集所有文件（目录在 Walk 内即时 MkdirAll）
	type fileTask struct {
		localPath, remotePath string
	}
	var tasks []fileTask
	var walkErrs []error

	walkErr := filepath.Walk(localDir, func(localPath string, fi os.FileInfo, err error) error {
		if err != nil {
			walkErrs = append(walkErrs, err)
			return nil
		}
		rel, err := filepath.Rel(localDir, localPath)
		if err != nil {
			walkErrs = append(walkErrs, err)
			return nil
		}
		remotePath := path.Join(remoteDir, filepath.ToSlash(rel))

		if fi.Mode()&os.ModeSymlink != 0 {
			return nil // skip symlinks
		}
		if fi.IsDir() {
			if err := sftpClient.MkdirAll(remotePath); err != nil {
				walkErrs = append(walkErrs, err)
			}
			return nil
		}
		tasks = append(tasks, fileTask{localPath, remotePath})
		return nil
	})
	if walkErr != nil {
		walkErrs = append(walkErrs, walkErr)
	}

	// 第二遍：并发 worker pool 传文件
	taskCh := make(chan fileTask)
	var mu sync.Mutex
	var result conn.DirTransferResult
	var errs []error
	errs = append(errs, walkErrs...)
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for task := range taskCh {
				f, err := os.Open(task.localPath)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					continue
				}
				finalPath, action, err := p.resolveConflict(task.remotePath, opts.Conflict)
				if err != nil {
					f.Close()
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					continue
				}
				if action == conflictSkip {
					f.Close()
					mu.Lock()
					result.Skipped++
					mu.Unlock()
					continue
				}
				if action == conflictRename {
					mu.Lock()
					result.Renamed++
					mu.Unlock()
				}
				n, timedOut, err := p.Upload(f, finalPath, opts.TimeoutMs)
				f.Close()
				mu.Lock()
				result.Bytes += int64(n)
				result.Files++
				if err != nil {
					errs = append(errs, err)
				}
				if timedOut {
					result.TimedOut++
				}
				mu.Unlock()
			}
		}()
	}
	go func() {
		for _, t := range tasks {
			taskCh <- t
		}
		close(taskCh)
	}()
	wg.Wait()

	p.logger.Debug("sftp upload_dir done",
		"sid", p.sid, "files", result.Files, "bytes", result.Bytes,
		"skipped", result.Skipped, "renamed", result.Renamed,
		"errors", len(errs))

	if len(errs) > 0 {
		return result, errors.Join(errs...)
	}
	return result, nil
}

// conflictAction 是 resolveConflict 的内部结果。
type conflictAction int

const (
	conflictOverwrite conflictAction = iota
	conflictSkip
	conflictRename
)

// resolveConflict 根据 policy 决定最终远端路径。
//   - ConflictOverwrite: 直接返回 remotePath，action=overwrite
//   - ConflictSkip: Stat 检查存在；存在返回 skip，不存在返回 overwrite
//   - ConflictRename: 找无冲突路径 name_1.ext、name_2.ext...
//
// 调用前 sftpClient 必须非 nil。
func (p *PtyConn) resolveConflict(remotePath string, policy conn.ConflictPolicy) (finalPath string, action conflictAction, err error) {
	if policy == conn.ConflictOverwrite {
		return remotePath, conflictOverwrite, nil
	}

	// Stat 检查存在性
	_, statErr := p.sftpClient.Stat(remotePath)
	notExist := isNotExist(statErr)

	if policy == conn.ConflictSkip {
		if notExist {
			return remotePath, conflictOverwrite, nil
		}
		if statErr != nil {
			return "", 0, statErr // 其他 Stat 错误
		}
		return remotePath, conflictSkip, nil
	}

	// ConflictRename
	if notExist {
		return remotePath, conflictOverwrite, nil // 无冲突直接用原名
	}
	if statErr != nil {
		return "", 0, statErr
	}
	// 找 name_1、name_2...
	dir := path.Dir(remotePath)
	base := path.Base(remotePath)
	ext := ""
	if dot := indexOfDot(base); dot >= 0 {
		ext = base[dot:]
		base = base[:dot]
	}
	for i := 1; ; i++ {
		candidate := path.Join(dir, base+"_"+itoa(i)+ext)
		if _, e := p.sftpClient.Stat(candidate); e != nil {
			if isNotExist(e) {
				return candidate, conflictRename, nil
			}
			return "", 0, e
		}
	}
}

// isNotExist 判断 sftp.Stat 错误是否表示"文件不存在"。
func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	// os.ErrNotExist 包装 sftp 的 SSH_FX_NO_SUCH_FILE
	return errors.Is(err, os.ErrNotExist)
}

// indexOfDot 返回 base 中最后一个 '.' 的索引（用于切分扩展名）。
// 无点返回 -1。
func indexOfDot(base string) int {
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '.' {
			return i
		}
	}
	return -1
}

// itoa 是 strconv.Itoa 的简版（避免新 import）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
