// envproxy.go：代理相关环境变量与「默认 Transport」初始化。
package main

import (
	"os"
	"strings"
)

// envProxySettingName 显式代理配置的环境变量名（优先级高于 config.json 的 proxy）。
const envProxySettingName = "DSH_SYSTRAY_PROXY"

// envProxySetting 读取显式代理配置环境变量（跨平台部分）。
func envProxySetting() string {
	return os.Getenv(envProxySettingName)
}

// proxyEnvHint 返回当前进程可见的标准代理环境变量摘要（排障日志用；无则空串）。
// 不打印可能含凭据的完整取值之外的额外信息，仅按需列举变量名。
func proxyEnvHint() string {
	var names []string
	for _, n := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "NO_PROXY", "no_proxy"} {
		if strings.TrimSpace(os.Getenv(n)) != "" {
			names = append(names, n)
		}
	}
	return strings.Join(names, ",")
}
