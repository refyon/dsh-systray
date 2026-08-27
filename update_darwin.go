//go:build darwin

package main

// updateSupported macOS 暂不支持自替换升级（.app 包结构不同），返回 false 跳过更新检查。
func updateSupported() bool { return false }

// confirmUpdate macOS 占位：updateSupported=false 时不会走到这里。
func confirmUpdate(newVersion string) bool { return false }

// spawnSwapper macOS 占位。
func spawnSwapper(newExePath string) {}

// runUpdateSwap macOS 占位。
func runUpdateSwap(oldPID int, target string) {}
