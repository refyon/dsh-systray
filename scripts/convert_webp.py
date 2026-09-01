#!/usr/bin/env python3
"""把 docs 截图转成 WebP（网站/README 用），删除原 PNG 源以减小仓库体积。
用法: python scripts/convert_webp.py
"""
import os

from PIL import Image

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
docs = os.path.join(root, "docs")


def _convert(png_path):
    webp_path = png_path[:-4] + ".webp"
    with Image.open(png_path) as im:
        im.save(webp_path, "WEBP", quality=82, method=6)
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
