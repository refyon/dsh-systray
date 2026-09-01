#!/usr/bin/env python3
"""合成 README 主图（docs/screenshot-hero.png）：
- 背景：设置页「常规」真实截图（docs/shots/general.webp）
- 前景：正在更新 DeepSeek Harness 依赖的下载中窗口（合成绘制）
- 两个窗口四边带柔和阴影
用法: python scripts/make_hero.py
"""
import os

from PIL import Image, ImageDraw, ImageFilter, ImageFont

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
docs = os.path.join(root, "docs")
general = os.path.join(docs, "shots", "general.webp")
out = os.path.join(docs, "screenshot-hero.png")

FONT = r"C:\Windows\Fonts\msyh.ttc"
FONT_BOLD = r"C:\Windows\Fonts\msyhbd.ttc"


def font(path, size):
    return ImageFont.truetype(path, size)


def window_shadow(img, blur, alpha):
    """由窗口自身的 alpha 遮罩生成阴影——形状（含圆角）与窗口完全一致。"""
    a = img.split()[3]
    sh = Image.new("RGBA", img.size, (15, 23, 42, 0))
    sh.putalpha(a.point(lambda v: int(v * alpha / 255)))
    sh = sh.filter(ImageFilter.GaussianBlur(blur))
    return sh


def rounded(img, radius):
    """给图片四角做圆角（alpha 遮罩）。"""
    mask = Image.new("L", img.size, 0)
    d = ImageDraw.Draw(mask)
    d.rounded_rectangle([0, 0, img.width - 1, img.height - 1], radius=radius, fill=255)
    out = img.copy()
    out.putalpha(mask)
    return out


def main():
    # ---------- 底图：常规页真实截图 ----------
    base = Image.open(general).convert("RGBA")
    # 缩放到目标宽度
    W = 780
    H = round(base.height * W / base.width)
    base = base.resize((W, H), Image.LANCZOS)
    base = rounded(base, 18)  # 常规页窗口圆角

    # ---------- 画布 ----------
    pad = 70
    canvas_w = W + pad * 2 + 60  # 右侧为前景窗口留空间
    canvas_h = H + pad * 2 + 30
    canvas = Image.new("RGBA", (canvas_w, canvas_h), (238, 242, 248, 255))

    # ---------- 背景窗口：常规页（浅阴影，圆角对齐） ----------
    bg_shadow = window_shadow(base, 10, 26)
    canvas.alpha_composite(bg_shadow, (pad - 8, pad - 5))
    canvas.alpha_composite(base, (pad, pad))

    # ---------- 前景窗口：正在更新 DeepSeek Harness 依赖（浅阴影，圆角对齐） ----------
    fw, fh = 540, 200
    fx, fy = canvas_w - fw - pad + 10, pad + H - fh + 60
    fg = Image.new("RGBA", (fw, fh), (0, 0, 0, 0))
    fd = ImageDraw.Draw(fg)
    fd.rounded_rectangle([0, 0, fw - 1, fh - 1], radius=16, fill=(255, 255, 255, 255))
    fd.rounded_rectangle([0, 0, fw - 1, fh - 1], radius=16, outline=(226, 232, 240, 255), width=1)
    fg_shadow = window_shadow(fg, 9, 34)
    canvas.alpha_composite(fg_shadow, (fx - 7, fy - 4))

    # 标题栏文字
    fd.text((24, 18), "DeepSeek Harness", font=font(FONT_BOLD, 15), fill=(15, 23, 42, 255))
    # 状态文字
    fd.text((24, 52), "正在更新 DeepSeek Harness 依赖（62%）…", font=font(FONT, 14), fill=(100, 116, 139, 255))
    # 进度条（轨道 + 品牌蓝填充）
    track_x, track_y, track_w, track_h = 24, 96, fw - 48, 10
    fd.rounded_rectangle([track_x, track_y, track_x + track_w, track_y + track_h], radius=5, fill=(241, 245, 249, 255))
    fill_w = int(track_w * 0.62)
    fd.rounded_rectangle([track_x, track_y, track_x + fill_w, track_y + track_h], radius=5, fill=(37, 99, 235, 255))
    # 底部小字
    fd.text((24, 128), "请勿关闭此窗口，更新完成后将自动重启", font=font(FONT, 12), fill=(148, 163, 184, 255))
    fd.text((fw - 24, 128), "取消更新", font=font(FONT, 12), fill=(37, 99, 235, 255), anchor="ra")

    canvas.alpha_composite(fg, (fx, fy))

    # ---------- 输出 ----------
    canvas.convert("RGB").save(out, "PNG")
    print(f"saved {out} ({canvas.width}x{canvas.height})")


if __name__ == "__main__":
    main()
