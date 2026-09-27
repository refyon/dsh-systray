"""截图物料验收：尺寸 / 裁切 / 与渲染结果一致 / 脱敏。

判定标准：
1. 尺寸：轮播图 840x560（渲染视口 = 设置窗口逻辑尺寸 winW×winH），主图 900x634；
2. 右侧留白：卡片右描边距图右缘 22~45px（CSS 是 32px）。
   本次事故的形态就是内容被裁到 3~5px，这一条专门盯它；
3. 与渲染结果一致：把已入库的图与「现场重渲染的 PNG」逐像素比对（--from-render 指定
   渲染产物目录），确保入库图确实来自渲染器、不是旧的半成品；
4. 脱敏：脚本与物料里不得出现本机真实用户名/主机名/真实路径，演示值必须在场。

用法:
  node scripts/render_shots.mjs --keep-png
  python scripts/verify_shots.py --from-render docs/.shots-tmp
"""
import argparse
import os
import sys

from PIL import Image, ImageChops

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOCS = os.path.join(ROOT, "docs")

ap = argparse.ArgumentParser()
ap.add_argument("--from-render", default=None,
                help="渲染器保留的 PNG 目录（--keep-png 产物），用于逐像素比对")
a = ap.parse_args()

REAL_TOKENS = ["lenovo", "Lenovo", "LENOVO", "agent-env", "D:\\Desktop"]
DEMO_TOKENS = ["demo@example.com", "C:\\\\Users\\\\demo"]

problems = []
lines = []


def note(ok, msg):
    lines.append(("  OK  " if ok else " FAIL ") + msg)
    if not ok:
        problems.append(msg)


def right_gap(img):
    w, h = img.size
    px = img.convert("RGB").load()
    cands = []
    for y in range(int(h * 0.25), int(h * 0.8), 4):
        for x in range(w - 4, int(w * 0.6), -1):
            p, q = px[x, y], px[x - 1, y]
            if abs(p[0] - q[0]) + abs(p[1] - q[1]) + abs(p[2] - q[2]) > 22:
                cands.append(x)
                break
    if not cands:
        return None
    cands.sort()
    return w - 1 - cands[len(cands) // 2]


def diff_ratio(im_a, im_b):
    """两图差异像素占比（0~1）。"""
    if im_a.size != im_b.size:
        return 1.0
    d = ImageChops.difference(im_a.convert("RGB"), im_b.convert("RGB"))
    bbox = d.getbbox()
    if bbox is None:
        return 0.0
    hist = d.convert("L").histogram()
    total = im_a.size[0] * im_a.size[1]
    changed = sum(hist[12:])  # 容差：WebP 有损压缩的轻微 ringing 不算差异
    return changed / total


# ---------- 1. 轮播图 ----------
for folder, lang in (("shots", "zh"), ("shots-en", "en")):
    d = os.path.join(DOCS, folder)
    files = sorted(os.listdir(d))
    note(len(files) == 8, f"{folder}: 应有 8 个文件，实际 {len(files)}")
    for f in files:
        p = os.path.join(d, f)
        im = Image.open(p)
        note(im.size == (840, 560), f"{folder}/{f}: 尺寸 {im.size}（应 840x560）")
        if f == "github-auth.png":
            note(im.format == "PNG", f"{folder}/{f}: 格式 {im.format}（网站按 .png 引用）")
        else:
            gap = right_gap(im)
            note(gap is not None and 22 <= gap <= 45,
                 f"{folder}/{f}: 卡片右缘留白 {gap}px（应 22~45；过小=内容被裁）")

# ---------- 2. 主图 ----------
for name in ("screenshot-hero.webp", "screenshot-hero-en.webp"):
    im = Image.open(os.path.join(DOCS, name))
    note(im.size == (900, 634), f"{name}: 尺寸 {im.size}（应 900x634）")

# ---------- 3. 与现场渲染结果比对 + 画面文字核对 ----------
if a.from_render:
    tmp = os.path.join(ROOT, a.from_render)
    checked = 0
    for folder, lang in (("shots", "zh"), ("shots-en", "en")):
        for f in sorted(os.listdir(os.path.join(DOCS, folder))):
            stem = f.rsplit(".", 1)[0]
            fresh = os.path.join(tmp, f"{lang}-{stem}.png")
            if not os.path.isfile(fresh):
                note(False, f"{folder}/{f}: 缺少对照渲染 {os.path.relpath(fresh, ROOT)}")
                continue
            shipped = Image.open(os.path.join(DOCS, folder, f))
            r = diff_ratio(shipped, Image.open(fresh))
            note(r < 0.03, f"{folder}/{f}: 与渲染结果差异 {r * 100:.2f}%（应 <3%）")
            checked += 1
    note(checked == 16, f"比对了 {checked}/16 张（应 16 张）")

    # 画面文字：每张图都必须含有该页的标题（证明不是空白/裁空的图），
    # 且不得出现本机真实信息。
    EXPECT_TITLE = {
        "general": ("常规", "General"),
        "about-top": ("关于", "About"),
        "about-bottom": ("关于", "About"),
        "logs": ("日志", "Logs"),
        "export": ("导出", "Export"),
        "sync": ("数据同步", "Data sync"),
        "github-auth": ("关于", "About"),
    }
    for folder, lang in (("shots", "zh"), ("shots-en", "en")):
        for f in sorted(os.listdir(os.path.join(DOCS, folder))):
            stem = f.rsplit(".", 1)[0]
            tf = os.path.join(tmp, f"{lang}-{stem}.txt")
            if not os.path.isfile(tf):
                note(False, f"{folder}/{f}: 缺少文字导出 {os.path.relpath(tf, ROOT)}")
                continue
            text = open(tf, encoding="utf-8").read()
            if stem in EXPECT_TITLE:
                want = EXPECT_TITLE[stem][0 if lang == "zh" else 1]
                note(want in text, f"{folder}/{f}: 画面文字应含页标题「{want}」")
            # 先剔除允许出现的演示值（演示值本身含 example/demo 字样），再找本机真实信息
            probe = text
            probe = probe.replace("demo@example.com", "«demo»").replace("example/prompt-assistant", "«demo»")
            probe = probe.replace("example.com", "«demo»").replace("C:\\Users\\demo", "«demo»")
            for tok in REAL_TOKENS:
                if len(tok) < 4:
                    continue
                note(tok not in probe, f"{folder}/{f}: 画面文字不应出现本机信息 {tok!r}")
            # 侧栏 7 项齐全（帮助/Help 在最后一项，被裁掉时最先消失）
            nav_last = ("帮助", "Help")[0 if lang == "zh" else 1]
            note(nav_last in text, f"{folder}/{f}: 侧栏应含最后一项「{nav_last}」")
else:
    lines.append("  --   跳过“与渲染结果比对”（未提供 --from-render）")

# ---------- 4. 脱敏 ----------
for p in (os.path.join(ROOT, "scripts", "render_shots.mjs"),):
    src = open(p, encoding="utf-8").read()
    for tok in REAL_TOKENS:
        note(tok not in src, f"{os.path.basename(p)}: 不应出现本机信息 {tok!r}")
    for tok in DEMO_TOKENS:
        note(tok in src, f"{os.path.basename(p)}: 应使用演示值 {tok!r}")

print("\n".join(lines))
print()
if problems:
    print(f"[FAIL] {len(problems)} 项不合格：")
    for p in problems:
        print("   -", p)
    sys.exit(1)
print(f"[OK] 全部通过（{len(lines)} 项）")
