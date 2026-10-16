"""发布物料验收：渲染素材排版 / 脱敏 / 站点预览同步。

判定标准：
1. 渲染素材（--from-render 指定 docs/.shots-parts）：尺寸 840x560（= 设置窗口逻辑尺寸），
   卡片右描边距图右缘 22~45px（CSS 是 32px）——本次事故的形态就是内容被裁到 3~5px，
   这一条专门盯它；画面文字必须含页标题与侧栏最后一项（证明不是空白/裁空的图），
   且不得出现本机真实用户名/主机名/真实路径，演示值必须在场；
2. 脱敏：渲染与演示数据脚本里不得出现本机真实信息，演示值必须来自 shot-shim.mjs；
3. 站点预览：docs/mock/ 必须与 src/frontend/dist 一致（官网轮播是 iframe 实时预览，
   前端改动后不重跑 build_mock.mjs 就会漂移）；
4. README 演示动图：docs/demo/ 必须是 APNG（24 位色 + alpha）、2 倍图、无限循环、
   体积 ≤ 5 MB，且 README.md / README.en.md 各自引用了浅色与深色两套
   （GitHub 的 #gh-light-mode-only / #gh-dark-mode-only）。

历史：这里原来还验收网站轮播用的 8 张位图截图（docs/shots、docs/shots-en）与两个 README
主图（docs/screenshot-hero[-en].webp）。官网已改为 iframe 实时预览，位图截图 2026-10-06 起
不再产出；README 首屏 2026-10-12 改用设置界面演示动图，2026-10-16 从 GIF 换成 APNG
（docs/demo/settings-*.apng，深浅双主题），主图及其合成/转换脚本（make_hero.py、
convert_webp.py）一并移除。

用法:
  node scripts/render_shots.mjs
  python scripts/verify_shots.py --from-render docs/.shots-parts
"""
import argparse
import os
import re
import struct
import sys

from PIL import Image

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOCS = os.path.join(ROOT, "docs")

ap = argparse.ArgumentParser()
ap.add_argument("--from-render", default=None,
                help="渲染器产出的素材目录（默认 docs/.shots-parts），用于核对排版与画面文字")
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


# ---------- 1. 渲染素材（渲染产物） ----------
if a.from_render:
    parts = os.path.join(ROOT, a.from_render)
    EXPECT_TITLE = {"settings-general": ("常规", "General")}
    for stem, lang in (("settings-general", "zh"), ("settings-general-en", "en")):
        png = os.path.join(parts, f"{stem}.png")
        if not os.path.isfile(png):
            note(False, f"{stem}.png: 缺少渲染素材 {os.path.relpath(png, ROOT)}")
            continue
        im = Image.open(png)
        note(im.size == (840, 560), f"{stem}.png: 尺寸 {im.size}（应 840x560）")
        gap = right_gap(im)
        note(gap is not None and 22 <= gap <= 45,
             f"{stem}.png: 卡片右缘留白 {gap}px（应 22~45；过小=内容被裁）")

        tf = os.path.join(parts, f"{stem}.txt")
        if not os.path.isfile(tf):
            note(False, f"{stem}.txt: 缺少文字导出（渲染器应同时导出画面文字）")
            continue
        text = open(tf, encoding="utf-8").read()
        want = EXPECT_TITLE["settings-general"][0 if lang == "zh" else 1]
        note(want in text, f"{stem}.txt: 画面文字应含页标题「{want}」")
        nav_last = ("帮助", "Help")[0 if lang == "zh" else 1]
        note(nav_last in text, f"{stem}.txt: 侧栏应含最后一项「{nav_last}」")
        # 先剔除允许出现的演示值（演示值本身含 example/demo 字样），再找本机真实信息
        probe = text
        probe = probe.replace("demo@example.com", "«demo»").replace("example/prompt-assistant", "«demo»")
        probe = probe.replace("example.com", "«demo»").replace("C:\\Users\\demo", "«demo»")
        for tok in REAL_TOKENS:
            if len(tok) < 4:
                continue
            note(tok not in probe, f"{stem}.txt: 画面文字不应出现本机信息 {tok!r}")
else:
    lines.append("  --   跳过素材核对（未提供 --from-render）")

