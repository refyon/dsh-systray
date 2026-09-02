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

    # ---------- 背景窗口：常规页（四边浅阴影，圆角对齐） ----------
    bg_shadow, spad = window_shadow(base, 12, 20)
    canvas.alpha_composite(bg_shadow, (pad - spad, pad - spad))
    canvas.alpha_composite(base, (pad, pad))

    # ---------- 前景窗口：与真实 APP 更新窗口（splash 视图）一致的下载进度特写 ----------
    # 真实结构：白卡 + 标题「DeepSeek Harness」+ 状态「正在下载 <资产名>（N%）…」
    #          + 进度条 + 「取消更新」ghost 按钮；无额外底部小字。
    # v2：窗口调小（400×140）并收紧内部留白——此前 500×176 留白过多。
    fw, fh = 400, 140
    fx, fy = canvas_w - fw - pad + 10, pad + H - fh + 70
    fg = Image.new("RGBA", (fw, fh), (0, 0, 0, 0))
    fd = ImageDraw.Draw(fg)
    fd.rounded_rectangle([0, 0, fw - 1, fh - 1], radius=14, fill=(255, 255, 255, 255))
    fd.rounded_rectangle([0, 0, fw - 1, fh - 1], radius=14, outline=(226, 232, 240, 255), width=1)
    fg_shadow, fspad = window_shadow(fg, 10, 26)
    canvas.alpha_composite(fg_shadow, (fx - fspad, fy - fspad))

    # 标题（居中，同 splash-card：bold 15）
    fd.text((fw // 2, 22), "DeepSeek Harness", font=font(FONT_BOLD, 14), fill=(15, 23, 42, 255), anchor="mm")
    # 状态文本（同真实下载文案：正在下载 <资产名>（N%）…，居中、灰）
    fd.text((fw // 2, 46), "正在下载 dsh-systray-windows-x64.zip（62%）…",
            font=font(FONT, 12), fill=(100, 116, 139, 255), anchor="mm")
    # 进度条（轨道 + 品牌蓝填充，居中）
    track_x, track_y, track_w, track_h = (fw - 260) // 2, 68, 260, 7
    fd.rounded_rectangle([track_x, track_y, track_x + track_w, track_y + track_h], radius=4, fill=(241, 245, 249, 255))
    fill_w = int(track_w * 0.62)
    fd.rounded_rectangle([track_x, track_y, track_x + fill_w, track_y + track_h], radius=4, fill=(37, 99, 235, 255))
    # 「取消更新」ghost 按钮（btn-ghost：浅灰圆角胶囊、常规文字色，居中）
    btn_w, btn_h = 80, 24
    btn_x, btn_y = (fw - btn_w) // 2, 92
    fd.rounded_rectangle([btn_x, btn_y, btn_x + btn_w, btn_y + btn_h], radius=btn_h // 2, fill=(241, 245, 249, 255))
    fd.text((fw // 2, btn_y + btn_h // 2), "取消更新", font=font(FONT, 11), fill=(15, 23, 42, 255), anchor="mm")

    canvas.alpha_composite(fg, (fx, fy))

    # ---------- 输出 ----------
    canvas.convert("RGB").save(out, "PNG")
    print(f"saved {out} ({canvas.width}x{canvas.height})")


if __name__ == "__main__":
    main()
