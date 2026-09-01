#!/usr/bin/env python3
"""把 docs 截图（全尺寸 PNG）转成网站用的 WebP（简单直接）：
1. LANCZOS 高质量缩放到目标宽度
2. WebP quality 92（高保真）
3. 删除 PNG 源以减小仓库体积
用法: python scripts/convert_webp.py
"""
import os

from PIL import Image

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
docs = os.path.join(root, "docs")

TARGET_W = 900   # 不缩小（窗口截图 808px 宽，原尺寸输出保证文字清晰）
WEBP_Q = 95      # WebP 质量（高保真）


def _convert(png_path):
    webp_path = png_path[:-4] + ".webp"
    with Image.open(png_path) as im:
        w, h = im.size
        if w > TARGET_W:
            nh = max(1, round(h * TARGET_W / w))
            im = im.resize((TARGET_W, nh), Image.LANCZOS)
        im.save(webp_path, "WEBP", quality=WEBP_Q, method=6)
    src_size = os.path.getsize(png_path)
    os.remove(png_path)
    dst_size = os.path.getsize(webp_path)
    print(f"{os.path.basename(webp_path)}: {src_size} -> {dst_size} bytes ({dst_size * 100 // max(src_size, 1)}%)")


for t in [os.path.join(docs, "shots"), os.path.join(docs, "screenshot.png")]:
    if os.path.isdir(t):
        for f in sorted(os.listdir(t)):
            if f.endswith(".png"):
                _convert(os.path.join(t, f))
    elif os.path.isfile(t) and t.endswith(".png"):
        _convert(t)
