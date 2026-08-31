//go:build windows && docsgen

package main

// 设计感文档图生成器（非真机截图）：以网站同源设计语言（Noto Sans SC + 品牌蓝 #1D4ED8）
// 绘制轮播页与 README 主图。所有内容按 2x 渲染（视网膜级），网页/README 缩小显示时文字清晰。
// UI 调整后运行（需 -tags docsgen 避免 go test ./... 误跑）：
//   go generate ./...   （等价于 go test -tags docsgen -run RegenerateDocsImages -v）
// 输出：
//   docs/shots/*.png      网站轮播页（启动进度 / 常规 / 关于 / 日志 / 导出 / 导入）
//   docs/screenshot.png   README 主图（关于页 + 检查更新下载 DeepSeek Harness 新版本进度）

//go:generate go test -tags docsgen -run RegenerateDocsImages -v

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

type rgba = color.RGBA

var (
	cBG     = rgba{233, 238, 246, 255} // 页面冷调背景 #E9EEF6
	cWhite  = rgba{255, 255, 255, 255}
	cInk    = rgba{16, 24, 40, 255}    // #101828
	cSub    = rgba{102, 112, 133, 255} // #667085
	cBlue   = rgba{29, 78, 216, 255}   // #1D4ED8
	cCardBg = rgba{245, 247, 250, 255} // #F5F7FA
	cBorder = rgba{228, 231, 236, 255} // #E4E7EC
	cSel    = rgba{224, 234, 255, 255} // #E0EAFF
	cGreen  = rgba{22, 163, 74, 255}   // #16A34A
	cAmber  = rgba{217, 119, 6, 255}   // WARN
	cRed    = rgba{220, 38, 38, 255}   // ERROR
	cLine   = rgba{71, 84, 103, 255}   // 日志正文 #475467
)

// scale 全局渲染倍率：3x 超采样成图。文字按 3 倍分辨率渲染，显示端按逻辑尺寸（图片三分之一像素宽）
// 展示则等于精确 3x 缩小，文字锐利（比 1x/2x 灰度更清晰，接近系统级渲染观感）。
// README 主图用较小逻辑宽以贴合 GitHub 容器。
const scale = 3

// heroW README 主图逻辑宽度（≈GitHub markdown 内容宽度），避免 2x 成图被 GitHub 缩减发虚。
const heroW = 980

// ==================== 画布与矢量原语 ====================

type canvas struct {
	img *image.RGBA
}

func newCanvas(w, h int) *canvas {
	return &canvas{img: image.NewRGBA(image.Rect(0, 0, w*scale, h*scale))}
}

func (c *canvas) fillRect(x0, y0, x1, y1 int, col rgba) {
	draw.Draw(c.img, image.Rect(x0*scale, y0*scale, x1*scale, y1*scale), &image.Uniform{col}, image.Point{}, draw.Src)
}

// roundRectA 圆角矩形（SDF 抗锯齿），am 为整体不透明度（0~1）。
func (c *canvas) roundRectA(x0, y0, x1, y1, r int, col rgba, am float64) {
	x0 *= scale
	y0 *= scale
	x1 *= scale
	y1 *= scale
	r *= scale
	if x1 <= x0 || y1 <= y0 {
		return
	}
	w := float64(x1 - x0)
	h := float64(y1 - y0)
	rr := float64(r)
	if rr > w/2 {
		rr = w / 2
	}
	if rr > h/2 {
		rr = h / 2
	}
	if rr < 1 {
		rr = 1
	}
	cx := float64(x0) + w/2
	cy := float64(y0) + h/2
	hw := w/2 - rr
	hh := h/2 - rr
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			dx := math.Abs(float64(x)+0.5-cx) - hw
			dy := math.Abs(float64(y)+0.5-cy) - hh
			if dx < 0 {
				dx = 0
			}
			if dy < 0 {
				dy = 0
			}
			d := math.Sqrt(dx*dx+dy*dy) - rr
			// 边缘覆盖度钳制到 0..1，再乘整体透明度（否则矩形内部 am 被放大数倍）
			cov := 0.5 - d
			if cov <= 0 {
				continue
			}
			if cov > 1 {
				cov = 1
			}
			a := cov * am
			cur := c.img.RGBAAt(x, y)
			na := 1 - a
			c.img.SetRGBA(x, y, rgba{
				uint8(float64(col.R)*a + float64(cur.R)*na),
				uint8(float64(col.G)*a + float64(cur.G)*na),
				uint8(float64(col.B)*a + float64(cur.B)*na),
				255,
			})
		}
	}
}

