#!/bin/bash
# dsh-systray 运行依赖安装脚本（macOS）
set -e
HARNESS_DIR="${HOME}/deepseek-harness"
HARNESS_REPO="https://github.com/deepseek-ai/deepseek-harness.git"
HARNESS_BRANCH="master"

command -v brew >/dev/null 2>&1 || { echo "需要先安装 Homebrew：https://brew.sh"; exit 1; }

command -v node >/dev/null 2>&1 || brew install node
command -v pnpm >/dev/null 2>&1 || npm install -g pnpm@11.7.0
command -v git  >/dev/null 2>&1 || brew install git

if [ ! -f "${HARNESS_DIR}/package.json" ]; then
  echo "拉取 harness 源码..."
  git clone --branch "${HARNESS_BRANCH}" "${HARNESS_REPO}" "${HARNESS_DIR}"
fi

if [ -f "${HARNESS_DIR}/package.json" ] && [ ! -d "${HARNESS_DIR}/node_modules" ]; then
  echo "安装 harness 依赖（pnpm install）..."
  (cd "${HARNESS_DIR}" && pnpm install)
fi

echo "dsh-systray 依赖安装完成。"