# ---------- 2. 脱敏 ----------
# 演示值集中在 shot-shim.mjs（渲染器与站点实时预览共用），其余脚本只应引用它们。
shim_path = os.path.join(ROOT, "scripts", "shot-shim.mjs")
for p in (os.path.join(ROOT, "scripts", "render_shots.mjs"),
          shim_path,
          os.path.join(ROOT, "scripts", "build_mock.mjs")):
    src = open(p, encoding="utf-8").read()
    for tok in REAL_TOKENS:
        note(tok not in src, f"{os.path.basename(p)}: 不应出现本机信息 {tok!r}")
shim_src = open(shim_path, encoding="utf-8").read()
for tok in DEMO_TOKENS:
    note(tok in shim_src, f"shot-shim.mjs: 应使用演示值 {tok!r}")

# ---------- 3. 站点实时界面预览（docs/mock/）与前端保持同步 ----------
# 官网轮播直接载入 App 真实前端（iframe），前端改动后必须重跑 scripts/build_mock.mjs，
# 否则站点预览会停留在旧界面（这里挡住这种漂移）。
for name in ("main.js", "style.css"):
    a_ = open(os.path.join(ROOT, "src", "frontend", "dist", name), encoding="utf-8").read()
    b_ = open(os.path.join(DOCS, "mock", name), encoding="utf-8").read()
    note(a_ == b_, f"docs/mock/{name}: 应与 src/frontend/dist/{name} 一致（改前端后跑 node scripts/build_mock.mjs）")
mock_idx = open(os.path.join(DOCS, "mock", "index.html"), encoding="utf-8").read()
note('<script src="shim.js"></script>' in mock_idx, "docs/mock/index.html: 应注入 shim.js（Wails 运行时桩）")
mock_shim = open(os.path.join(DOCS, "mock", "shim.js"), encoding="utf-8").read()
note("window.go" in mock_shim and "window.runtime" in mock_shim,
     "docs/mock/shim.js: 应提供 window.go / window.runtime")

# ---------- 4. 已弃用的位图截图目录不应复活 ----------
for folder in ("shots", "shots-en"):
    note(not os.path.isdir(os.path.join(DOCS, folder)),
         f"docs/{folder}/: 站点已改用实时预览，位图截图目录不应存在")

# ---------- 5. README 首屏演示动图（APNG，深浅双主题） ----------
# 为什么盯这一组：GIF（8-bit 256 色）把 840×560 的真实界面压出色彩断层与毛边，2026-10-16
# 换成 APNG（PNG 容器，24 位色 + 完整 alpha），并按 GitHub 的 #gh-light-mode-only /
# #gh-dark-mode-only 分派深浅两套。这里挡住「退回 GIF」「体积失控」「只留一套主题」
# 「README 引用了不存在的文件」四类回归。
DEMO_DIR = os.path.join(DOCS, "demo")
DEMO_VARIANTS = (("zh", "settings-zh.apng", "settings-zh-dark.apng"),
                 ("en", "settings-en.apng", "settings-en-dark.apng"))
# 背景板颜色（record_demo.mjs 的 --mat-light / --mat-dark）：窗口四周的底，也是圆角四角露出的颜色
MAT_RGB = {"light": (220, 227, 236), "dark": (30, 41, 59)}   # #DCE3EC / #1E293B


def _near(rgb, want, tol=10):
    return all(abs(a - b) <= tol for a, b in zip(rgb, want))


def apng_info(path):
    """返回 (宽, 高, 帧数, 循环次数, 非整画布帧数)；不是 APNG 时返回 None。

    「非整画布帧」统计 fcTL 里尺寸/偏移不是整画布、或 dispose/blend 不是
    NONE/SOURCE 的帧：ffmpeg 直出 APNG 时会做帧差分，产出局部帧 + dispose=BACKGROUND
    + blend=OVER（甚至 1x1@0,0 的怪帧），最终画面完全依赖浏览器的 APNG 合成实现，
    不同解码器表现不一致（2026-10-16 用户反馈「标题空心」即出在这类帧上）。
    本仓库改为全画布覆盖式写入，这里把它固化成不变量。
    """
    with open(path, "rb") as fh:
        data = fh.read()
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        return None
    pos, size, frames, plays, animated, odd = 8, (0, 0), 0, 0, False, 0
    while pos + 8 <= len(data):
        (length,) = struct.unpack(">I", data[pos:pos + 4])
        ctype = data[pos + 4:pos + 8]
        body = data[pos + 8:pos + 8 + length]
        if ctype == b"IHDR":
            size = struct.unpack(">II", body[:8])
        elif ctype == b"acTL":
            frames, plays = struct.unpack(">II", body[:8])
            animated = True
        elif ctype == b"fcTL":
            fw, fh, fx, fy = struct.unpack(">IIII", body[4:20])
            if (fw, fh, fx, fy) != (size[0], size[1], 0, 0) or body[24] != 0 or body[25] != 0:
                odd += 1
        pos += 12 + length
        if ctype == b"IEND":
            break
    return (size[0], size[1], frames, plays, odd) if animated else None


