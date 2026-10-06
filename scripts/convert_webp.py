#!/usr/bin/env python3
"""把合成好的 README 主图 PNG（docs/screenshot-hero[-en].png）转成 WebP：
1. LANCZOS 高质量缩放（宽度超过 TARGET_W 时；主图 900 宽，不缩）
2. WebP quality 95（高保真）
3. 删除 PNG 源以减小仓库体积

主图由 scripts/make_hero.py 合成（背景 = 渲染的「常规」页，前景 = 真实启动卡）。
站点轮播已改为 iframe 实时预览（docs/mock），不再有位图截图物料。

用法: python scripts/convert_webp.py
"""
import os
import sys

from PIL import Image

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
docs = os.path.join(root, "docs")

TARGET_W = 900    # 超过才缩小
WEBP_Q = 95       # WebP 质量（高保真）
TARGETS = ["screenshot-hero.png", "screenshot-hero-en.png"]


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


done = 0
for name in TARGETS:
    p = os.path.join(docs, name)
    if not os.path.isfile(p):
        print(f"跳过 {name}：不存在（先跑 python scripts/make_hero.py --lang zh|en）", file=sys.stderr)
        continue
    _convert(p)
    done += 1

if done == 0:
    sys.exit("没有可转换的主图 PNG")
