#!/usr/bin/env python3
"""合成「设置窗口 + GitHub 授权弹窗」轮播图（docs/shots[-en]/github-auth.png）：
- 弹窗按圆角卡片贴到设置窗口客户区上（真实弹窗已无边框，这里保证圆角与描边完整）
- 卡片四周用 alpha 遮罩 + 高斯模糊生成柔和阴影（与 README 主图 scripts/make_hero.py 同款做法）

为什么要这一步：弹窗是独立的 Win32 窗口，PrintWindow 渲染不出 DWM 合成的窗口阴影，
直接贴图会得到"没有投影的卡片"；且桌面锁定（无交互会话）时无法用整屏抓取拿到真实阴影。
统一在合成阶段画阴影后，出图与桌面状态无关、中英两张一致。

用法:
  python scripts/compose_dialog_shot.py app.png dialog.png out.png \
      --x 186 --y 157 --w 436 --h 190 [--crop 8] [--radius 10] [--blur 12] [--alpha 30] [--ix 0] [--iy 0]
"""
import argparse

from PIL import Image, ImageDraw, ImageFilter


def rounded_mask(size, radius, ss=4):
    """4 倍超采样后缩放，得到抗锯齿的圆角遮罩。"""
    w, h = size
    m = Image.new("L", (w * ss, h * ss), 0)
    ImageDraw.Draw(m).rounded_rectangle([0, 0, w * ss - 1, h * ss - 1], radius=radius * ss, fill=255)
    return m.resize(size, Image.LANCZOS)


def window_shadow(card, blur, alpha):
    """由卡片自身 alpha 生成四周均匀的柔和阴影（同 make_hero.py）：返回阴影图与留白 padding。"""
    pad = blur * 2
    w, h = card.width + pad * 2, card.height + pad * 2
    sh = Image.new("RGBA", (w, h), (15, 23, 42, 0))
    sh_a = Image.new("L", (w, h), 0)
    sh_a.paste(card.split()[3], (pad, pad))
    sh.putalpha(sh_a.point(lambda v: int(v * alpha / 255)))
    return sh.filter(ImageFilter.GaussianBlur(blur)), pad


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("app", help="设置窗口客户区 PNG（未裁剪）")
    ap.add_argument("dialog", help="弹窗 PNG（整窗）")
    ap.add_argument("out", help="输出 PNG")
    ap.add_argument("--x", type=int, required=True, help="弹窗内容左上角在 app 图中的 x")
    ap.add_argument("--y", type=int, required=True, help="弹窗内容左上角在 app 图中的 y")
    ap.add_argument("--w", type=int, required=True, help="弹窗内容宽")
    ap.add_argument("--h", type=int, required=True, help="弹窗内容高")
    ap.add_argument("--crop", type=int, default=8, help="设置窗口四周裁掉的圆角/边框像素（与其它轮播图一致）")
    ap.add_argument("--radius", type=int, default=10, help="卡片圆角半径")
    ap.add_argument("--blur", type=int, default=12, help="阴影模糊半径")
    ap.add_argument("--alpha", type=int, default=30, help="阴影不透明度（0-255）")
    ap.add_argument("--ix", type=int, default=0, help="弹窗内容在 dialog 图中的左内缩（PrintWindow 黑边）")
    ap.add_argument("--iy", type=int, default=0, help="弹窗内容在 dialog 图中的上内缩（PrintWindow 黑边）")
    a = ap.parse_args()

    base = Image.open(a.app).convert("RGBA")
    c = a.crop
    base = base.crop((c, c, base.width - c, base.height - c))

    dlg = Image.open(a.dialog).convert("RGBA")
    if dlg.width >= a.ix + a.w and dlg.height >= a.iy + a.h:
        dlg = dlg.crop((a.ix, a.iy, a.ix + a.w, a.iy + a.h))
    else:
        dlg = dlg.crop((0, 0, min(a.w, dlg.width), min(a.h, dlg.height)))

    card = dlg.copy()
    card.putalpha(rounded_mask(card.size, a.radius))

    sh, pad = window_shadow(card, a.blur, a.alpha)
    x, y = a.x - c, a.y - c
    canvas = base.copy()
    canvas.alpha_composite(sh, (x - pad, y - pad))
    canvas.alpha_composite(card, (x, y))
    canvas.convert("RGB").save(a.out, "PNG")
    print(f"saved {a.out} ({canvas.width}x{canvas.height}) card={card.width}x{card.height} at ({x},{y})")


if __name__ == "__main__":
    main()
