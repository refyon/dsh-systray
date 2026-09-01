#!/usr/bin/env python3
"""通过 WebView2 CDP（Page.captureScreenshot）直出渲染截图——高保真，不经屏幕合成。
用法: python capture_cdp.py <输出.png> [端口]
前置: dsh-systray 以 DSH_SYSTRAY_SHOT_PAGE 启动（自动开启 --remote-debugging-port=9333）
"""
import base64
import json
import sys
import time
import urllib.request

import websocket  # websocket-client

port = int(sys.argv[2]) if len(sys.argv) > 2 else 9333
out = sys.argv[1]


def get_targets():
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/json", timeout=3) as r:
        return json.load(r)


# 等待 CDP 端口就绪
targets = None
for _ in range(30):
    try:
        targets = get_targets()
        if targets:
            break
    except Exception:
        time.sleep(0.5)

if not targets:
    sys.exit("CDP not ready")

page = next((t for t in targets if t.get("type") == "page"), targets[0])
ws = websocket.create_connection(page["webSocketDebuggerUrl"], timeout=15)

def call(method, params=None, mid=1):
    ws.send(json.dumps({"id": mid, "method": method, "params": params or {}}))
    while True:
        msg = json.loads(ws.recv())
        if msg.get("id") == mid:
            return msg.get("result")

call("Page.enable", mid=1)
# 等待页面完成初始渲染（服务状态/页面切换完成后截图）
time.sleep(1.0)
res = call("Page.captureScreenshot", {"format": "png", "fromSurface": True}, mid=2)
if not res or "data" not in res:
    ws.close()
    sys.exit("capture failed")

with open(out, "wb") as f:
    f.write(base64.b64decode(res["data"]))
ws.close()
print(f"saved {out}")
