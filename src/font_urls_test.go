package main

import (
	"strings"
	"testing"
)

// TestNotoSansSCURLsCurrentPath 守住字体路径：上游曾把 NotoSansSC 从
// Sans/Variable/TTF/ 移到 Sans/Variable/TTF/Subset/ 并把文件名改为 NotoSansSC-VF.ttf，
// 旧路径 404 时下载只是静默回退系统字体（用户看不出异常）。同时保证三个候选主机齐全，
// 任一 CDN 不可达时仍有回退。
func TestNotoSansSCURLsCurrentPath(t *testing.T) {
	const wantPath = "Sans/Variable/TTF/Subset/NotoSansSC-VF.ttf"
	if len(notoSansSCURLs) < 3 {
		t.Fatalf("字体候选源过少（%d 个），CDN 单点故障无从回退", len(notoSansSCURLs))
	}
	hosts := map[string]bool{}
	for _, u := range notoSansSCURLs {
		if !strings.Contains(u, wantPath) {
			t.Errorf("字体 URL 未指向当前上游路径 %s：%s", wantPath, u)
		}
		if strings.Contains(u, "NotoSansSC%5Bwght%5D") || strings.Contains(u, "NotoSansCJKsc") {
			t.Errorf("字体 URL 使用已失效/不匹配 family 的文件：%s", u)
		}
		for _, h := range []string{"cdn.jsdelivr.net", "raw.githubusercontent.com", "github.com"} {
			if strings.Contains(u, h) {
				hosts[h] = true
			}
		}
	}
	for _, h := range []string{"cdn.jsdelivr.net", "raw.githubusercontent.com", "github.com"} {
		if !hosts[h] {
			t.Errorf("缺少候选源主机：%s", h)
		}
	}
}
