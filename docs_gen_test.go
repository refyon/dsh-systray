//go:build windows && docsgen

package main

// 设计感文档图生成器（非真机截图）：以网站同源设计语言（Noto Sans SC + 品牌蓝 #1D4ED8）
// 绘制轮播页与 README 功能图。UI 调整后运行（需 -tags docsgen 避免 go test ./... 误跑）：
//   go generate ./...   （等价于 go test -tags docsgen -run RegenerateDocsImages -v）
// 输出：
//   docs/shots/*.png      网站轮播页（启动进度 / 常规 / 关于 / 日志 / 导出 / 导入）
//   docs/features/*.png   README 功能聚焦图（托盘 / loading / 菜单 / 设置 / 单实例 / 更新）

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

// ==================== 画布与矢量原语 ====================

type canvas struct {
	img *image.RGBA
}

func newCanvas(w, h int) *canvas {
	return &canvas{img: image.NewRGBA(image.Rect(0, 0, w, h))}
}

func (c *canvas) fillRect(x0, y0, x1, y1 int, col rgba) {
	draw.Draw(c.img, image.Rect(x0, y0, x1, y1), &image.Uniform{col}, image.Point{}, draw.Src)
}

// roundRectA 圆角矩形（SDF 抗锯齿），am 为整体不透明度（0~1）。
func (c *canvas) roundRectA(x0, y0, x1, y1, r int, col rgba, am float64) {
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
			a := (0.5 - d) * am
			if a <= 0 {
				continue
			}
			if a > 1 {
				a = 1
			}
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

// shadow 多层偏移半透明圆角矩形，近似柔和投影。
func (c *canvas) shadow(x0, y0, x1, y1, r int) {
	for i := 5; i >= 1; i-- {
		ex := i * 2
		c.roundRectA(x0-ex/2, y0-ex/2+4, x1+ex/2, y1+ex/2+4, r+ex/2, rgba{16, 24, 40, 255}, 0.035)
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
	w += 6 // 抗锯齿边缘余量
	h += 4

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
			if a := uint8(255 - gray); a > 0 {
				mask.SetRGBA(x, y, rgba{0, 0, 0, a})
			}
		}
	}
	return mask, w, h
}

// textIn 在给定盒内绘制文本（左对齐或水平居中，垂直居中）。
func (c *canvas) textIn(s string, x0, y0, x1, y1 int, font uintptr, col rgba, align string) {
	mask, w, h := docTextLayer(s, font, x1-x0)
	if mask == nil {
		return
	}
	x := x0
	if align == "center" {
		x = x0 + (x1-x0-w)/2
	}
	y := y0 + (y1-y0-h)/2
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
		head:  makeFontQuality(21, 400, antialiasQual),
		title: makeFontQuality(19, 600, antialiasQual),
		body:  makeFontQuality(17, 400, antialiasQual),
		bodyB: makeFontQuality(17, 600, antialiasQual),
		small: makeFontQuality(13, 400, antialiasQual),
		btn:   makeFontQuality(16, 600, antialiasQual),
		btnS:  makeFontQuality(14, 600, antialiasQual),
		mono:  makeMonoFont(14),
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
		c.textIn(g, right-110+i*34, cy, right-76+i*34, cy+30, f.body, cSub, "center")
	}
}

func winBar(c *canvas, f *fonts, x, y, w int, title string) {
	c.textIn(title, x+32, y+16, x+180, y+46, f.title, cInk, "left")
	winControls(c, f, x+w, y+14)
	c.fillRect(x, y+46, x+w, y+48, cBorder)
}

