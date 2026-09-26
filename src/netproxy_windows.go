// netproxy_windows.go：读取 Windows「系统代理」设置（WinINET / Internet Settings）。
//
// 用标准库 syscall 直接读注册表，不引入额外依赖（x/sys/windows/registry 需要新增模块，
// 而这里只读两个字符串值）。
//
// 覆盖范围与浏览器一致：
//
//	HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings
//	  ProxyEnable   REG_DWORD 1 = 启用
//	  ProxyServer   REG_SZ    host:port 或 http=h:80;https=h2:443
//	  ProxyOverride REG_SZ    绕过清单（; 分隔，支持 * 通配；<local> 表示本机）
//	HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings\Connections
//	  DefaultConnectionSettings  LAN 连接的二进制设置（ProxyEnable/ProxyServer 未写入时兜底）
//
// 很多程序（如 v2rayN/Clash 的「系统代理」开关）只写 Internet Settings 根键；
// 少数场景（组策略下发、企业 PAC）写的是 Connections 子键，故保留兜底解析。
//
//go:build windows

package main

import (
	"encoding/binary"
	"strings"
	"syscall"
	"unsafe"
)

const (
	winINetKeyPath  = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	winINetConnPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings\Connections`
	winConnValue    = "DefaultConnectionSettings"

	regSZ     = 1
	regDWORD  = 4
	regBinary = 3
)

// readSystemProxy 读取 Windows 系统代理设置；未启用或读不到时 ok=false。
func readSystemProxy() (systemProxyConfig, bool) {
	server, _ := regQueryString(syscall.HKEY_CURRENT_USER, winINetKeyPath, "ProxyServer")
	override, _ := regQueryString(syscall.HKEY_CURRENT_USER, winINetKeyPath, "ProxyOverride")
	enabled, haveEnable := regQueryDWORD(syscall.HKEY_CURRENT_USER, winINetKeyPath, "ProxyEnable")

	if (!haveEnable || enabled == 0) && strings.TrimSpace(server) == "" {
		// 根键没有可用信息：尝试 LAN 连接设置（组策略 / 旧版写入形态）
		if s, o, en, ok := parseConnectionSettings(); ok {
			server, override, enabled, haveEnable = s, o, en, true
		}
	}
	// 有地址才算启用；ProxyEnable 缺省时按「有地址即启用」处理（部分工具只写地址）。
	active := strings.TrimSpace(server) != "" && (!haveEnable || enabled != 0)
	return parseSystemProxy(active, server, override)
}

// parseConnectionSettings 解析 DefaultConnectionSettings 二进制：
//
//	offset 0  : DWORD 版本（≥ 46 才有 bypass 字段）
//	offset 4  : DWORD flags（bit0 = 直连，bit1 = 启用代理，bit2 = 自动配置脚本）
//	offset 8  : DWORD 代理服务器字符串长度
//	offset 12 : 代理服务器（ANSI，形如 host:port 或 http=h:80;https=h2:443）
//	其后      : DWORD 绕过清单长度 + 内容（ANSI，; 分隔）
func parseConnectionSettings() (server, override string, enabled uint32, ok bool) {
	raw, err := regQueryBinary(syscall.HKEY_CURRENT_USER, winINetConnPath, winConnValue)
	if err != nil || len(raw) < 16 {
		return "", "", 0, false
	}
	flags := binary.LittleEndian.Uint32(raw[4:8])
	if flags&0x2 == 0 {
		return "", "", 0, false // 未启用显式代理
	}
	pos := 12
	if n := int(binary.LittleEndian.Uint32(raw[8:12])); n > 0 && pos+n <= len(raw) {
		server = string(raw[pos : pos+n])
		pos += n
	}
	if pos+4 <= len(raw) {
		if n := int(binary.LittleEndian.Uint32(raw[pos : pos+4])); n > 0 && pos+4+n <= len(raw) {
			override = string(raw[pos+4 : pos+4+n])
		}
	}
	if strings.TrimSpace(server) == "" {
		return "", "", 0, false
	}
	return server, override, 1, true
}

// regQueryString 读 REG_SZ 值；键/值不存在或类型不符返回错误。
func regQueryString(root syscall.Handle, path, name string) (string, error) {
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(root, syscall.StringToUTF16Ptr(path), 0, syscall.KEY_READ, &h); err != nil {
		return "", err
	}
	defer syscall.RegCloseKey(h)
	var (
		typ  uint32
		size uint32 = 4096
	)
	buf := make([]uint16, size/2)
	if err := syscall.RegQueryValueEx(h, syscall.StringToUTF16Ptr(name), nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size); err != nil {
		return "", err
	}
	if typ != regSZ {
		return "", syscall.ERROR_FILE_NOT_FOUND
	}
	// RegQueryValueEx 返回的 size 含结尾 NUL，按字节计
	n := int(size) / 2
	if n > 0 && n <= len(buf) && buf[n-1] == 0 {
		n--
	}
	if n <= 0 || n > len(buf) {
		return "", nil
	}
	return syscall.UTF16ToString(buf[:n]), nil
}

// regQueryDWORD 读 REG_DWORD 值。
func regQueryDWORD(root syscall.Handle, path, name string) (uint32, bool) {
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(root, syscall.StringToUTF16Ptr(path), 0, syscall.KEY_READ, &h); err != nil {
		return 0, false
	}
	defer syscall.RegCloseKey(h)
	var (
		typ  uint32
		val  uint32
		size uint32 = 4
	)
	if err := syscall.RegQueryValueEx(h, syscall.StringToUTF16Ptr(name), nil, &typ, (*byte)(unsafe.Pointer(&val)), &size); err != nil || typ != regDWORD {
		return 0, false
	}
	return val, true
}

// regQueryBinary 读 REG_BINARY 值。
func regQueryBinary(root syscall.Handle, path, name string) ([]byte, error) {
	var h syscall.Handle
	if err := syscall.RegOpenKeyEx(root, syscall.StringToUTF16Ptr(path), 0, syscall.KEY_READ, &h); err != nil {
		return nil, err
	}
	defer syscall.RegCloseKey(h)
	var (
		typ  uint32
		size uint32 = 8192
	)
	buf := make([]byte, size)
	if err := syscall.RegQueryValueEx(h, syscall.StringToUTF16Ptr(name), nil, &typ, &buf[0], &size); err != nil {
		return nil, err
	}
	if typ != regBinary && typ != regSZ {
		return nil, syscall.ERROR_FILE_NOT_FOUND
	}
	if int(size) > len(buf) {
		size = uint32(len(buf))
	}
	return buf[:size], nil
}