func (c *canvas) roundRect(x0, y0, x1, y1, r int, col rgba) {
	c.roundRectA(x0, y0, x1, y1, r, col, 1)
}

// shadow 极淡的四边对称投影（4 层、4px 宽、低透明度）：仅用于明确窗口边界，不喧宾夺主。
func (c *canvas) shadow(x0, y0, w, h, r int) {
	for i := 4; i >= 1; i-- {
		c.roundRectA(x0-i, y0-i, x0+w+i, y0+h+i, r+i, rgba{16, 24, 40, 255}, 0.025)
	}
}

// ==================== 文本（GDI 灰度 → alpha 层） ====================

var (
	pCreateDIBSectionDoc = modGdi32.NewProc("CreateDIBSection")
	pGdiFlushDoc         = modGdi32.NewProc("GdiFlush")
)

type docBmpInfo struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

// docTextLayer 把文本渲染为 alpha 掩码（黑字白底 → 亮度转透明度），返回掩码与测量宽高。
func docTextLayer(s string, font uintptr, maxW int) (*image.RGBA, int, int) {
	dc, _, _ := pGetDC.Call(0)
	if dc == 0 {
		return nil, 0, 0
	}
	defer pReleaseDC.Call(0, dc)
	t, _ := syscall.UTF16PtrFromString(s)
	old, _, _ := pSelectObject.Call(dc, font)
	mrc := rect{0, 0, int32(maxW), 0}
	pDrawTextW.Call(dc, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&mrc)), dtCalcRect|dtWordBreak)
	pSelectObject.Call(dc, old)
	w := int(mrc.right - mrc.left)
	h := int(mrc.bottom - mrc.top)
	if w <= 0 || h <= 0 {
		return nil, 0, 0
	}
	w += 12 // 抗锯齿边缘余量
	h += 8

	mem, _, _ := pCreateCompatibleDC.Call(dc)
	bmi := docBmpInfo{
		biSize:        uint32(unsafe.Sizeof(docBmpInfo{})),
		biWidth:       int32(w),
		biHeight:      -int32(h),
		biPlanes:      1,
		biBitCount:    32,
		biCompression: 0,
	}
	var bits unsafe.Pointer
	hbmp, _, _ := pCreateDIBSectionDoc.Call(mem, uintptr(unsafe.Pointer(&bmi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmp == 0 {
		pDeleteDC.Call(mem)
		return nil, 0, 0
	}
	defer pDeleteObject.Call(hbmp)
	defer pDeleteDC.Call(mem)
	oldBmp, _, _ := pSelectObject.Call(mem, hbmp)
	if wb, _, _ := pGetStockObject.Call(whiteBrush); wb != 0 {
		pFillRect.Call(mem, uintptr(unsafe.Pointer(&rect{0, 0, int32(w), int32(h)})), wb)
	}
	pSelectObject.Call(mem, font)
	pSetTextColor.Call(mem, 0)
	pSetBkColor.Call(mem, 0xFFFFFF)
	pSetBkMode.Call(mem, bkOpaque)
	tr := rect{0, 0, int32(w), int32(h)}
	pDrawTextW.Call(mem, uintptr(unsafe.Pointer(t)), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), 0)
	pSelectObject.Call(mem, oldBmp)
	// 关键：把 GDI 批处理刷入 DIB 内存，否则直接读 bits 会拿到陈旧/未绘制数据
	pGdiFlushDoc.Call()

	mask := image.NewRGBA(image.Rect(0, 0, w, h))
	data := unsafe.Slice((*byte)(bits), w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			gray := int(data[(y*w+x)*4]) // 白底黑字时 RGB 三通道相同
			// alpha gamma 校正：GDI 灰度掩码的边缘像素是线性覆盖度，直接作为 alpha 会让笔画中调偏虚。
			// a' = a^(1/1.5) 提升中间调 alpha，笔画更实更黑，接近系统级文字渲染的锐度。
			cover := float64(255-gray) / 255
			if cover <= 0 {
				continue
			}
			a := uint8(math.Pow(cover, 1.0/1.5)*255 + 0.5)
			if a > 0 {
				mask.SetRGBA(x, y, rgba{0, 0, 0, a})
			}
		}
	}
	return mask, w, h
}