leftover = sorted(f for f in os.listdir(DEMO_DIR) if f.lower().endswith(".gif")) if os.path.isdir(DEMO_DIR) else []
note(not leftover, f"docs/demo/: 动图已改用 APNG，不应残留 {leftover}")

for lang, light_name, dark_name in DEMO_VARIANTS:
    for name in (light_name, dark_name):
        path = os.path.join(DEMO_DIR, name)
        if not os.path.isfile(path):
            note(False, f"docs/demo/{name}: 缺少 README 演示动图（node scripts/record_demo.mjs && node scripts/encode_demo.mjs）")
            continue
        bytes_ = os.path.getsize(path)
        note(bytes_ <= 5 * 1024 * 1024, f"docs/demo/{name}: 体积 {bytes_ / 1048576:.2f} MB（应 ≤ 5 MB，否则加载变慢）")
        info = apng_info(path)
        note(info is not None, f"docs/demo/{name}: 应是 APNG（PNG 容器含 acTL 块）")
        if info:
            w, h, frames, plays, odd = info
            note((w, h) == (1320, 900),
                 f"docs/demo/{name}: 尺寸 {w}x{h}（应 1320x900 = 窗口 840x560 + 四周 20px 背景板，再 ×1.5）")
            note(frames >= 10, f"docs/demo/{name}: 帧数 {frames}（应 ≥ 10，过少说明退化成了幻灯片）")
            note(plays == 0, f"docs/demo/{name}: 循环次数 {plays}（应 0 = 无限循环）")
            note(odd == 0, f"docs/demo/{name}: {odd} 个非整画布帧（应全部为整画布 + dispose=NONE + blend=SOURCE，"
                           f"局部帧会让渲染结果取决于浏览器的 APNG 合成实现）")
            # 背景板 + 圆角：窗口四角应露出背景板颜色，边中点则仍是界面（否则说明圆角/背景板丢了）
            mat = MAT_RGB["dark" if "-dark" in name else "light"]
            frame = Image.open(path)
            frame.seek(0)
            rgb = frame.convert("RGB")
            s = w / 880.0
            corners = [rgb.getpixel((int(x * s), int(y * s))) for x, y in ((22, 22), (858, 22), (22, 578), (858, 578))]
            note(all(_near(c, mat) for c in corners),
                 f"docs/demo/{name}: 窗口四角应露出背景板颜色 {mat}（圆角 14px），实测 {corners}")
            mid = rgb.getpixel((int(440 * s), int(22 * s)))
            note(not _near(mid, mat),
                 f"docs/demo/{name}: 窗口上边中点应是界面底色而非背景板（实测 {mid}）")
    readme = "README.md" if lang == "zh" else "README.en.md"
    src = open(os.path.join(ROOT, readme), encoding="utf-8").read()
    for name, suffix in ((light_name, "#gh-light-mode-only"), (dark_name, "#gh-dark-mode-only")):
        tag = re.search(r"<img[^>]*" + re.escape(name) + re.escape(suffix) + r"[^>]*>", src)
        note(tag is not None, f"{readme}: 应引用 docs/demo/{name}{suffix}")
        # 显示宽度必须与 1 倍抓图宽度一致（840 + 2×20），否则浏览器会二次缩放、笔画发虚
        if tag:
            note('width="880"' in tag.group(0),
                 f"{readme}: docs/demo/{name} 的 <img> 应带 width=\"880\"（= 1 倍抓图宽度，避免浏览器缩放）")

print("\n".join(lines))
print()
if problems:
    print(f"[FAIL] {len(problems)} 项不合格：")
    for p in problems:
        print("   -", p)
    sys.exit(1)
print(f"[OK] 全部通过（{len(lines)} 项）")
