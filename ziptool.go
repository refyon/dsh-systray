package main

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ==================== 解压工具（7-Zip）检测与下载 ====================
// 启动环境检查的一部分：优先使用 7zip；本机已装则直接用，未装则下载便携版到用户目录，
// 无管理员权限、后台静默。下载失败不阻塞启动（zip 读写有 Go 原生兜底）。

// archiveToolsDir 便携解压工具的存放目录。
func archiveToolsDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "dsh-systray", "tools")
}

// archiveToolCache 探测结果缓存（空串=尚未探测成功）。
var archiveToolCache string

// findArchiveTool 查找 7z 系列可执行文件：先便携目录，再系统 PATH。
func findArchiveTool() string {
	if archiveToolCache != "" {
		return archiveToolCache
	}
	for _, name := range []string{"7zz", "7za", "7z"} {
		p := filepath.Join(archiveToolsDir(), name)
		if runtime.GOOS == "windows" {
			p += ".exe"
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			archiveToolCache = p
			return p
		}
	}
	for _, name := range []string{"7zz", "7za", "7z"} {
		if p, err := exec.LookPath(name); err == nil {
			archiveToolCache = p
			return p
		}
	}
	return ""
}

// archiveToolDownloadURLs 各平台的便携 7zip 下载地址（依次尝试）。
func archiveToolDownloadURLs() []string {
	if runtime.GOOS == "windows" {
		// 独立控制台版 7za.exe（zip 格式可直接解压，支持 zip 读写）
		return []string{"https://www.7-zip.org/a/7za920.zip"}
	}
	// macOS 控制台版 7zz（tar.xz，系统 bsdtar 可直接解压）
	return []string{
		"https://www.7-zip.org/a/7z2301-mac.tar.xz",
		"https://github.com/ip7z/7zip/releases/download/26.02/7z2602-mac.tar.xz",
	}
}

// downloadArchiveTool 下载便携 7zip（带进度回调）。失败返回错误。
func downloadArchiveTool(destDir string, onStatus func(text string, pct float64)) (string, error) {
	var lastErr error
	for _, url := range archiveToolDownloadURLs() {
		tmp, err := os.CreateTemp(destDir, "7zip-dl-*")
		if err != nil {
			return "", err
		}
		tmpPath := tmp.Name()
		tmp.Close()
		progress(onStatus, "正在下载解压工具 7-Zip…", 0)
		if err := downloadWithProgress(url, tmpPath, func(p float64) {
			progress(onStatus, "正在下载解压工具 7-Zip…", p)
		}); err != nil {
			lastErr = err
			_ = os.Remove(tmpPath)
			log.Printf("archive tool download %s failed: %v", url, err)
			continue
		}
		toolPath, err := unpackArchiveTool(tmpPath, destDir)
		_ = os.Remove(tmpPath)
		if err != nil {
			lastErr = err
			log.Printf("archive tool unpack failed: %v", err)
			continue
		}
		progress(onStatus, "", 1)
		return toolPath, nil
	}
	return "", lastErr
}