// textIn 在给定盒内绘制文本（左对齐 / 水平居中 / 右对齐，垂直居中）。
func (c *canvas) textIn(s string, x0, y0, x1, y1 int, font uintptr, col rgba, align string) {
	mask, w, h := docTextLayer(s, font, (x1-x0)*scale)
	if mask == nil {
		return
	}
	x := x0 * scale
	if align == "center" {
		x += ((x1 - x0) * scale - w) / 2
	} else if align == "right" {
		x = x1*scale - w
	}
	y := y0*scale + ((y1-y0)*scale-h)/2
	colored := image.NewRGBA(mask.Bounds())
	for yy := 0; yy < h; yy++ {
		for xx := 0; xx < w; xx++ {
			if a := mask.RGBAAt(xx, yy).A; a > 0 {
				// 预乘 alpha（image/draw 的 Over 按预乘语义合成）
				colored.SetRGBA(xx, yy, rgba{
					uint8(uint32(col.R) * uint32(a) / 255),
					uint8(uint32(col.G) * uint32(a) / 255),
					uint8(uint32(col.B) * uint32(a) / 255),
					a,
				})
			}
		}
	}
	draw.Draw(c.img, image.Rect(x, y, x+w, y+h), colored, image.Point{}, draw.Over)
}

// ==================== 字体 ====================

type fonts struct {
	head, title, body, bodyB, small, btn, btnS, mono uintptr
}

func loadFonts() *fonts {
	return &fonts{
		head:  makeFontQuality(21*scale, 400, antialiasQual),
		title: makeFontQuality(19*scale, 600, antialiasQual),
		body:  makeFontQuality(17*scale, 400, antialiasQual),
		bodyB: makeFontQuality(17*scale, 600, antialiasQual),
		small: makeFontQuality(13*scale, 400, antialiasQual),
		btn:   makeFontQuality(16*scale, 600, antialiasQual),
		btnS:  makeFontQuality(14*scale, 600, antialiasQual),
		mono:  makeMonoFontQuality(14*scale, antialiasQual),
	}
}

func (f *fonts) release() {
	for _, h := range []uintptr{f.head, f.title, f.body, f.bodyB, f.small, f.btn, f.btnS, f.mono} {
		if h != 0 {
			pDeleteObject.Call(h)
		}
	}
}

// ==================== 轮播页 ====================

const (
	slideW = 1100
	slideH = 660
)

func winControls(c *canvas, f *fonts, right, cy int) {
	glyphs := []string{"—", "□", "×"}
	for i, g := range glyphs {
		c.textIn(g, right-110+i*34, cy, right-76+i*34, cy+28, f.body, cSub, "center")
	}
}

// winBar 低矮标题栏（约 36px）：标题 + 窗口控制 + 分隔线。
func winBar(c *canvas, f *fonts, x, y, w int, title string) {
	c.textIn(title, x+32, y+6, x+180, y+32, f.title, cInk, "left")
	winControls(c, f, x+w, y+4)
	c.fillRect(x, y+36, x+w, y+38, cBorder)
}