// drawSlide 带窗口框架的轮播页；content 绘制右侧内容区（cx 为内容左缘，wy 为窗口顶）。
func drawSlide(f *fonts, sel int, title string, content func(*canvas, *fonts, int, int)) *canvas {
	c := newCanvas(slideW, slideH)
	c.fillRect(0, 0, slideW, slideH, cBG)
	wx, wy, ww, wh := 150, 62, 920, 536
	c.shadow(wx, wy, ww, wh, 18)
	c.roundRect(wx, wy, wx+ww, wy+wh, 16, cWhite)
	winBar(c, f, wx, wy, ww, "设置")
	// 侧栏
	c.roundRect(wx+24, wy+62, wx+24+168, wy+496, 14, cCardBg)
	items := []string{"常规", "关于", "日志", "导出", "导入"}
	for i, it := range items {
		y := wy + 74 + i*52
		if i == sel {
			c.roundRect(wx+32, y-9, wx+32+152, y+35, 10, cSel)
			c.textIn(it, wx+46, y, wx+46+130, y+26, f.bodyB, cBlue, "left")
		} else {
			c.textIn(it, wx+46, y, wx+46+130, y+26, f.body, cSub, "left")
		}
	}
	// 页面标题
	c.textIn(title, wx+232, wy+56, wx+232+300, wy+84, f.head, cInk, "left")
	content(c, f, wx+232, wy)
	return c
}

func drawGeneral(c *canvas, f *fonts, cx, wy int) {
	// 卡片 1：开机自启动
	c.roundRect(cx-14, wy+70, cx+646, wy+150, 12, cBorder)
	c.roundRect(cx-13, wy+71, cx+645, wy+149, 11, cCardBg)
	c.textIn("开机自启动", cx+14, wy+84, cx+200, wy+110, f.body, cInk, "left")
	c.roundRect(cx+130, wy+91, cx+176, wy+117, 13, cBlue) // 开关轨道 ON
	c.roundRect(cx+152, wy+95, cx+172, wy+115, 10, cWhite) // 圆钮
	c.textIn("登录后自动启动后台服务并常驻托盘", cx+14, wy+116, cx+440, wy+140, f.small, cSub, "left")
	// 卡片 2：后台服务
	c.roundRect(cx-14, wy+170, cx+646, wy+268, 12, cBorder)
	c.roundRect(cx-13, wy+171, cx+645, wy+267, 11, cCardBg)
	c.roundRect(cx+2, wy+198, cx+10, wy+206, 4, cGreen)
	c.textIn("后台服务：运行中", cx+18, wy+190, cx+280, wy+216, f.body, cSub, "left")
	c.roundRect(cx, wy+216, cx+150, wy+256, 20, cBlue)
	c.textIn("重启后台服务", cx, wy+216, cx+150, wy+256, f.btn, cWhite, "center")
}

func drawAbout(c *canvas, f *fonts, cx, wy int) {
	c.textIn("dsh-systray 版本号", cx, wy+116, cx+320, wy+142, f.body, cSub, "left")
	c.textIn("v0.3.12", cx, wy+142, cx+320, wy+168, f.title, cBlue, "left")
	c.textIn("DeepSeek Harness 版本号", cx, wy+178, cx+360, wy+204, f.body, cSub, "left")
	c.textIn("v0.1.1-rc.2", cx, wy+204, cx+320, wy+230, f.title, cBlue, "left")
	c.roundRect(cx, wy+244, cx+150, wy+288, 22, cBlue)
	c.textIn("检查更新", cx, wy+244, cx+150, wy+288, f.btn, cWhite, "center")
}

func drawLogs(c *canvas, f *fonts, cx, wy int) {
	c.textIn(`C:\Users\lenovo\AppData\Roaming\dsh-systray\logs\app.log`, cx, wy+70, cx+630, wy+94, f.small, cSub, "left")
	// 现代选择器
	c.roundRect(cx, wy+98, cx+160, wy+132, 8, cBorder)
	c.roundRect(cx+1, wy+99, cx+159, wy+131, 7, cWhite)
	c.textIn("app.log", cx+12, wy+98, cx+130, wy+132, f.body, cInk, "left")
	c.textIn("▾", cx+130, wy+98, cx+158, wy+132, f.body, cSub, "center")
	// 清空
	c.roundRect(cx+175, wy+99, cx+267, wy+131, 16, cBlue)
	c.textIn("清空", cx+175, wy+99, cx+267, wy+131, f.btnS, cWhite, "center")
	// 日志卡片
	c.roundRect(cx-14, wy+144, cx+646, wy+444, 12, cBorder)
	c.roundRect(cx-13, wy+145, cx+645, wy+443, 11, cCardBg)
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
	y := wy + 158
	for _, ln := range lines {
		c.textIn(ln.t, cx+2, y, cx+134, y+22, f.mono, cSub, "left")
		c.textIn(ln.lvl+" ", cx+136, y, cx+196, y+22, f.mono, ln.col, "left")
		c.textIn(ln.msg, cx+198, y, cx+630, y+22, f.mono, cLine, "left")
		y += 22
	}
}

