package main

// ==================== 后台服务端口可用性预检 ====================
//
// 背景（2026-09-28 现场）：Windows 会把一段动态端口划入「排除端口段」（Hyper-V / WinNAT / WSL
// 在开机时保留，`netsh int ipv4 show excludedportrange protocol=tcp` 可见）。落在段内的端口
// bind() 直接返回 WSAEACCES，node 侧表现为
//
//	Error: listen EACCES: permission denied 127.0.0.1:3080
//
// 且 `dsh web` 进程立刻退出。此时：
//   - 失败原因与「插件/版本不兼容」毫无关系，但旧收尾会去做 disable-all 用户插件 + LKG 回退
//     （现场：三次启动把全部用户插件禁用又恢复，最后「启动失败自动回退也失败」）；
//   - 用户只看到「服务进程异常退出」，看不到「端口被系统保留」。
//
// 因此这里在 spawn 之前先做一次真实 bind 预检，把「端口不可用」与「插件加载失败」彻底分开，
// 并给用户一条一键换端口的恢复路径（见 main.go 的 promptPortChange）。
//
// 平台差异（go1.27 实测）：
//   - Windows：占用 = errno 10048（WSAEADDRINUSE），排除端口段 = errno 10013（WSAEACCES）；
//     **`errors.Is(err, syscall.EADDRINUSE/EACCES)` 在 Windows 上恒为 false**（syscall 里那两个
//     是 C 运行时的 100/13），所以必须按数值直判，POSIX 分支只为 darwin 保留。

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

// portErrKind 端口预检结论。
type portErrKind int

const (
	portOK       portErrKind = iota // 可绑定
	portBlocked                     // 被系统保留 / 无权限——只能换端口
	portInUse                       // 已被其它进程监听——可尝试停掉占用者
	portBindFail                    // 其它绑定失败（地址非法等），与「被拦住」同样处理
)

func (k portErrKind) String() string {
	switch k {
	case portOK:
		return "ok"
	case portBlocked:
		return "blocked"
	case portInUse:
		return "in-use"
	default:
		return "bind-failed"
	}
}

// ServiceState.FailKind 的取值：前端据此把「端口不可用」与普通启动失败分开渲染
// （见 app.go GetServiceState、frontend/dist/main.js 的 updatePortBlockedHint）。
const (
	failKindPortBlocked = "port-blocked"
	failKindPortInUse   = "port-in-use"
)

// freePortBase 推荐端口起点。取 15000 以上：Windows 动态端口范围默认 1024-15000，排除端口段
// 正是从这个范围里划走的，落在范围外可基本规避这一类冲突（本机 3004-3103 即为一例）。
const freePortBase = 18080

// wsaEACCES / wsaEADDRINUSE：Windows 的 WSA 错误码（见文件头平台差异说明）。
const (
	wsaEACCES     = syscall.Errno(10013)
	wsaEADDRINUSE = syscall.Errno(10048)
)

// isForbiddenBindErr 绑定失败是否属于「系统不允许」（保留端口段 / 无权限）。
func isForbiddenBindErr(err error) bool {
	if err == nil {
		return false
	}
	// darwin / POSIX
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == wsaEACCES
}

// isAddrInUseErr 绑定失败是否属于「已被占用」。
func isAddrInUseErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) { // darwin / POSIX
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == wsaEADDRINUSE
}

// probePort 在 127.0.0.1:port 上真实 bind 一次并立即释放，返回端口当前可用性。
// 只测监听地址（托盘服务固定绑回环）；未 accept 过连接，释放后不留 TIME_WAIT。
func probePort(p int) (portErrKind, error) {
	if p <= 0 || p > 65535 {
		return portBindFail, fmt.Errorf("端口号非法：%d", p)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err == nil {
		_ = ln.Close()
		return portOK, nil
	}
	switch {
	case isForbiddenBindErr(err):
		return portBlocked, err
	case isAddrInUseErr(err):
		return portInUse, err
	}
	// 兜底（错误码超出上面两类）：端口上确实有监听者 → 占用；否则视为被系统拦住。
	if pid, perr := findListenerPID(p); perr == nil && pid > 0 {
		return portInUse, err
	}
	return portBlocked, err
}

// failKindFor 端口预检结论 → ServiceState.FailKind（前端契约，勿改字面量）。
func failKindFor(k portErrKind) string {
	switch k {
	case portInUse:
		return failKindPortInUse
	case portBlocked, portBindFail:
		return failKindPortBlocked
	}
	return ""
}

// portFailReason 端口不可用的用户可读原因（托盘状态行 / 设置页副标题 / 弹窗共用）。
// 与既有失败原因文案一致（main.go 里 "服务启动失败（…）" 系列不走 i18n）。
func portFailReason(kind string, p int) string {
	if kind == failKindPortInUse {
		return fmt.Sprintf("端口 %d 已被其它程序占用，后台服务无法启动", p)
	}
	return fmt.Sprintf("端口 %d 被系统保留（Windows 排除端口段），后台服务无法启动", p)
}

// pickFreePort 选一个可绑定的端口：hint 可用则沿用，否则从 freePortBase 顺序探测；
// 全部失败时让系统分配（bind :0 不会给出被排除/占用的端口）。返回 0 表示没找到。
func pickFreePort(hint int) int {
	if hint > 0 && hint <= 65535 {
		if kind, _ := probePort(hint); kind == portOK {
			return hint
		}
	}
	for p := freePortBase; p < freePortBase+200 && p <= 65535; p++ {
		if kind, _ := probePort(p); kind == portOK {
			return p
		}
	}
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		p := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		return p
	}
	return 0
}