// drawSlide 带窗口框架的轮播页；ww 为窗口宽度，content 绘制右侧内容区。
// cx 为内容左缘、rx 为内容右缘（含右内边距）、wy 为窗口顶。
func drawSlide(f *fonts, sel int, title string, ww int, content func(*canvas, *fonts, int, int, int)) *canvas {
	c := newCanvas(slideW, slideH)
	c.fillRect(0, 0, slideW, slideH, cBG)
	wx, wy, wh := 150, 62, 536
	c.shadow(wx, wy, ww, wh, 18)
	c.roundRect(wx, wy, wx+ww, wy+wh, 16, cWhite)
	winBar(c, f, wx, wy, ww, "设置")
	// 侧栏（浅灰卡片，留 16px 内边距）
	c.roundRect(wx+24, wy+46, wx+24+168, wy+496, 14, cCardBg)
	items := []string{"常规", "关于", "日志", "导出", "导入"}
	for i, it := range items {
		y := wy + 60 + i*50
		if i == sel {
			c.roundRect(wx+32, y-9, wx+32+152, y+35, 10, cSel)
			c.textIn(it, wx+46, y, wx+46+130, y+26, f.bodyB, cBlue, "left")
		} else {
			c.textIn(it, wx+46, y, wx+46+130, y+26, f.body, cSub, "left")
		}
	}
	// 页面标题
	c.textIn(title, wx+224, wy+38, wx+224+320, wy+66, f.head, cInk, "left")
	// 内容主区：右缘对齐内容底部的右内边距（与标题左缘对齐留白）
	content(c, f, wx+224, wx+ww-24, wy)
	return c
}

// card 白底内容卡片（1px 边框浅灰 + 白底 + 圆角），用于内容分组。
func card(c *canvas, x0, y0, x1, y1 int) {
	c.roundRect(x0, y0, x1, y1, 12, cBorder)
	c.roundRect(x0+1, y0+1, x1-1, y1-1, 11, cWhite)
}

// ctlTrack 绘制开关（轨道 + 圆钮）；right 为基础外框 x0，on 为状态。
func ctlTrack(c *canvas, x0, y0 int, on bool) {
	tw, th := 46, 26
	if on {
		c.roundRect(x0, y0, x0+tw, y0+th, th/2, cBlue)
		c.roundRect(x0+tw-22, y0+4, x0+tw-4, y0+th-4, 9, cWhite)
	} else {
		c.roundRect(x0, y0, x0+tw, y0+th, th/2, cBorder)
		c.roundRect(x0+4, y0+4, x0+22, y0+th-4, 9, cWhite)
	}
}

// ctlPill 绘制胶囊按钮（w×h，primary=品牌蓝/否则灰）；text 居中。
func ctlPill(c *canvas, f *fonts, x0, y0, w, h int, text string, primary bool) {
	fill := cBorder
	if primary {
		fill = cBlue
	}
	c.roundRect(x0, y0, x0+w, y0+h, h/2, fill)
	var col rgba
	if primary {
		col = cWhite
	} else {
		col = cInk
	}
	c.textIn(text, x0, y0, x0+w, y0+h, f.body, col, "center")
}

func drawGeneral(c *canvas, f *fonts, cx, rx, wy int) {
	// 卡片1：开机自启动 → 开关行
	card(c, cx, wy+76, rx, wy+150)
	c.textIn("开机自启动", cx+20, wy+88, cx+220, wy+114, f.body, cInk, "left")
	c.textIn("登录后自动启动后台服务并常驻托盘", cx+20, wy+120, cx+560, wy+140, f.small, cSub, "left")
	// 开关（轨道 ON + 圆钮，右对齐）
	ctlTrack(c, rx-64, wy+94, true)

	// 卡片2：后台服务状态 → 状态行 + 重启按钮
	card(c, cx, wy+166, rx, wy+250)
	c.roundRect(cx+20, wy+192, cx+28, wy+200, 4, cGreen)
	c.textIn("后台服务：运行中", cx+38, wy+184, cx+260, wy+210, f.body, cSub, "left")
	ctlPill(c, f, rx-164, wy+188, 144, 30, "重启后台服务", true)
}

