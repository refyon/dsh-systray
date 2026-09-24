package main

import (
	"strings"
	"testing"
)

// pinInstallRegistry 把 installRegistry() 的探测结果钉死（进程内缓存），
// 使环境变量相关的单测不发真实网络请求。
func pinInstallRegistry(t *testing.T, reg string) {
	t.Helper()
	installRegistryMu.Lock()
	prevDone, prevVal := installRegistryDone, installRegistryVal
	installRegistryDone, installRegistryVal = true, reg
	installRegistryMu.Unlock()
	t.Cleanup(func() {
		installRegistryMu.Lock()
		installRegistryDone, installRegistryVal = prevDone, prevVal
		installRegistryMu.Unlock()
	})
}

// envValue 取 "KEY=VALUE" 列表里 KEY 的值。
func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// TestPnpmTunedEnvDualPrefix pnpm 配置必须**同时**写两个前缀：
// pnpm 10.34.5（托盘自带运行时）只认 npm_config_*，pnpm 11.7.0 只认 pnpm_config_*
// （2026-09-24 实测；旧实现只写 npm_config_*，在 pnpm ≥ 11 上静默失效）。
func TestPnpmTunedEnvDualPrefix(t *testing.T) {
	pinInstallRegistry(t, npmMirrorRegistry)
	env := pnpmTunedEnv()
	for _, key := range []string{
		"child_concurrency", "package_import_method", "side_effects_cache",
		"fetch_retries", "fetch_timeout", "prefer_offline",
	} {
		for _, prefix := range []string{"npm_config_", "pnpm_config_"} {
			if got := envValue(env, prefix+key); got == "" {
				t.Errorf("%s%s 缺失（%v）", prefix, key, env)
			}
		}
	}
	if got := envValue(env, "npm_config_prefer_offline"); got != "true" {
		t.Fatalf("npm_config_prefer_offline = %q, want true", got)
	}
	for _, k := range []string{"npm_config_registry", "pnpm_config_registry"} {
		if got := envValue(env, k); got != npmMirrorRegistry {
			t.Fatalf("%s = %q, want %q（首选镜像）", k, got, npmMirrorRegistry)
		}
	}
}

// TestPnpmTunedEnvWithRegistry 指定 registry 必须**替换**而非追加同名变量
// （重复键的行为依赖 exec 环境的去重实现，不能依赖），两个前缀都要替换。
func TestPnpmTunedEnvWithRegistry(t *testing.T) {
	pinInstallRegistry(t, npmMirrorRegistry)
	env := pnpmTunedEnvWithRegistry(npmOfficialRegistry)
	for _, prefix := range []string{"npm_config_registry", "pnpm_config_registry"} {
		n := 0
		for _, kv := range env {
			if strings.HasPrefix(kv, prefix+"=") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s 出现 %d 次，want 1（%v）", prefix, n, env)
		}
		if got := envValue(env, prefix); got != npmOfficialRegistry {
			t.Fatalf("%s = %q, want %q", prefix, got, npmOfficialRegistry)
		}
	}
	// 空 registry 表示沿用首选
	for _, prefix := range []string{"npm_config_registry", "pnpm_config_registry"} {
		if got := envValue(pnpmTunedEnvWithRegistry(""), prefix); got != npmMirrorRegistry {
			t.Fatalf("空 registry: %s = %q, want %q", prefix, got, npmMirrorRegistry)
		}
	}
}

// TestPnpmHarnessEnvRegistryOverride harness 目录 pnpm 命令：默认首选 registry，
// 预检发现镜像尚未同步目标版本时由 override 切官方（见 harnessRegistryForVersion）。
func TestPnpmHarnessEnvRegistryOverride(t *testing.T) {
	pinInstallRegistry(t, npmMirrorRegistry)
	prev := harnessRegistryOverride
	t.Cleanup(func() { harnessRegistryOverride = prev })

	for _, prefix := range []string{"npm_config_registry", "pnpm_config_registry"} {
		harnessRegistryOverride = ""
		if got := envValue(pnpmHarnessEnv(), prefix); got != npmMirrorRegistry {
			t.Fatalf("默认 %s = %q, want %q", prefix, got, npmMirrorRegistry)
		}
		harnessRegistryOverride = npmOfficialRegistry
		if got := envValue(pnpmHarnessEnv(), prefix); got != npmOfficialRegistry {
			t.Fatalf("override %s = %q, want %q", prefix, got, npmOfficialRegistry)
		}
	}
	if got := envValue(pnpmHarnessEnv(), "pnpm_config_prefer_offline"); got != "true" {
		t.Fatalf("pnpm_config_prefer_offline = %q, want true", got)
	}
	// harness 侧不带 profile 的重度调优档（PNPM_MAX_WORKERS 等只服务 profile 的文件占用问题）
	if got := envValue(pnpmHarnessEnv(), "PNPM_MAX_WORKERS"); got != "" {
		t.Fatalf("harness env 不应带 PNPM_MAX_WORKERS，got %q", got)
	}
	if got := envValue(pnpmHarnessEnv(), "pnpm_config_child_concurrency"); got != "" {
		t.Fatalf("harness env 不应带 child_concurrency，got %q", got)
	}
}

// TestHarnessCmdEnvOnlyForPnpm 只有 pnpm 命令（按可执行文件名判定）才追加 registry 环境，
// git 等命令返回 nil 原样继承进程环境。
func TestHarnessCmdEnvOnlyForPnpm(t *testing.T) {
	pinInstallRegistry(t, npmMirrorRegistry)
	prev := harnessRegistryOverride
	t.Cleanup(func() { harnessRegistryOverride = prev })
	for _, name := range []string{"pnpm", "pnpm.cmd", "pnpm.exe", `C:\tools\pnpm.cmd`} {
		env := harnessCmdEnv(name)
		if envValue(env, "npm_config_registry") == "" || envValue(env, "pnpm_config_registry") == "" {
			t.Errorf("harnessCmdEnv(%q) 缺少 registry 环境（%v）", name, env)
		}
	}
	for _, name := range []string{"git", "git.exe", `C:\Program Files\Git\cmd\git.exe`, ""} {
		if env := harnessCmdEnv(name); env != nil {
			t.Errorf("harnessCmdEnv(%q) 应返回 nil，got %v", name, env)
		}
	}
}
