"""发布物料验收：README 主图 / 主图素材 / 脱敏 / 站点预览同步。

判定标准：
1. 主图：docs/screenshot-hero[-en].webp 尺寸 900x634（合成画布固定尺寸）；
2. 主图背景素材（--from-render 指定 docs/.shots-parts）：尺寸 840x560（= 设置窗口逻辑尺寸），
   卡片右描边距图右缘 22~45px（CSS 是 32px）——本次事故的形态就是内容被裁到 3~5px，
   这一条专门盯它；画面文字必须含页标题与侧栏最后一项（证明不是空白/裁空的图），
   且不得出现本机真实用户名/主机名/真实路径，演示值必须在场；
3. 脱敏：渲染与演示数据脚本里不得出现本机真实信息，演示值必须来自 shot-shim.mjs；
4. 站点预览：docs/mock/ 必须与 src/frontend/dist 一致（官网轮播是 iframe 实时预览，
   前端改动后不重跑 build_mock.mjs 就会漂移）。

历史：这里原来还验收网站轮播用的 8 张位图截图（docs/shots、docs/shots-en）。官网已改为
iframe 实时预览，位图截图 2026-10-06 起不再产出也不再验收。

用法:
  node scripts/render_shots.mjs
  python scripts/verify_shots.py --from-render docs/.shots-parts
"""
import argparse
import os
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


# ---------- 1. 主图 ----------
for name in ("screenshot-hero.webp", "screenshot-hero-en.webp"):
    p = os.path.join(DOCS, name)
    if not os.path.isfile(p):
        note(False, f"{name}: 缺失")
        continue
    im = Image.open(p)
    note(im.size == (900, 634), f"{name}: 尺寸 {im.size}（应 900x634）")
    note(os.path.getsize(p) > 10_000, f"{name}: 体积 {os.path.getsize(p)} B（过小疑似空白图）")

# ---------- 2. 主图素材（渲染产物） ----------
if a.from_render:
    parts = os.path.join(ROOT, a.from_render)
    EXPECT_TITLE = {"hero-bg": ("常规", "General")}
    for stem, lang in (("hero-bg", "zh"), ("hero-bg-en", "en")):
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
        want = EXPECT_TITLE["hero-bg"][0 if lang == "zh" else 1]
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

# ---------- 3. 脱敏 ----------
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

# ---------- 4. 站点实时界面预览（docs/mock/）与前端保持同步 ----------
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

# ---------- 5. 已弃用的位图截图目录不应复活 ----------
for folder in ("shots", "shots-en"):
    note(not os.path.isdir(os.path.join(DOCS, folder)),
         f"docs/{folder}/: 站点已改用实时预览，位图截图目录不应存在")

print("\n".join(lines))
print()
if problems:
    print(f"[FAIL] {len(problems)} 项不合格：")
    for p in problems:
        print("   -", p)
    sys.exit(1)
print(f"[OK] 全部通过（{len(lines)} 项）")