func drawAbout(c *canvas, f *fonts, cx, rx, wy int) {
	// 卡片1：版本信息两行（标签左 / 值右）
	card(c, cx, wy+76, rx, wy+172)
	c.textIn("dsh-systray 版本号", cx+20, wy+92, cx+280, wy+116, f.body, cSub, "left")
	c.textIn("v0.3.12", rx-260, wy+88, rx-20, wy+118, f.title, cBlue, "right")
	c.textIn("DeepSeek Harness 版本号", cx+20, wy+136, cx+320, wy+160, f.body, cSub, "left")
	c.textIn("v0.1.1-rc.2", rx-260, wy+132, rx-20, wy+162, f.title, cBlue, "right")

	// 卡片2：预发布通道开关
	card(c, cx, wy+188, rx, wy+252)
	c.textIn("开启预发布通道", cx+20, wy+204, cx+220, wy+228, f.body, cInk, "left")
	c.textIn("alpha/beta/rc 预发布版", cx+20, wy+228, cx+300, wy+246, f.small, cSub, "left")
	ctlTrack(c, rx-64, wy+206, false)

	// 主操作：检查更新
	c.roundRect(cx, wy+276, cx+156, wy+316, 20, cBlue)
	c.textIn("检查更新", cx, wy+276, cx+156, wy+316, f.btn, cWhite, "center")
}

func drawLogs(c *canvas, f *fonts, cx, rx, wy int) {
	c.textIn(`C:\Users\demo\AppData\Roaming\dsh-systray\logs\app.log`, cx, wy+70, rx, wy+92, f.small, cSub, "left")
	// 选择器 + 清空（同排）
	c.roundRect(cx, wy+96, cx+160, wy+130, 8, cBorder)
	c.roundRect(cx+1, wy+97, cx+159, wy+129, 7, cWhite)
	c.textIn("app.log", cx+12, wy+96, cx+130, wy+130, f.body, cInk, "left")
	c.textIn("▾", cx+130, wy+96, cx+158, wy+130, f.body, cSub, "center")
	ctlPill(c, f, cx+175, wy+98, 92, 30, "清空", true)

	// 日志卡片（白底 + 边框，宽度充满内容区）
	card(c, cx, wy+148, rx, wy+452)
	lines := []struct {
		t, lvl, msg string
		col         rgba
	}{
		{"2026-08-30 10:31:02", "INFO", "server ready at http://127.0.0.1:3080", cBlue},
		{"2026-08-30 10:31:05", "INFO", "tray icon shown", cBlue},
		{"2026-08-30 10:31:21", "WARN", "update check skipped (dev build)", cAmber},
		{"2026-08-30 10:33:47", "INFO", "settings window opened", cBlue},
		{"2026-08-30 10:35:12", "INFO", "export started: 2 items", cBlue},
		{"2026-08-30 10:35:16", "INFO", "export finished: dsh-systray-export-20260830.zip", cBlue},
		{"2026-08-30 10:40:03", "ERROR", "port 3080 busy, retrying…", cRed},
		{"2026-08-30 10:40:06", "INFO", "server ready at http://127.0.0.1:3080", cBlue},
		{"2026-08-30 10:52:31", "INFO", "update available: v0.3.13", cBlue},
		{"2026-08-30 10:52:40", "INFO", "downloading update…", cBlue},
		{"2026-08-30 10:52:58", "INFO", "update installed, restarting", cBlue},
		{"2026-08-30 10:53:02", "INFO", "server ready at http://127.0.0.1:3080", cBlue},
	}
	y := wy + 166
	for _, ln := range lines {
		c.textIn(ln.t, cx+4, y, cx+136, y+22, f.mono, cSub, "left")
		c.textIn(ln.lvl+" ", cx+140, y, cx+200, y+22, f.mono, ln.col, "left")
		c.textIn(ln.msg, cx+202, y, rx-16, y+22, f.mono, cLine, "left")
		y += 24
	}
}