func drawExport(c *canvas, f *fonts, cx, wy int) {
	rows := []struct {
		lbl, sub string
		on       bool
	}{
		{"所有历史会话", "sessions.zip · ~/.dsh/sessions", true},
		{"已安装的插件", "plugins.zip · ~/.dsh/profiles/node_modules", false},
		{"需要打包的文件目录", "files.zip · 恢复时选择解压位置", false},
	}
	for i, r := range rows {
		y := wy + 88 + i*52
		if r.on {
			c.roundRect(cx, y+2, cx+18, y+20, 5, cBlue)
			c.textIn("✓", cx, y+2, cx+18, y+20, f.btnS, cWhite, "center")
		} else {
			c.roundRect(cx, y+2, cx+18, y+20, 5, cBorder)
			c.roundRect(cx+1, y+3, cx+17, y+19, 4, cWhite)
		}
		c.textIn(r.lbl, cx+30, y, cx+300, y+24, f.body, cInk, "left")
		c.textIn(r.sub, cx+30, y+26, cx+620, y+46, f.small, cSub, "left")
	}
	c.roundRect(cx, wy+256, cx+110, wy+288, 16, cBlue)
	c.textIn("选择目录…", cx, wy+256, cx+110, wy+288, f.btnS, cWhite, "center")
	c.roundRect(cx, wy+314, cx+120, wy+350, 18, cBlue)
	c.textIn("导出…", cx, wy+314, cx+120, wy+350, f.btn, cWhite, "center")
	c.textIn("已选 1 项，点击「导出…」打包为 zip", cx, wy+372, cx+620, wy+394, f.small, cSub, "left")
}

func drawImport(c *canvas, f *fonts, cx, wy int) {
	c.roundRect(cx, wy+80, cx+180, wy+114, 17, cBlue)
	c.textIn("添加导入压缩包…", cx, wy+80, cx+180, wy+114, f.btnS, cWhite, "center")
	c.textIn(`C:\Users\lenovo\Downloads\dsh-systray-export-20260830-103102-a1b2c3.zip`, cx, wy+128, cx+630, wy+152, f.small, cSub, "left")
	rows := []string{"所有历史会话（12.4 MB）", "已安装的插件（3.1 MB）", "需要打包的文件目录"}
	for i, lbl := range rows {
		y := wy + 176 + i*44
		c.textIn(lbl, cx, y+2, cx+420, y+28, f.body, cInk, "left")
		c.roundRect(cx+530, y, cx+626, y+30, 15, cBlue)
		c.textIn("恢复", cx+530, y, cx+626, y+30, f.btnS, cWhite, "center")
	}
	c.textIn("解析成功：共 3 个可恢复项，点击右侧「恢复」逐项恢复。", cx, wy+312, cx+640, wy+336, f.small, cSub, "left")
}

func drawSplash(c *canvas, f *fonts) {
	c.fillRect(0, 0, slideW, slideH, cBG)
	wx, wy, ww, wh := 310, 240, 480, 180
	c.shadow(wx, wy, ww, wh, 18)
	c.roundRect(wx, wy, wx+ww, wy+wh, 16, cWhite)
	winBar(c, f, wx, wy, ww, "DeepSeek Harness")
	c.textIn("正在启动后台服务…", wx+30, wy+70, wx+ww-30, wy+104, f.body, cSub, "center")
	c.roundRect(wx+50, wy+124, wx+ww-50, wy+134, 5, cBorder)
	c.roundRect(wx+50, wy+124, wx+50+int(float64(ww-100)*0.35), wy+134, 5, cBlue)
}

// ==================== README 功能图 ====================

func featureCanvas() *canvas {
	c := newCanvas(460, 260)
	c.roundRect(0, 0, 460, 260, 12, cBorder)
	c.roundRect(1, 1, 459, 259, 11, cCardBg)
	return c
}

