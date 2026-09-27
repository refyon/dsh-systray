#!/usr/bin/env python3
"""把渲染出的截图 PNG 转成网站/README 用的 WebP：
1. LANCZOS 高质量缩放到目标宽度（超过时）
2. WebP quality 95（高保真）
3. 删除 PNG 源以减小仓库体积

渲染器（scripts/render_shots.mjs）按窗口逻辑尺寸 840×560 出图，宽度已低于目标宽度，
因此默认不缩放——截图与界面像素一一对应，文字最清晰。

保留为 PNG 的：docs/shots[-en]/github-auth.png（网站轮播按 .png 引用，含弹窗阴影，
WebP 有损压缩会在卡片边缘留下色带）。

用法: python scripts/convert_webp.py
"""
import argparse
import os

from PIL import Image

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
docs = os.path.join(root, "docs")

TARGET_W = 900    # 超过才缩小；渲染宽度 840 < 900，即原尺寸输出
WEBP_Q = 95       # WebP 质量（高保真）
KEEP_PNG = {"github-auth.png"}


def _convert(png_path):
    webp_path = png_path[:-4] + ".webp"
    with Image.open(png_path) as im:
        im = im.convert("RGB")
        w, h = im.size
        if w > TARGET_W:
            nh = max(1, round(h * TARGET_W / w))
            im = im.resize((TARGET_W, nh), Image.LANCZOS)
        im.save(webp_path, "WEBP", quality=WEBP_Q, method=6)
    src_size = os.path.getsize(png_path)
    os.remove(png_path)
    dst_size = os.path.getsize(webp_path)
    print(f"{os.path.basename(webp_path)}: {src_size} -> {dst_size} bytes ({dst_size * 100 // max(src_size, 1)}%)")


ap = argparse.ArgumentParser(description="把 docs 下的截图 PNG 转成 WebP")
ap.add_argument("--only", choices=["shots", "hero"], default=None,
                help="只处理某一类：shots=轮播图，hero=主图（默认两类都处理）")
a = ap.parse_args()

targets = []
if a.only in (None, "shots"):
    targets.append(os.path.join(docs, "shots"))
    if os.path.isdir(os.path.join(docs, "shots-en")):
        targets.append(os.path.join(docs, "shots-en"))  # 英文版截图（存在时一并转换）
if a.only in (None, "hero"):
    targets.append(os.path.join(docs, "screenshot-hero.png"))
    targets.append(os.path.join(docs, "screenshot-hero-en.png"))

for t in targets:
    if os.path.isdir(t):
        for f in sorted(os.listdir(t)):
            if f.endswith(".png") and f not in KEEP_PNG:
                _convert(os.path.join(t, f))
    elif os.path.isfile(t) and t.endswith(".png"):
        _convert(t)
