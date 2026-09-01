#!/usr/bin/env python3
"""把 docs 截图（全尺寸 PNG）转成网站用的 WebP（高保真）：
1. LANCZOS 高质量缩放到目标宽度（保留边缘与色彩细节）
2. 极轻 UnsharpMask（radius<=0.6 / percent<=60 / threshold>=4）：仅补偿缩放柔化，
   不产生可见光晕（halo）与彩色失真（此前 2.0/150 参数过激导致颜色失真）
3. WebP quality 92（近无损，截图总量小，下载仍快）
4. 删除 PNG 源以减小仓库体积
用法: python scripts/convert_webp.py
"""
import os
import shutil

from PIL import Image, ImageFilter

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
docs = os.path.join(root, "docs")

TARGET_W = 640            # 输出宽度（px）
SHARPEN = (0.6, 55, 4)    # UnsharpMask: radius, percent, threshold（极轻，避免光晕/失真）
WEBP_Q = 92               # WebP 质量（高保真）


def _convert(png_path):
    webp_path = png_path[:-4] + ".webp"
    with Image.open(png_path) as im:
        # 1) 高质量缩放
        w, h = im.size
        if w > TARGET_W:
            nh = max(1, round(h * TARGET_W / w))
            im = im.resize((TARGET_W, nh), Image.LANCZOS)
        # 2) 极轻锐化（仅补偿缩放柔化，阈值抑制平坦区噪声与色偏）
        im = im.filter(ImageFilter.UnsharpMask(radius=SHARPEN[0], percent=SHARPEN[1], threshold=SHARPEN[2]))
        # 3) 高保真 WebP
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

# README 主图 = 关于页截图
src = os.path.join(docs, "shots", "about.webp")
dst = os.path.join(docs, "screenshot.webp")
if os.path.isfile(src):
    shutil.copy2(src, dst)
    print(f"screenshot.webp <- about.webp ({os.path.getsize(dst)} bytes)")