// unpackArchiveTool 从下载的包中取出 7z 可执行文件放入 destDir，返回其路径。
// Windows：7za920.zip（用 Go 原生 zip 解出 7za.exe）；macOS：tar.xz（系统 tar 解出 7zz）。
func unpackArchiveTool(pkgPath, destDir string) (string, error) {
	if runtime.GOOS == "windows" {
		zr, err := zip.OpenReader(pkgPath)
		if err != nil {
			return "", fmt.Errorf("打开 7zip 包失败：%w", err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			name := filepath.Base(f.Name)
			if !strings.EqualFold(name, "7za.exe") {
				continue
			}
			if err := copyZipEntryToDir(f, destDir, name); err != nil {
				return "", err
			}
			return filepath.Join(destDir, "7za.exe"), nil
		}
		return "", fmt.Errorf("7zip 包中未找到 7za.exe")
	}
	// macOS：tar -xf（bsdtar 原生支持 xz）
	extractDir, err := os.MkdirTemp(destDir, "7zip-extract-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(extractDir)
	cmd := exec.Command("tar", "-xf", pkgPath, "-C", extractDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("解压 7zip 包失败：%v: %s", err, strings.TrimSpace(string(out)))
	}
	var found string
	_ = filepath.Walk(extractDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found != "" {
			return nil
		}
		if strings.HasPrefix(info.Name(), "7zz") {
			found = p
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("7zip 包中未找到 7zz")
	}
	dest := filepath.Join(destDir, "7zz")
	data, err := os.ReadFile(found)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return "", err
	}
	return dest, nil
}

// copyZipEntryToDir 把 zip 条目按 basename 写到 destDir。
func copyZipEntryToDir(f *zip.File, destDir, outName string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(filepath.Join(destDir, outName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// downloadWithProgress 下载文件（进度 0-1）。25 分钟超时。
func downloadWithProgress(url, dest string, onProgress func(pct float64)) error {
	client := &http.Client{Timeout: 25 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 256*1024)
	var done int64
	total := resp.ContentLength
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if onProgress != nil && total > 0 {
				onProgress(float64(done) / float64(total))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

// ensureArchiveTool 确保有可用的 7z 解压工具：已有→直接用；没有→下载便携版（失败返回错误，不阻塞启动）。
func ensureArchiveTool(onStatus func(text string, pct float64)) error {
	if findArchiveTool() != "" {
		log.Printf("archive tool available: %s", findArchiveTool())
		progress(onStatus, "", 1)
		return nil
	}
	dir := archiveToolsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("archive tools dir create failed: %v", err)
		progress(onStatus, "", 0)
		return err
	}
	toolPath, err := downloadArchiveTool(dir, onStatus)
	if err != nil {
		log.Printf("archive tool download failed: %v (zip fallback enabled)", err)
		progress(onStatus, "", 0)
		return err
	}
	archiveToolCache = toolPath
	log.Printf("archive tool installed: %s", toolPath)
	return nil
}

// ==================== zip 打包（Go 原生，跟随符号链接） ====================

type packEntry struct {
	name string // zip 内相对路径（/ 分隔）
	src  string // 实际文件路径
	size int64
	dir  bool
}

// collectPack 递归收集要打包的条目：目录内容打包到 name/ 下；符号链接/联接点跟随目标打包。
// resolved 用于环路检测（按解析后的真实路径），depth/entryCount 防异常树失控。
func collectPack(name, src string, out *[]packEntry, resolved map[string]bool, depth int, entryCount *int) error {
	if *entryCount > 500000 {
		return nil
	}
	info, err := os.Stat(src) // 跟随符号链接
	if err != nil {
		return nil // 损坏的链接：跳过
	}
	if info.IsDir() {
		if depth > 40 {
			return nil
		}
		real, err := filepath.EvalSymlinks(src)
		if err != nil {
			real = src
		}
		if resolved[real] {
			return nil // 环路：跳过
		}
		resolved[real] = true
		*out = append(*out, packEntry{name: strings.TrimSuffix(name, "/") + "/", src: src, dir: true})
		ents, err := os.ReadDir(src)
		if err != nil {
			return nil
		}
		for _, e := range ents {
			*entryCount++
			if err := collectPack(name+"/"+e.Name(), filepath.Join(src, e.Name()), out, resolved, depth+1, entryCount); err != nil {
				return err
			}
		}
		return nil
	}
	*out = append(*out, packEntry{name: name, src: src, size: info.Size()})
	return nil
}

// zipCreate 把 entries（zip内路径 → 文件系统路径）打包为一个 zip。onProgress 为总字节进度 0-1（可空）。
func zipCreate(zipPath string, entries map[string]string, onProgress func(pct float64)) error {
	var all []packEntry
	resolved := map[string]bool{}
	count := 0
	for name, src := range entries {
		if err := collectPack(strings.Trim(name, "/"), src, &all, resolved, 0, &count); err != nil {
			return err
		}
	}
	var total int64
	for _, e := range all {
		total += e.size
	}
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	var done int64
	report := func() {
		if onProgress != nil && total > 0 {
			pct := float64(done) / float64(total)
			if pct > 1 {
				pct = 1
			}
			onProgress(pct)
		}
	}
	for _, e := range all {
		if e.dir {
			// 目录条目也写入 zip（保留空目录，供冲突检测/恢复时重建结构）
			hdr := &zip.FileHeader{Name: e.name}
			hdr.SetMode(os.ModeDir | 0o755)
			if _, err := zw.CreateHeader(hdr); err != nil {
				zw.Close()
				f.Close()
				return err
			}
			continue
		}
		sf, err := os.Open(e.src)
		if err != nil {
			zw.Close()
			f.Close()
			return fmt.Errorf("读取 %s 失败：%w", e.src, err)
		}
		info, serr := sf.Stat()
		if serr != nil {
			sf.Close()
			continue
		}
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		hdr.Modified = info.ModTime()
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			sf.Close()
			zw.Close()
			f.Close()
			return err
		}
		n, err := io.Copy(w, sf)
		sf.Close()
		if err != nil {
			zw.Close()
			f.Close()
			return fmt.Errorf("写入 %s 失败：%w", e.name, err)
		}
		done += n
		report()
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ==================== zip 解压（优先 7z，Go 兜底） ====================

// validateZipSafe 校验 zip 内条目路径安全：不允许绝对路径、盘符、越出目标目录的 ..。
func validateZipSafe(zipPath string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("无法打开压缩包：%w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := f.Name
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
			return fmt.Errorf("压缩包包含非法绝对路径条目：%s", name)
		}
		if len(name) >= 2 && name[1] == ':' {
			return fmt.Errorf("压缩包包含非法盘符路径条目：%s", name)
		}
		clean := filepath.Clean(filepath.FromSlash(name))
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("压缩包包含越界路径条目：%s", name)
		}
	}
	return nil
}

// zipExtract 解压 zip 到 destDir：优先使用 7z（overwrite=true 覆盖 / false 跳过已有），失败回退 Go 原生。
func zipExtract(zipPath, destDir string, overwrite bool) error {
	if err := validateZipSafe(zipPath); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	if tool := findArchiveTool(); tool != "" {
		args := []string{"x", "-y", "-tzip"}
		if overwrite {
			args = append(args, "-aoa")
		} else {
			args = append(args, "-aos")
		}
		args = append(args, "-o"+destDir, zipPath)
		cmd := exec.Command(tool, args...)
		hideCmdWindow(cmd)
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else {
			log.Printf("7z extract failed (%s), using Go fallback: %s", strings.TrimSpace(string(out)), err)
		}
	}
	return goExtractZip(zipPath, destDir, overwrite)
}

// goExtractZip Go 原生解压（zip 格式，overwrite=false 时跳过已存在文件）。
func goExtractZip(zipPath, destDir string, overwrite bool) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if name == "." || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			continue // 非法条目（validateZipSafe 已拦）
		}
		target := filepath.Join(destDir, name)
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(target, 0o755)
			continue
		}
		if !overwrite {
			if _, err := os.Stat(target); err == nil {
				continue // 跳过已有
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, werr := io.Copy(out, rc)
		rc.Close()
		cerr := out.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

// ==================== zip 读取 ====================

// zipListNames 列出 zip 内全部条目名。
func zipListNames(zipPath string) ([]string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names, nil
}

// zipReadFile 读取 zip 内单个文本条目。
func zipReadFile(zipPath, name string) ([]byte, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 16<<20))
	}
	return nil, fmt.Errorf("压缩包中未找到 %s", name)
}

// newExportUUID 8 字节随机数的小写十六进制串（用于导出包命名）。
func newExportUUID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
