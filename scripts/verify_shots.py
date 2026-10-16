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


def apng_info(path):
    """返回 (宽, 高, 帧数, 循环次数)；不是 APNG 时返回 None。"""
    with open(path, "rb") as fh:
        data = fh.read()
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        return None
    pos, size, frames, plays, animated = 8, (0, 0), 0, 0, False
    while pos + 8 <= len(data):
        (length,) = struct.unpack(">I", data[pos:pos + 4])
        ctype = data[pos + 4:pos + 8]
        body = data[pos + 8:pos + 8 + length]
        if ctype == b"IHDR":
            size = struct.unpack(">II", body[:8])
        elif ctype == b"acTL":
            frames, plays = struct.unpack(">II", body[:8])
            animated = True
        pos += 12 + length
        if ctype == b"IEND":
            break
    return (size[0], size[1], frames, plays) if animated else None


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
            w, h, frames, plays = info
            note((w, h) == (1680, 1120), f"docs/demo/{name}: 尺寸 {w}x{h}（应 1680x1120 = 窗口 840x560 的 2 倍图）")
            note(frames >= 10, f"docs/demo/{name}: 帧数 {frames}（应 ≥ 10，过少说明退化成了幻灯片）")
            note(plays == 0, f"docs/demo/{name}: 循环次数 {plays}（应 0 = 无限循环）")
    readme = "README.md" if lang == "zh" else "README.en.md"
    src = open(os.path.join(ROOT, readme), encoding="utf-8").read()
    for name, suffix in ((light_name, "#gh-light-mode-only"), (dark_name, "#gh-dark-mode-only")):
        note(f"docs/demo/{name}{suffix}" in src, f"{readme}: 应引用 docs/demo/{name}{suffix}")

print("\n".join(lines))
print()
if problems:
    print(f"[FAIL] {len(problems)} 项不合格：")
    for p in problems:
        print("   -", p)
    sys.exit(1)
print(f"[OK] 全部通过（{len(lines)} 项）")
