package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makeArchiveFixture 造一个含 sshmng 二进制的归档文件。
// format ∈ {"tar.gz", "tar", "zip"}。返回文件路径。
// binaryContent 是写入 sshmng 文件的字节（可以是任意内容，测试不执行它）。
func makeArchiveFixture(t *testing.T, dir, format, tag string, binaryContent []byte) string {
	t.Helper()
	goos, goarch := runtime.GOOS, runtime.GOARCH
	ext := format
	name := "sshmng-" + tag + "-" + goos + "-" + goarch + "." + ext
	path := filepath.Join(dir, name)

	binaryName := "sshmng"
	if goos == "windows" {
		binaryName = "sshmng.exe"
	}

	switch format {
	case "tar.gz":
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		writeTarEntry(t, tw, binaryName, binaryContent)
		tw.Close()
		gz.Close()
		f.Close()
	case "tar":
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		tw := tar.NewWriter(f)
		writeTarEntry(t, tw, binaryName, binaryContent)
		tw.Close()
		f.Close()
	case "zip":
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		w := zip.NewWriter(f)
		zf, err := w.Create(binaryName)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := zf.Write(binaryContent); err != nil {
			t.Fatalf("zip write: %v", err)
		}
		w.Close()
		f.Close()
	default:
		t.Fatalf("unknown format %q", format)
	}
	return path
}

func writeTarEntry(t *testing.T, tw *tar.Writer, name string, content []byte) {
	t.Helper()
	hdr := &tar.Header{
		Name:     name,
		Typeflag: tar.TypeReg,
		Size:     int64(len(content)),
		Mode:     0755,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("tar write: %v", err)
	}
}

