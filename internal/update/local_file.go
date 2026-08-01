package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// assetNameRe 解析 goreleaser 产出的 asset 文件名：
//   sshmng-<tag>-<goos>-<goarch>.<ext>
// Safari 会把 .tar.gz 解压成 .tar，所以也接受 .tar 扩展名。
// ext ∈ {tar.gz, tar, zip}。
var assetNameRe = regexp.MustCompile(`^sshmng-(v[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9.-]*)-(darwin|linux|windows)-(amd64|arm64)\.(tar\.gz|tar|zip)$`)

// binaryName 是当前平台 sshmng 二进制的文件名。
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "sshmng.exe"
	}
	return "sshmng"
}

// findBinaryReader 根据 assetPath 的类型找到 sshmng 二进制并返回其 reader。
//   - 目录：在目录树里找 sshmng / sshmng.exe
//   - .tar.gz / .tgz：gzip 解压 + tar 遍历找二进制
//   - .tar：tar 遍历找二进制
//   - .zip：zip 遍历找二进制
//
// 调用方负责关闭返回的 reader（若是 *os.File / *zip.File 等）。
// archivePath 仅用于错误信息和扩展名判断。
func findBinaryReader(assetPath string) (io.ReadCloser, error) {
	info, err := os.Stat(assetPath)
	if err != nil {
		return nil, fmt.Errorf("stat asset: %w", err)
	}
	if info.IsDir() {
		return findBinaryInDir(assetPath)
	}
	ext := strings.ToLower(filepath.Ext(assetPath))
	// .tar.gz 的 Ext() 只返回 .gz，需要单独判断完整扩展名
	base := strings.ToLower(filepath.Base(assetPath))
	switch {
	case strings.HasSuffix(base, ".tar.gz") || strings.HasSuffix(base, ".tgz"):
		return findBinaryInTarGz(assetPath)
	case strings.HasSuffix(base, ".tar"):
		return findBinaryInTar(assetPath)
	case ext == ".zip":
		return findBinaryInZip(assetPath)
	default:
		return nil, fmt.Errorf("unsupported asset format %q (expected .tar.gz, .tar, or .zip, or a directory)", filepath.Base(assetPath))
	}
}

// findBinaryInDir 在目录树里找 sshmng / sshmng.exe 文件。
// goreleaser 产出的 tar 解压后是扁平结构（sshmng + LICENSE + README.md），
// 但 Safari 可能进一步解压成目录，所以做递归查找兜底。
func findBinaryInDir(dir string) (io.ReadCloser, error) {
	target := binaryName()
	var found string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Base(path) == target {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk dir: %w", err)
	}
	if found == "" {
		return nil, fmt.Errorf("binary %q not found in directory %s", target, dir)
	}
	f, err := os.Open(found)
	if err != nil {
		return nil, fmt.Errorf("open binary: %w", err)
	}
	return f, nil
}

// findBinaryInTarGz 打开 .tar.gz，gzip 解压后在 tar 流里找二进制。
// 返回的 reader 关闭时需要同时关 gzip reader 和底层 file——用 compositeReader 包装。
func findBinaryInTarGz(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open asset: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			f.Close()
			return nil, fmt.Errorf("binary %q not found in tar.gz", binaryName())
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(h.Name) == binaryName() {
			// tar.Reader 的 io.ReadCloser 关闭需要关 gz + f。
			return &compositeCloser{Reader: tr, closers: []io.Closer{gz, f}}, nil
		}
	}
}

// findBinaryInTar 打开 .tar（无 gzip）在 tar 流里找二进制。
// Safari 把 .tar.gz 解压成 .tar 时走这条路径。
func findBinaryInTar(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open asset: %w", err)
	}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			f.Close()
			return nil, fmt.Errorf("binary %q not found in tar", binaryName())
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(h.Name) == binaryName() {
			return &compositeCloser{Reader: tr, closers: []io.Closer{f}}, nil
		}
	}
}

// findBinaryInZip 打开 .zip 在条目里找二进制。
func findBinaryInZip(path string) (io.ReadCloser, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, file := range r.File {
		if file.FileInfo().IsDir() {
			continue
		}
		if filepath.Base(file.Name) == binaryName() {
			rc, err := file.Open()
			if err != nil {
				r.Close()
				return nil, fmt.Errorf("open zip entry: %w", err)
			}
			return &compositeCloser{Reader: rc, closers: []io.Closer{rc, r}}, nil
		}
	}
	r.Close()
	return nil, fmt.Errorf("binary %q not found in zip", binaryName())
}

// compositeCloser 包装一个 Reader，关闭时关闭所有关联的 Closer（如 gzip + file）。
type compositeCloser struct {
	io.Reader
	closers []io.Closer
}

func (c *compositeCloser) Close() error {
	var firstErr error
	for _, cl := range c.closers {
		if err := cl.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// validateAssetPlatform 从 asset 文件名解析 goos/goarch 并校验匹配当前平台。
// 目录输入无法从文件名判断平台，跳过校验（信任用户自己解压的目录）。
func validateAssetPlatform(assetPath string) error {
	info, err := os.Stat(assetPath)
	if err != nil {
		return fmt.Errorf("stat asset: %w", err)
	}
	if info.IsDir() {
		return nil
	}
	m := assetNameRe.FindStringSubmatch(filepath.Base(assetPath))
	if m == nil {
		return fmt.Errorf("asset filename %q doesn't match expected format sshmng-<tag>-<os>-<arch>.(tar.gz|tar|zip) — if Safari extracted the archive, pass the .tar file or the extracted directory instead",
			filepath.Base(assetPath))
	}
	goos, goarch := m[2], m[3]
	if goos != runtime.GOOS || goarch != runtime.GOARCH {
		return fmt.Errorf("asset platform mismatch: asset is for %s/%s, this binary is %s/%s. Download the correct archive for your platform.",
			goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// assetVersionFromName 从 asset 文件名提取版本 tag（如 "v0.1.4"）。
// 目录或无法解析的文件名返回空字符串。
func assetVersionFromName(name string) string {
	m := assetNameRe.FindStringSubmatch(filepath.Base(name))
	if m == nil {
		return ""
	}
	return m[1]
}