func drawExport(c *canvas, f *fonts, cx, rx, wy int) {
	rows := []struct {
		lbl, sub string
		on       bool
	}{
		{"所有历史会话", "sessions.zip · ~/.dsh/sessions", true},
		{"已安装的插件", "plugins.zip · 通过 dsh add 安装的插件", false},
		{"需要打包的文件目录", "files.zip · 恢复时选择解压位置", false},
	}
	y := wy + 78
	for _, r := range rows {
		card(c, cx, y, rx, y+66)
		if r.on {
			c.roundRect(cx+20, y+24, cx+38, y+42, 5, cBlue)
			c.textIn("✓", cx+20, y+21, cx+38, y+42, f.btnS, cWhite, "center")
		} else {
			c.roundRect(cx+20, y+24, cx+38, y+42, 5, cBorder)
			c.roundRect(cx+21, y+25, cx+37, y+41, 4, cWhite)
		}
		c.textIn(r.lbl, cx+52, y+12, cx+300, y+38, f.body, cInk, "left")
		c.textIn(r.sub, cx+52, y+38, rx-16, y+58, f.small, cSub, "left")
		y += 66
	}
	// 底部操作行：次操作「选择目录…」（仅文件目录需要） + 主操作「导出…」
	ctlPill(c, f, cx, y+18, 132, 34, "选择目录…", false)
	ctlPill(c, f, cx+148, y+18, 120, 34, "导出…", true)
	c.textIn("已选 1 项，点击「导出…」打包为 zip", cx, y+62, rx-16, y+82, f.small, cSub, "left")
}

func drawImport(c *canvas, f *fonts, cx, rx, wy int) {
	// 顶部：添加导入压缩包按钮
	ctlPill(c, f, cx, wy+80, 190, 30, "添加导入压缩包…", true)
	c.textIn(`C:\Users\demo\Downloads\dsh-systray-export-20260830-103102-a1b2c3.zip`, cx, wy+124, rx-16, wy+146, f.small, cSub, "left")
	rows := []string{"所有历史会话（12.4 MB）", "已安装的插件（3.1 MB）", "需要打包的文件目录"}
	y := wy + 164
	for _, lbl := range rows {
		card(c, cx, y, rx, y+44)
		c.textIn(lbl, cx+20, y+8, cx+420, y+34, f.body, cInk, "left")
		ctlPill(c, f, rx-104, y+7, 84, 30, "恢复", true)
		y += 44
	}
	c.textIn("解析成功：共 3 个可恢复项，点击右侧「恢复」逐项恢复。", cx, y+10, rx-16, y+34, f.small, cSub, "left")
}

func drawSplash(c *canvas, f *fonts) {
	c.fillRect(0, 0, slideW, slideH, cBG)
	wx, wy, ww, wh := 310, 240, 480, 150
	c.shadow(wx, wy, ww, wh, 18)
	c.roundRect(wx, wy, wx+ww, wy+wh, 16, cWhite)
	winBar(c, f, wx, wy, ww, "DeepSeek Harness")
	c.textIn("正在安装运行环境依赖（首次约 2-5 分钟）…", wx+30, wy+48, wx+ww-30, wy+82, f.body, cSub, "center")
	c.roundRect(wx+50, wy+100, wx+ww-50, wy+110, 5, cBorder)
	c.roundRect(wx+50, wy+100, wx+50+int(float64(ww-100)*0.35), wy+110, 5, cBlue)
}

// ==================== README 主图：检查更新下载 DeepSeek Harness 新版本 ====================

