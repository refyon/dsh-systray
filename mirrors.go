package main

// ==================== 工具下载：多镜像自动切换（共享） ====================
// 各平台在下载运行时/工具（Node、pnpm 等）时一律经由本文件组装候选源并逐源尝试：
// 单源连接/整体超时即自动切换下一镜像（日志记录失败源），全部失败返回汇总错误。
// 均支持环境变量固定单一来源（便于用户/企业网络自行指定内网镜像）。

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// mirrorEnvRoots 组装镜像根列表：envName 设置时仅用该镜像根（用户显式指定），
// 否则返回 defaults。根不含尾部 "/"。
func mirrorEnvRoots(envName string, defaults []string) []string {
	if e := strings.TrimRight(os.Getenv(envName), "/"); e != "" {
		return []string{e}
	}
	out := make([]string, 0, len(defaults))
	for _, d := range defaults {
		out = append(out, strings.TrimRight(d, "/"))
	}
	return out
}

// nodeDistBases Node.js 发行版镜像根（依次尝试官方与常用国内镜像）。
func nodeDistBases() []string {
	return mirrorEnvRoots("DSH_NODE_MIRROR", []string{
		"https://nodejs.org/dist",
		"https://npmmirror.com/mirrors/node",
		"https://mirrors.huaweicloud.com/nodejs",
	})
}

// npmRegistryBases npm registry 镜像（环境变量 DSH_NPM_REGISTRY 指定单一来源；
// 默认 npmmirror 优先（国内/CDN 均可达且快），官方兜底）。
func npmRegistryBases() []string {
	return mirrorEnvRoots("DSH_NPM_REGISTRY", []string{
		"https://registry.npmmirror.com",
		"https://registry.npmjs.org",
	})
}

// urlHost 取 URL 主机名（展示用）。
func urlHost(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, "/"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return u
}

// mirrorDownload 依次尝试 urls 下载到 dest；单源整体超时 perSource 即切下一源。
// onPct 每次收到进度回调（host=当前源，pct 0..1，源间重置）；可为 nil。
// 成功返回 nil 并清理旧文件；全部失败返回含各源原因的汇总错误（残留部分文件已删除）。
func mirrorDownload(urls []string, dest string, label string, perSource time.Duration,
	onPct func(host string, pct float64)) error {
	var errs []string
	for _, u := range urls {
		host := urlHost(u)
		log.Printf("download %s: trying %s", label, u)
		err := downloadSingle(u, dest, perSource, func(pct float64) {
			if onPct != nil {
				onPct(host, pct)
			}
		})
		if err == nil {
			log.Printf("download %s: ok from %s", label, host)
			return nil
		}
		_ = os.Remove(dest) // 清掉残片，避免影响下一源/后续判定
		log.Printf("download %s: %s failed: %v", label, host, err)
		errs = append(errs, fmt.Sprintf("%s: %v", host, err))
	}
	return fmt.Errorf("下载 %s 失败（已尝试 %d 个来源）：%s", label, len(urls), strings.Join(errs, "；"))
}

// downloadSingle 单源下载（带整体超时与进度回调）。
func downloadSingle(url, dest string, timeout time.Duration, onPct func(pct float64)) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{}
	resp, err := client.Do(req)
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
			if onPct != nil && total > 0 {
				onPct(float64(done) / float64(total))
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