// boxScale 盒式降采样（图标缩放更平滑）。
func boxScale(src image.Image, target int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, target, target))
	for y := 0; y < target; y++ {
		for x := 0; x < target; x++ {
			x0, x1 := x*sw/target, (x+1)*sw/target
			y0, y1 := y*sh/target, (y+1)*sh/target
			var r, g, bl, a, n int
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					pr, pg, pb, pa := src.At(xx+b.Min.X, yy+b.Min.Y).RGBA()
					r += int(pr >> 8)
					g += int(pg >> 8)
					bl += int(pb >> 8)
					a += int(pa >> 8)
					n++
				}
			}
			if n > 0 {
				dst.SetRGBA(x, y, rgba{uint8(r / n), uint8(g / n), uint8(bl / n), uint8(a / n)})
			}
		}
	}
	return dst
}

func drawFeatTray(c *canvas, f *fonts) {
	fh, err := os.Open("app-icon.png")
	if err != nil {
		return
	}
	defer fh.Close()
	src, err := png.Decode(fh)
	if err != nil {
		return
	}
	icon := boxScale(src, 150)
	draw.Draw(c.img, image.Rect(155, 55, 305, 205), icon, image.Point{}, draw.Over)
}

func drawFeatSplash(c *canvas, f *fonts) {
	wx, wy, ww, wh := 60, 60, 340, 140
	c.shadow(wx, wy, ww, wh, 14)
	c.roundRect(wx, wy, wx+ww, wy+wh, 12, cWhite)
	winBar(c, f, wx, wy, ww, "DeepSeek Harness")
	c.textIn("正在启动后台服务…", wx+20, wy+66, wx+ww-20, wy+92, f.small, cSub, "center")
	c.roundRect(wx+30, wy+108, wx+ww-30, wy+116, 4, cBorder)
	c.roundRect(wx+30, wy+108, wx+30+int(float64(ww-60)*0.35), wy+116, 4, cBlue)
}

func drawFeatMenu(c *canvas, f *fonts) {
	wx, wy, ww, wh := 90, 18, 280, 224
	c.shadow(wx, wy, ww, wh, 14)
	c.roundRect(wx, wy, wx+ww, wy+wh, 12, cWhite)
	c.roundRect(wx+1, wy+1, wx+ww-1, wy+wh-1, 11, cWhite)
	c.textIn("后台服务：运行中", wx+22, wy+10, wx+ww-22, wy+38, f.small, cSub, "left")
	c.fillRect(wx+18, wy+46, wx+ww-18, wy+48, cBorder)
	c.textIn("打开 Web UI", wx+22, wy+56, wx+ww-22, wy+84, f.body, cInk, "left")
	c.textIn("设置", wx+22, wy+88, wx+ww-22, wy+116, f.body, cInk, "left")
	c.fillRect(wx+18, wy+124, wx+ww-18, wy+126, cBorder)
	c.textIn("退出", wx+22, wy+134, wx+ww-22, wy+162, f.body, cInk, "left")
}

func drawFeatSettings(c *canvas, f *fonts) {
	c.roundRect(20, 36, 440, 98, 10, cBorder)
	c.roundRect(21, 37, 439, 97, 9, cCardBg)
	c.textIn("开机自启动", 34, 54, 200, 80, f.body, cInk, "left")
	c.roundRect(160, 54, 206, 80, 13, cBlue)
	c.roundRect(182, 58, 202, 78, 10, cWhite)
	c.roundRect(20, 114, 440, 190, 10, cBorder)
	c.roundRect(21, 115, 439, 189, 9, cCardBg)
	c.roundRect(34, 146, 42, 154, 4, cGreen)
	c.textIn("后台服务：运行中", 50, 138, 260, 164, f.small, cSub, "left")
	c.roundRect(34, 164, 184, 204, 20, cBlue)
	c.textIn("重启后台服务", 34, 164, 184, 204, f.btnS, cWhite, "center")
}

func drawFeatSingle(c *canvas, f *fonts) {
	wx, wy, ww, wh := 70, 30, 320, 200
	c.shadow(wx, wy, ww, wh, 14)
	c.roundRect(wx, wy, wx+ww, wy+wh, 12, cWhite)
	winBar(c, f, wx, wy, ww, "DeepSeek Harness")
	c.textIn("DeepSeek Harness 已在运行中，", wx+20, wy+72, wx+ww-20, wy+100, f.body, cSub, "center")
	c.textIn("请使用系统托盘图标操作。", wx+20, wy+102, wx+ww-20, wy+130, f.body, cSub, "center")
	c.roundRect(wx+120, wy+150, wx+200, wy+182, 16, cBlue)
	c.textIn("确定", wx+120, wy+150, wx+200, wy+182, f.btnS, cWhite, "center")
}

