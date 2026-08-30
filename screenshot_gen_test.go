//go:build windows && screenshotgen

package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// 真机截图生成器：打开真实设置窗口，抓取「常规」「关于」两页，叠合成 docs/screenshot.png。
// 改动 Go UI 代码后运行（需 -tags screenshotgen 避免 go test ./... 误跑）：
//   go generate ./...   （等价于 go test -tags screenshotgen -run RegenerateScreenshot -v）
// 输出为当前代码的真实渲染，自动跟随界面调整。
//
//go:generate go test -tags screenshotgen -run RegenerateScreenshot -v

type bmpInfoHeader struct {
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

var (
	tpGetWindowRect      = modUser32.NewProc("GetWindowRect")
	tpPrintWindow        = modUser32.NewProc("PrintWindow")
	tpCreateCompatibleDC = modGdi32.NewProc("CreateCompatibleDC")
	tpDeleteDC           = modGdi32.NewProc("DeleteDC")
	tpCreateCompatibleBm = modGdi32.NewProc("CreateCompatibleBitmap")
	tpGetDIBits          = modGdi32.NewProc("GetDIBits")
)

func capWin(hwnd uintptr) *image.RGBA {
	var rc rect
	tpGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	w := int(rc.right - rc.left)
	h := int(rc.bottom - rc.top)
	dc, _, _ := pGetDC.Call(0)
	defer pReleaseDC.Call(0, dc)
	mem, _, _ := tpCreateCompatibleDC.Call(dc)
	defer tpDeleteDC.Call(mem)
	bmp, _, _ := tpCreateCompatibleBm.Call(dc, uintptr(w), uintptr(h))
	defer pDeleteObject.Call(bmp)
	old, _, _ := pSelectObject.Call(mem, bmp)
	tpPrintWindow.Call(hwnd, mem, 0)
	pSelectObject.Call(mem, old)
	bih := bmpInfoHeader{biSize: uint32(unsafe.Sizeof(bmpInfoHeader{})), biWidth: int32(w), biHeight: -int32(h), biPlanes: 1, biBitCount: 32}
	buf := make([]byte, w*h*4)
	tpGetDIBits.Call(mem, bmp, 0, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bih)), 0)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			img.SetRGBA(x, y, color.RGBA{R: buf[i+2], G: buf[i+1], B: buf[i], A: 255})
		}
	}
	// flag 0 已干净（无 DWM 黑框），仅裁 1px 去毛边
	inset := 1
	bot := 2
	cropped := image.NewRGBA(image.Rect(0, 0, w-2*inset, h-inset-bot))
	for y := 0; y < h-inset-bot; y++ {
		for x := 0; x < w-2*inset; x++ {
			cropped.SetRGBA(x, y, img.RGBAAt(x+inset, y+inset))
		}
	}
	return cropped
}

// roundedMask 生成圆角 alpha 掩码（半径 r 的四角透明）。
func roundedMask(w, h, r int) *image.Alpha {
	m := image.NewAlpha(image.Rect(0, 0, w, h))
	rr := r * r
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			// 四个圆角之外透明
			corner := false
			if x < r && y < r {
				if (x-r)*(x-r)+(y-r)*(y-r) > rr {
					corner = true
				}
			}
			if x >= w-r && y < r {
				if (x-(w-r))*(x-(w-r))+(y-r)*(y-r) > rr {
					corner = true
				}
			}
			if x < r && y >= h-r {
				if (x-r)*(x-r)+(y-(h-r))*(y-(h-r)) > rr {
					corner = true
				}
			}
			if x >= w-r && y >= h-r {
				if (x-(w-r))*(x-(w-r))+(y-(h-r))*(y-(h-r)) > rr {
					corner = true
				}
			}
			if corner {
				a = 0
			}
			m.SetAlpha(x, y, color.Alpha{A: a})
		}
	}
	return m
}

func blend(dst *image.RGBA, src *image.RGBA, dx, dy int) {
	var mask *image.Alpha
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	r := 16
	mask = roundedMask(w, h, r)
	dstRect := image.Rect(dx, dy, dx+w, dy+h)
	draw.DrawMask(dst, dstRect, src, image.Point{}, mask, image.Point{}, draw.Over)
}

func drawShadow(dst *image.RGBA, x, y, w, h int) {
	for i := 5; i >= 1; i-- {
		ex := i * 2
		// 用简单半透明叠加近似阴影（在窗口区域下方绘制偏移的黑色层）
		ox, oy := x-ex/2, y-ex/2+6
		sw, sh := w+ex, h+ex
		for yy := oy; yy < oy+sh; yy++ {
			for xx := ox; xx < ox+sw; xx++ {
				if yy < 0 || yy >= dst.Bounds().Dy() || xx < 0 || xx >= dst.Bounds().Dx() {
					continue
				}
				// 只叠加在窗口主体之外（背景）——简化：叠加一个偏移的阴影矩形
				alpha := 8 + (5-i)*2
				cur := dst.RGBAAt(xx, yy)
				dst.SetRGBA(xx, yy, color.RGBA{R: uint8(int(cur.R)*(255-alpha)/255), G: uint8(int(cur.G)*(255-alpha)/255), B: uint8(int(cur.B)*(255-alpha)/255), A: 255})
			}
		}
	}
}

func TestRegenerateScreenshot(t *testing.T) {
	// 设置一个好看的版本号用于截图（否则为 dev / 未检测到）
	const fakeVersion = "0.3.19"
	appVersion = fakeVersion
	fake := t.TempDir()
	_ = os.MkdirAll(filepath.Join(fake, "node_modules", "@deepseek-ai", "dsh"), 0o755)
	_ = os.WriteFile(filepath.Join(fake, "node_modules", "@deepseek-ai", "dsh", "package.json"), []byte(`{"version":"0.1.1-rc.2"}`), 0o644)
	harnessDir = fake
	settingsAutoOn = true
	settingsSvcUp.Store(true)

	done := make(chan uintptr, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hwnd := createSettingsWindow()
		done <- hwnd
		if hwnd == 0 {
			return
		}
		for {
			var m msg
			ret, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(ret) <= 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	hwnd := <-done
	if hwnd == 0 {
		t.Fatal("settings window not created")
	}
	time.Sleep(300 * time.Millisecond)

	// 抓「关于」页
	settingsCat = 1
	settingsShowPane(hwnd)
	time.Sleep(120 * time.Millisecond)
	about := capWin(hwnd)

	// 抓「常规」页（服务状态置为运行中）
	settingsCat = 0
	settingsShowPane(hwnd)
	settingsSetServiceStatus(true)
	time.Sleep(120 * time.Millisecond)
	gen := capWin(hwnd)

	pPostMessageW.Call(hwnd, wmClose, 0, 0)

	// 叠合成 1240x880 层叠图
	W, H := 1240, 880
	out := image.NewRGBA(image.Rect(0, 0, W, H))
	bg := color.RGBA{R: 233, G: 238, B: 246}
	draw.Draw(out, out.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	aw, ah := about.Bounds().Dx(), about.Bounds().Dy()
	gw, gh := gen.Bounds().Dx(), gen.Bounds().Dy()

	drawShadow(out, 118, 44, aw, ah)
	blend(out, about, 118, 44)
	drawShadow(out, 178, 316, gw, gh)
	blend(out, gen, 178, 316)

	outPath := filepath.Join(".", "docs", "screenshot.png")
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, out); err != nil {
		t.Fatal(err)
	}
	t.Logf("regenerated %s", outPath)
}
