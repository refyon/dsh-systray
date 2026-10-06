#!/usr/bin/env python3
"""合成 README 主图（docs/screenshot-hero.png，再由 convert_webp.py 转 webp）：
- 背景：设置页「常规」真实渲染截图（docs/.shots-parts/hero-bg[-en].png）
- 前景：真实渲染的启动进度卡片（docs/.shots-parts/splash-card[-en].png）
- 两个窗口四边带柔和阴影

背景与前景卡片都来自渲染器而不是这里手绘：文案、字号、进度条尺寸都跟 App 里一模一样，
以后界面改了只重跑渲染即可，不必在这里同步改绘制代码。

用法:
  node scripts/render_shots.mjs          # 先出 .shots-parts/hero-bg*.png 与 splash-card*.png
  python scripts/make_hero.py --lang zh  # 再合成中/英主图
  python scripts/convert_webp.py         # 最后 PNG 转 webp（并删掉中间 PNG）
"""
import argparse
import os
import sys

from PIL import Image, ImageDraw, ImageFilter

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
docs = os.path.join(root, "docs")

W = 780          # 底图窗口宽度
RADIUS = 14      # 与 style.css 的 --radius 一致（卡片圆角）
CARD_RADIUS = 14 # 启动卡圆角同上

ap = argparse.ArgumentParser(description="合成 README 主图")
ap.add_argument("--lang", choices=["zh", "en"], default="zh", help="主图语言（决定进度卡文案）")
a = ap.parse_args()

parts = os.path.join(docs, ".shots-parts")
suffix = "-en" if a.lang == "en" else ""
base_png = os.path.join(parts, f"hero-bg{suffix}.png")
splash = os.path.join(parts, f"splash-card{suffix}.png")
out = os.path.join(docs, f"screenshot-hero{suffix}.png")


def window_shadow(img, blur, alpha):
    """由窗口自身 alpha 遮罩生成阴影，四周加 padding 后高斯模糊——四边均匀、圆角对齐。"""
    a = img.split()[3]
    pad = blur * 2
    w = img.width + pad * 2
    h = img.height + pad * 2
    sh = Image.new("RGBA", (w, h), (15, 23, 42, 0))
    sh_a = Image.new("L", (w, h), 0)
    sh_a.paste(a, (pad, pad))
    sh.putalpha(sh_a.point(lambda v: int(v * alpha / 255)))
    sh = sh.filter(ImageFilter.GaussianBlur(blur))
    return sh, pad


def rounded(img, radius):
    """给图片四角做圆角（alpha 遮罩）。"""
    mask = Image.new("L", img.size, 0)
    d = ImageDraw.Draw(mask)
    d.rounded_rectangle([0, 0, img.width - 1, img.height - 1], radius=radius, fill=255)
    out = img.copy()
    out.putalpha(mask)
    return out


def load_splash_card():
    """前景卡片：渲染器出的是不透明 PNG（浏览器截图不留 alpha），这里补回圆角。"""
    if not os.path.isfile(splash):
        sys.exit(f"缺少 {splash}——请先运行 node scripts/render_shots.mjs")
    card = Image.open(splash).convert("RGBA")
    return rounded(card, CARD_RADIUS)


def main():
    if not os.path.isfile(base_png):
        sys.exit(f"缺少 {base_png}——请先运行 node scripts/render_shots.mjs")

    # ---------- 底图：常规页真实截图 ----------
    base = Image.open(base_png).convert("RGBA")
    H = round(base.height * W / base.width)
    base = base.resize((W, H), Image.LANCZOS)
    base = rounded(base, RADIUS)

    # ---------- 前景：真实启动进度卡片 ----------
    fg = load_splash_card()

    # ---------- 画布 ----------
    pad = 70
    canvas_w = W + pad * 2 + 60              # 右侧为前景窗口留空间
    canvas_h = H + pad * 2 + 30
    canvas = Image.new("RGBA", (canvas_w, canvas_h), (238, 242, 248, 255))

    # 背景窗口（四边浅阴影）
    bg_shadow, spad = window_shadow(base, 12, 20)
    canvas.alpha_composite(bg_shadow, (pad - spad, pad - spad))
    canvas.alpha_composite(base, (pad, pad))

    # 前景卡片：压在右下角，与底图窗口有重叠，暗示「启动进度窗口浮在设置页之上」
    fx = canvas_w - fg.width - pad + 30
    fy = pad + H - fg.height + 48
    fg_shadow, fspad = window_shadow(fg, 10, 26)
    canvas.alpha_composite(fg_shadow, (fx - fspad, fy - fspad))
    canvas.alpha_composite(fg, (fx, fy))

    # ---------- 输出 ----------
    canvas.convert("RGB").save(out, "PNG")
    print(f"saved {out} ({canvas.width}x{canvas.height}) fg={fg.width}x{fg.height} at ({fx},{fy})")


if __name__ == "__main__":
    main()