func drawFeatUpdate(c *canvas, f *fonts) {
	wx, wy, ww, wh := 60, 60, 340, 140
	c.shadow(wx, wy, ww, wh, 14)
	c.roundRect(wx, wy, wx+ww, wy+wh, 12, cWhite)
	winBar(c, f, wx, wy, ww, "DeepSeek Harness")
	c.textIn("正在下载更新 v0.3.13…", wx+20, wy+66, wx+ww-20, wy+92, f.small, cSub, "center")
	c.roundRect(wx+30, wy+108, wx+ww-30, wy+116, 4, cBorder)
	c.roundRect(wx+30, wy+108, wx+30+int(float64(ww-60)*0.62), wy+116, 4, cBlue)
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
	got := img.RGBAAt(x, y)
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
				expectColor(t, img, 390, 300, cWhite, "splash window")
				expectColor(t, img, 380, 369, cBlue, "splash progress")
			}},
		{"general", func() *canvas { return drawSlide(f, 0, "常规", drawGeneral) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "general bg")
				expectColor(t, img, 950, 152, cCardBg, "general card1")
				expectColor(t, img, 522, 166, cBlue, "general toggle")
				expectColor(t, img, 950, 262, cCardBg, "general card2")
			}},
		{"about", func() *canvas { return drawSlide(f, 1, "关于", drawAbout) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "about bg")
				expectColor(t, img, 510, 158, cWhite, "about body")
			}},
		{"logs", func() *canvas { return drawSlide(f, 2, "日志", drawLogs) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "logs bg")
				expectColor(t, img, 460, 177, cWhite, "logs select")
				expectColor(t, img, 600, 212, cCardBg, "logs card")
			}},
		{"export", func() *canvas { return drawSlide(f, 3, "导出", drawExport) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "export bg")
				expectColor(t, img, 390, 160, cBlue, "export checkbox")
			}},
		{"import", func() *canvas { return drawSlide(f, 4, "导入", drawImport) },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 20, 20, cBG, "import bg")
				expectColor(t, img, 472, 159, cBlue, "import button")
			}},
	}
	for _, s := range slides {
		c := s.draw()
		path := filepath.Join("docs", "shots", s.name+".png")
		if err := c.save(path); err != nil {
			t.Fatal(err)
		}
		s.chk(t, c.img)
		t.Logf("generated %s", path)
	}

	features := []struct {
		name string
		draw func() *canvas
		chk  func(*testing.T, *image.RGBA)
	}{
		{"tray", func() *canvas { c := featureCanvas(); drawFeatTray(c, f); return c },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 30, 30, cCardBg, "tray bg")
			}},
		{"splash", func() *canvas { c := featureCanvas(); drawFeatSplash(c, f); return c },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 230, 100, cWhite, "feat splash window")
				expectColor(t, img, 110, 172, cBlue, "feat splash progress")
			}},
		{"menu", func() *canvas { c := featureCanvas(); drawFeatMenu(c, f); return c },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 230, 130, cWhite, "menu card")
			}},
		{"settings", func() *canvas { c := featureCanvas(); drawFeatSettings(c, f); return c },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 60, 60, cCardBg, "settings card1")
				expectColor(t, img, 175, 67, cBlue, "settings toggle")
			}},
		{"single", func() *canvas { c := featureCanvas(); drawFeatSingle(c, f); return c },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 230, 90, cWhite, "single dialog")
			}},
		{"update", func() *canvas { c := featureCanvas(); drawFeatUpdate(c, f); return c },
			func(t *testing.T, img *image.RGBA) {
				expectColor(t, img, 230, 100, cWhite, "update window")
				expectColor(t, img, 130, 172, cBlue, "update progress")
			}},
	}
	for _, s := range features {
		c := s.draw()
		path := filepath.Join("docs", "features", s.name+".png")
		if err := c.save(path); err != nil {
			t.Fatal(err)
		}
		s.chk(t, c.img)
		t.Logf("generated %s", path)
	}
}