// makeDirFixture 造一个解压后的目录结构，含 sshmng 二进制 + LICENSE + README.md。
func makeDirFixture(t *testing.T, parentDir, name string, binaryContent []byte) string {
	t.Helper()
	dir := filepath.Join(parentDir, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	binaryName := "sshmng"
	if runtime.GOOS == "windows" {
		binaryName = "sshmng.exe"
	}
	if err := os.WriteFile(filepath.Join(dir, binaryName), binaryContent, 0755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("MIT"), 0644); err != nil {
		t.Fatalf("write LICENSE: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# sshmng"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	return dir
}

// readAllAndClose 读完 reader 并关闭，返回内容。
func readAllAndClose(t *testing.T, r io.ReadCloser) []byte {
	t.Helper()
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return data
}

func TestFindBinaryReader_TarGz(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake sshmng binary content")
	path := makeArchiveFixture(t, dir, "tar.gz", "v0.1.4", content)

	rc, err := findBinaryReader(path)
	if err != nil {
		t.Fatalf("findBinaryReader: %v", err)
	}
	got := readAllAndClose(t, rc)
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
}

func TestFindBinaryReader_Tar(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake sshmng binary content - tar version")
	path := makeArchiveFixture(t, dir, "tar", "v0.1.4", content)

	rc, err := findBinaryReader(path)
	if err != nil {
		t.Fatalf("findBinaryReader: %v", err)
	}
	got := readAllAndClose(t, rc)
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
}

func TestFindBinaryReader_Zip(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake sshmng binary content - zip version")
	path := makeArchiveFixture(t, dir, "zip", "v0.1.4", content)

	rc, err := findBinaryReader(path)
	if err != nil {
		t.Fatalf("findBinaryReader: %v", err)
	}
	got := readAllAndClose(t, rc)
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
}

func TestFindBinaryReader_Directory(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake sshmng binary content - extracted dir")
	extractedDir := makeDirFixture(t, dir, "sshmng-extracted", content)

	rc, err := findBinaryReader(extractedDir)
	if err != nil {
		t.Fatalf("findBinaryReader: %v", err)
	}
	got := readAllAndClose(t, rc)
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}
}

func TestFindBinaryReader_DirectoryMissingBinary(t *testing.T) {
	dir := t.TempDir()
	emptyDir := filepath.Join(dir, "empty")
	if err := os.MkdirAll(emptyDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 放一些无关文件
	os.WriteFile(filepath.Join(emptyDir, "LICENSE"), []byte("MIT"), 0644)
	os.WriteFile(filepath.Join(emptyDir, "README.md"), []byte("# sshmng"), 0644)

	_, err := findBinaryReader(emptyDir)
	if err == nil {
		t.Fatal("expected error for directory without sshmng binary")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found, got: %v", err)
	}
}

func TestFindBinaryReader_UnsupportedFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sshmng-v0.1.4-darwin-arm64.7z")
	if err := os.WriteFile(path, []byte("fake"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := findBinaryReader(path)
	if err == nil {
		t.Fatal("expected error for unsupported format")
	}
	if !strings.Contains(err.Error(), "unsupported asset format") {
		t.Errorf("error should mention unsupported format, got: %v", err)
	}
}

func TestFindBinaryReader_BinaryNotFoundInArchive(t *testing.T) {
	dir := t.TempDir()
	// 造一个不含 sshmng 的 tar.gz
	path := filepath.Join(dir, "sshmng-v0.1.4-"+runtime.GOOS+"-"+runtime.GOARCH+".tar.gz")
	f, _ := os.Create(path)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	writeTarEntry(t, tw, "wrong-binary", []byte("not sshmng"))
	tw.Close()
	gz.Close()
	f.Close()

	_, err := findBinaryReader(path)
	if err == nil {
		t.Fatal("expected error for archive without sshmng")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found, got: %v", err)
	}
}

func TestValidateAssetPlatform_TarGz(t *testing.T) {
	dir := t.TempDir()
	path := makeArchiveFixture(t, dir, "tar.gz", "v0.1.4", []byte("x"))
	if err := validateAssetPlatform(path); err != nil {
		t.Errorf("validateAssetPlatform: %v", err)
	}
}

func TestValidateAssetPlatform_Tar(t *testing.T) {
	dir := t.TempDir()
	path := makeArchiveFixture(t, dir, "tar", "v0.1.4", []byte("x"))
	if err := validateAssetPlatform(path); err != nil {
		t.Errorf("validateAssetPlatform: %v", err)
	}
}

func TestValidateAssetPlatform_DirectorySkipsCheck(t *testing.T) {
	dir := t.TempDir()
	extractedDir := makeDirFixture(t, dir, "extracted", []byte("x"))
	// 目录输入应跳过平台校验
	if err := validateAssetPlatform(extractedDir); err != nil {
		t.Errorf("validateAssetPlatform on dir should be nil, got: %v", err)
	}
}

func TestValidateAssetPlatform_WrongOS(t *testing.T) {
	dir := t.TempDir()
	wrongOS := "linux"
	if runtime.GOOS == "linux" {
		wrongOS = "darwin"
	}
	path := filepath.Join(dir, "sshmng-v0.1.4-"+wrongOS+"-"+runtime.GOARCH+".tar.gz")
	os.WriteFile(path, []byte("x"), 0600)

	err := validateAssetPlatform(path)
	if err == nil {
		t.Fatal("expected platform mismatch error")
	}
	if !strings.Contains(err.Error(), "platform mismatch") {
		t.Errorf("error should mention platform mismatch, got: %v", err)
	}
}

func TestValidateAssetPlatform_BadFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "random-archive.tar.gz")
	os.WriteFile(path, []byte("x"), 0600)

	err := validateAssetPlatform(path)
	if err == nil {
		t.Fatal("expected bad filename error")
	}
	if !strings.Contains(err.Error(), "doesn't match expected format") {
		t.Errorf("error should mention format, got: %v", err)
	}
	if !strings.Contains(err.Error(), "Safari") {
		t.Errorf("error should hint about Safari, got: %v", err)
	}
}

func TestAssetVersionFromName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"sshmng-v0.1.4-darwin-arm64.tar.gz", "v0.1.4"},
		{"sshmng-v1.2.3-linux-amd64.tar", "v1.2.3"},
		{"sshmng-v0.1.4-windows-amd64.zip", "v0.1.4"},
		{"sshmng-v0.1.4-rc1-darwin-arm64.tar.gz", "v0.1.4-rc1"},
		{"random.tar.gz", ""},
		{"/path/to/sshmng-v2.0.0-darwin-arm64.tar.gz", "v2.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := assetVersionFromName(tc.input)
			if got != tc.want {
				t.Errorf("assetVersionFromName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestUpdateFromFile_TarGz_AppliesBinary(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake sshmng binary - tar.gz path")
	assetPath := makeArchiveFixture(t, dir, "tar.gz", "v0.1.4", content)
	targetPath := filepath.Join(dir, "target-sshmng")
	// 创建一个空 target 文件，update.Apply 会替换它
	if err := os.WriteFile(targetPath, []byte("old content"), 0755); err != nil {
		t.Fatalf("write target: %v", err)
	}

	u := newTestUpdater(t, &mockSource{}, &mockLib{})
	u.log = testLogger(t)
	gotVersion, err := u.updateFromFileWithTarget(context.Background(), assetPath, targetPath)
	if err != nil {
		t.Fatalf("updateFromFileWithTarget: %v", err)
	}
	if gotVersion != "v0.1.4" {
		t.Errorf("version = %q, want v0.1.4", gotVersion)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Errorf("target not updated: got %q, want %q", data, content)
	}
}

func TestUpdateFromFile_Tar_AppliesBinary(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake sshmng binary - tar path (Safari extracted)")
	assetPath := makeArchiveFixture(t, dir, "tar", "v0.1.4", content)
	targetPath := filepath.Join(dir, "target-sshmng")
	os.WriteFile(targetPath, []byte("old"), 0755)

	u := newTestUpdater(t, &mockSource{}, &mockLib{})
	u.log = testLogger(t)
	_, err := u.updateFromFileWithTarget(context.Background(), assetPath, targetPath)
	if err != nil {
		t.Fatalf("updateFromFileWithTarget: %v", err)
	}
	data, _ := os.ReadFile(targetPath)
	if !bytes.Equal(data, content) {
		t.Errorf("target not updated via .tar path: got %q", data)
	}
}

func TestUpdateFromFile_Directory_AppliesBinary(t *testing.T) {
	dir := t.TempDir()
	content := []byte("fake sshmng binary - extracted dir path")
	extractedDir := makeDirFixture(t, dir, "sshmng-extracted", content)
	targetPath := filepath.Join(dir, "target-sshmng")
	os.WriteFile(targetPath, []byte("old"), 0755)

	u := newTestUpdater(t, &mockSource{}, &mockLib{})
	u.log = testLogger(t)
	gotVersion, err := u.updateFromFileWithTarget(context.Background(), extractedDir, targetPath)
	if err != nil {
		t.Fatalf("updateFromFileWithTarget: %v", err)
	}
	if gotVersion != "" {
		t.Errorf("version from dir should be empty, got %q", gotVersion)
	}
	data, _ := os.ReadFile(targetPath)
	if !bytes.Equal(data, content) {
		t.Errorf("target not updated via directory path: got %q", data)
	}
}

func TestUpdateFromFile_RejectsPlatformMismatch(t *testing.T) {
	dir := t.TempDir()
	wrongOS := "linux"
	if runtime.GOOS == "linux" {
		wrongOS = "darwin"
	}
	path := filepath.Join(dir, "sshmng-v0.1.4-"+wrongOS+"-"+runtime.GOARCH+".tar.gz")
	os.WriteFile(path, []byte("x"), 0600)

	u := newTestUpdater(t, &mockSource{}, &mockLib{})
	u.log = testLogger(t)
	_, err := u.updateFromFileWithTarget(context.Background(), path, filepath.Join(dir, "target"))
	if err == nil {
		t.Fatal("expected platform mismatch error")
	}
	if !strings.Contains(err.Error(), "platform mismatch") {
		t.Errorf("error should mention platform mismatch, got: %v", err)
	}
}

func TestUpdateFromFile_MissingFile(t *testing.T) {
	u := newTestUpdater(t, &mockSource{}, &mockLib{})
	u.log = testLogger(t)
	_, err := u.updateFromFileWithTarget(context.Background(), filepath.Join(t.TempDir(), "nonexistent.tar.gz"), "/tmp/target")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestUpdateFromFile_EmptyPath(t *testing.T) {
	u := newTestUpdater(t, &mockSource{}, &mockLib{})
	u.log = testLogger(t)
	_, err := u.updateFromFileWithTarget(context.Background(), "", "/tmp/target")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestWrapUpdateApplyError_PermissionDenied(t *testing.T) {
	err := fmt.Errorf("rename: permission denied")
	wrapped := wrapUpdateApplyError(err, "/usr/local/bin/sshmng")
	if !strings.Contains(wrapped.Error(), "not writable") {
		t.Errorf("should hint about writable, got: %v", wrapped)
	}
	if !strings.Contains(wrapped.Error(), "/usr/local/bin/sshmng") {
		t.Errorf("should mention path, got: %v", wrapped)
	}
}

func TestWrapUpdateApplyError_OtherError(t *testing.T) {
	err := fmt.Errorf("some other error")
	wrapped := wrapUpdateApplyError(err, "/tmp/sshmng")
	if strings.Contains(wrapped.Error(), "hint") {
		t.Errorf("should not add hint for non-permission error, got: %v", wrapped)
	}
}

// testLogger 返回写到 testing.T 的 logger，避免测试输出污染 stderr。
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