func drawHero(c *canvas, f *fonts) {
	// 底图：关于页（窄窗口 800，已含检查更新按钮）
	base := drawSlide(f, 1, "关于", 800, drawAbout)
	draw.Draw(c.img, c.img.Bounds(), base.img, image.Point{}, draw.Src)
	// 叠加：下载进度窗口（位于「检查更新」按钮正下方，横向居中，不遮挡按钮）
	wx, wy, ww, wh := 282, 400, 420, 140
	c.shadow(wx, wy, ww, wh, 18)
	c.roundRect(wx, wy, wx+ww, wy+wh, 16, cWhite)
	winBar(c, f, wx, wy, ww, "DeepSeek Harness")
	c.textIn("正在下载 DeepSeek Harness 新版本…", wx+30, wy+48, wx+ww-30, wy+82, f.small, cSub, "center")
	c.roundRect(wx+50, wy+100, wx+ww-50, wy+110, 5, cBorder)
	c.roundRect(wx+50, wy+100, wx+50+int(float64(ww-100)*0.55), wy+110, 5, cBlue)
}

// ==================== 生成与自检 ====================

func (c *canvas) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	return png.Encode(fh, c.img)
}

func expectColor(t *testing.T, img *image.RGBA, x, y int, want rgba, label string) {
	got := img.RGBAAt(x*scale, y*scale)
	if absDiff(got.R, want.R) > 8 || absDiff(got.G, want.G) > 8 || absDiff(got.B, want.B) > 8 {
		t.Errorf("%s @(%d,%d): got %v, want %v", label, x, y, got, want)
	}
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func TestRegenerateDocsImages(t *testing.T) {
	f := loadFonts()
	defer f.release()

	slides := []struct {
		name string
		draw func() *canvas
		chk  func(*testing.T, *image.RGBA)
	}{
		{"splash", func() *canvas { c := newCanvas(slideW, slideH); drawSplash(c, f); return c },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "splash bg")
				expectColor(t, img, 600, 260, cWhite, "splash window")
				expectColor(t, img, 380, 345, cBlue, "splash progress")
			}},
		{"general", func() *canvas { return drawSlide(f, 0, "常规", 920, drawGeneral) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "general bg")
				expectColor(t, img, 700, 175, cWhite, "general card")
				expectColor(t, img, 995, 168, cBlue, "general toggle")
			}},
		{"about", func() *canvas { return drawSlide(f, 1, "关于", 920, drawAbout) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "about bg")
				expectColor(t, img, 700, 186, cWhite, "about card")
			}},
		{"logs", func() *canvas { return drawSlide(f, 2, "日志", 920, drawLogs) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "logs bg")
				expectColor(t, img, 450, 175, cWhite, "logs select")
				expectColor(t, img, 700, 300, cWhite, "logs card")
			}},
		{"export", func() *canvas { return drawSlide(f, 3, "导出", 920, drawExport) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "export bg")
				expectColor(t, img, 403, 172, cBlue, "export checkbox")
			}},
		{"import", func() *canvas { return drawSlide(f, 4, "导入", 920, drawImport) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "import bg")
				expectColor(t, img, 984, 248, cBlue, "import button")
			}},
	}
	for _, s := range slides {
		c := s.draw()
		path := filepath.Join("docs", "shots", s.name+".png")
		if err := c.save(path); err != nil {
			t.Fatal(err)
		}
		s.chk(t, c.img)
		t.Logf("generated %s (%dx%d)", path, c.img.Bounds().Dx(), c.img.Bounds().Dy())
	}

	hero := newCanvas(heroW, slideH)
	drawHero(hero, f)
	heroPath := filepath.Join("docs", "screenshot.png")
	if err := hero.save(heroPath); err != nil {
		t.Fatal(err)
	}
	expectColor(t, hero.img, 20, 20, cBG, "hero bg")
	expectColor(t, hero.img, 500, 450, cWhite, "hero progress window")
	expectColor(t, hero.img, 420, 505, cBlue, "hero progress fill")
	t.Logf("generated %s (%dx%d)", heroPath, hero.img.Bounds().Dx(), hero.img.Bounds().Dy())
}
