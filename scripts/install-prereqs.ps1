# dsh-systray 运行依赖安装脚本（以管理员身份运行）
$ErrorActionPreference = 'Continue'
$harnessDir    = '{{HARNESS_DIR}}'
if ($harnessDir -eq '{{HARNESS_DIR}}') { $harnessDir = Join-Path $env:USERPROFILE 'deepseek-harness' }
$harnessRepo   = 'https://github.com/deepseek-ai/deepseek-harness.git'
$harnessBranch = 'master'

function Test-Cmd($n) { return $null -ne (Get-Command $n -ErrorAction SilentlyContinue) }
function Refresh-Path {
    $env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
}

# 1) Git（拉取 harness 源码需要）
if (-not (Test-Cmd 'git')) {
    Write-Host 'Git missing, installing via winget...'
    winget install --id Git.Git -e --silent --accept-package-agreements --accept-source-agreements
    Refresh-Path
}

# 2) Node.js
if (-not (Test-Cmd 'node')) {
    Write-Host 'Node.js missing, installing via winget...'
    winget install --id OpenJS.NodeJS.LTS -e --silent --accept-package-agreements --accept-source-agreements
    Refresh-Path
}
if (-not (Test-Cmd 'node')) {
    Write-Host 'WARN: node still not found after install attempt.'
}

# 3) pnpm
if (-not (Test-Cmd 'pnpm')) {
    if (Test-Cmd 'npm') {
        Write-Host 'Installing pnpm...'
        npm install -g pnpm@11.7.0
        Refresh-Path
    } else {
        Write-Host 'WARN: npm missing, cannot install pnpm.'
    }
}

# 4) harness 源码：缺失则拉取（shallow 克隆更快、更不易中断；失败时清理残留）
if (-not (Test-Path (Join-Path $harnessDir 'package.json'))) {
    if (Test-Cmd 'git') {
        Write-Host "Harness source missing, cloning (shallow) $harnessRepo ..."
        git clone --depth 1 --branch $harnessBranch $harnessRepo $harnessDir
        if ($LASTEXITCODE -ne 0) {
            Write-Host "ERROR: git clone failed (exit $LASTEXITCODE); check network or repo url."
            if (Test-Path $harnessDir) { Remove-Item $harnessDir -Recurse -Force -ErrorAction SilentlyContinue }
        }
    } else {
        Write-Host 'ERROR: git unavailable, cannot clone harness source.'
    }
}

# 5) 安装 harness 依赖
if (Test-Path (Join-Path $harnessDir 'package.json')) {
    if (-not (Test-Path (Join-Path $harnessDir 'node_modules'))) {
        Write-Host 'Installing harness dependencies (pnpm install)...'
        Push-Location $harnessDir
        pnpm install
        Pop-Location
    }
}

Write-Host 'dsh-systray prerequisite setup finished.'
