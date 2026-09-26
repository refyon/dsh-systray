// netproxy_other.go：非 Windows 平台的系统代理读取（macOS / Linux）。
//
// 与 Windows 不同，这两个平台没有统一「系统代理」注册表：macOS 走 SystemConfiguration
// 框架（scutil --proxy）、Linux 走桌面环境各自设置。而 Go 在非 Windows 上读取
// HTTP_PROXY/HTTPS_PROXY 环境变量正是这两个平台约定俗成的「系统代理」入口，
// 故此处返回「无系统代理」，由 outboundProxyFunc 里的环境变量分支接管
// （config.json 的 proxy 也可显式指定）。
//
//go:build !windows

package main

// readSystemProxy 非 Windows 平台不额外探测系统代理。
func readSystemProxy() (systemProxyConfig, bool) {
	return systemProxyConfig{}, false
}
