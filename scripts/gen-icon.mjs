// scripts/gen-icon.mjs — 图标生成：纯 Node、零外部依赖（不依赖外部 SVG / sharp）。
//
// 输入：icon_gen.go 中已嵌入的浅色鲸鱼 ICO（iconDataB64）与 macOS 模板 PNG（iconTemplateB64）。
// 输出：icon_gen.go（浅色 + 深色双主题 ICO + 模板 PNG）、icon.ico、preview-dark.png。
// 用法：node scripts/gen-icon.mjs [输出路径]

import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { deflateSync, inflateSync } from 'node:zlib'

const __dirname = dirname(fileURLToPath(import.meta.url))
const root = join(__dirname, '..')

// ---- 从现有 icon_gen.go 提取已嵌入的图标数据（主 ICO + 模板 PNG） ----
const genPath = join(root, 'icon_gen.go')
const genSrc = readFileSync(genPath, 'utf8')
function extractConst(name) {
  const marker = 'const ' + name + ' = `'
  const start = genSrc.indexOf(marker)
  if (start < 0) throw new Error(name + ' not found in icon_gen.go')
  const b64Start = start + marker.length
  const end = genSrc.indexOf('`', b64Start)
  if (end < 0) throw new Error(name + ' unterminated in icon_gen.go')
  return genSrc.slice(b64Start, end)
}
const icoSrc = Buffer.from(extractConst('iconDataB64'), 'base64')
const template = Buffer.from(extractConst('iconTemplateB64'), 'base64')

// ---- 纯 Node PNG 编解码（zlib deflate/inflate，无 sharp 依赖） ----
const CRC_TABLE = (() => {
  const t = new Int32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xEDB88320 ^ (c >>> 1) : c >>> 1
    t[n] = c
  }
  return t
})()
function crc32(buf) {
  let c = 0xFFFFFFFF
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xFF] ^ (c >>> 8)
  return (c ^ 0xFFFFFFFF) >>> 0
}
function chunk(type, data) {
  const len = Buffer.alloc(4)
  len.writeUInt32BE(data.length)
  const t = Buffer.from(type, 'ascii')
  const crc = Buffer.alloc(4)
  crc.writeUInt32BE(crc32(Buffer.concat([t, data])))
  return Buffer.concat([len, t, data, crc])
}
function encodePNG(width, height, rgba) {
  const stride = width * 4
  const raw = Buffer.alloc((stride + 1) * height)
  for (let y = 0; y < height; y++) {
    raw[y * (stride + 1)] = 0
    rgba.copy(raw, y * (stride + 1) + 1, y * stride, (y + 1) * stride)
  }
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(width, 0)
  ihdr.writeUInt32BE(height, 4)
  ihdr[8] = 8
  ihdr[9] = 6 // RGBA
  const sig = Buffer.from([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A])
  return Buffer.concat([sig, chunk('IHDR', ihdr), chunk('IDAT', deflateSync(raw, { level: 9 })), chunk('IEND', Buffer.alloc(0))])
}
function decodePNG(buf) {
  if (buf.length < 8 || buf.readUInt32BE(0) !== 0x89504E47) throw new Error('not a png')
  let width = 0
  let height = 0
  let bit = 0
  let color = 0
  const idat = []
  let off = 8
  while (off + 8 <= buf.length) {
    const len = buf.readUInt32BE(off)
    const type = buf.toString('ascii', off + 4, off + 8)
    const data = buf.subarray(off + 8, off + 8 + len)
    if (type === 'IHDR') {
      width = data.readUInt32BE(0)
      height = data.readUInt32BE(4)
      bit = data[8]
      color = data[9]
    } else if (type === 'IDAT') {
      idat.push(data)
    } else if (type === 'IEND') {
      break
    }
    off += 12 + len
  }
  if (bit !== 8 || color !== 6) throw new Error('unsupported png bit=' + bit + ' color=' + color)
  const raw = inflateSync(Buffer.concat(idat))
  const stride = width * 4
  const out = Buffer.alloc(width * height * 4)
  const paeth = (a, b, c) => {
    const p = a + b - c
    const pa = Math.abs(p - a)
    const pb = Math.abs(p - b)
    const pc = Math.abs(p - c)
    return pa <= pb && pa <= pc ? a : pb <= pc ? b : c
  }
  for (let y = 0; y < height; y++) {
    const f = raw[y * (stride + 1)]
    const line = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1))
    const prev = y > 0 ? out.subarray((y - 1) * stride, y * stride) : null
    for (let x = 0; x < stride; x++) {
      const a = x >= 4 ? line[x - 4] : 0
      const b = prev ? prev[x] : 0
      const c = x >= 4 && prev ? prev[x - 4] : 0
      let v = line[x]
      if (f === 1) v = (v + a) & 0xFF
      else if (f === 2) v = (v + b) & 0xFF
      else if (f === 3) v = (v + ((a + b) >> 1)) & 0xFF
      else if (f === 4) v = (v + paeth(a, b, c)) & 0xFF
      out[y * stride + x] = v
    }
  }
  return { width, height, data: out }
}
function parseICO(ico) {
  const count = ico.readUInt16LE(4)
  const out = []
  for (let i = 0; i < count; i++) {
    const off = 6 + i * 16
    let w = ico[off]
    let h = ico[off + 1]
    if (w === 0) w = 256
    if (h === 0) h = 256
    const size = ico.readUInt32LE(off + 8)
    const dataOff = ico.readUInt32LE(off + 12)
    out.push({ size: w, data: ico.subarray(dataOff, dataOff + size) })
  }
  return out
}
function makeICO(pngs) {
  const header = Buffer.alloc(6)
  header.writeUInt16LE(0, 0)
  header.writeUInt16LE(1, 2)
  header.writeUInt16LE(pngs.length, 4)
  let offset = 6 + 16 * pngs.length
  const entries = []
  for (const { size, data } of pngs) {
    const e = Buffer.alloc(16)
    e[0] = size >= 256 ? 0 : size
    e[1] = size >= 256 ? 0 : size
    e[2] = 0
    e[3] = 0
    e.writeUInt16LE(1, 4)
    e.writeUInt16LE(32, 6)
    e.writeUInt32LE(data.length, 8)
    e.writeUInt32LE(offset, 12)
    entries.push(e)
    offset += data.length
  }
  return Buffer.concat([header, ...entries, ...pngs.map((p) => p.data)])
}
// 将 PNG 中不透明像素重着色为垂直渐变（顶部 topRGB → 底部 botRGB），保留 alpha。
function recolorPNG(png, topRGB, botRGB) {
  const { width, height, data } = decodePNG(png)
  const out = Buffer.from(data)
  for (let y = 0; y < height; y++) {
    const k = height > 1 ? y / (height - 1) : 0
    const r = Math.round(topRGB[0] + (botRGB[0] - topRGB[0]) * k)
    const g = Math.round(topRGB[1] + (botRGB[1] - topRGB[1]) * k)
    const b = Math.round(topRGB[2] + (botRGB[2] - topRGB[2]) * k)
    for (let x = 0; x < width; x++) {
      const i = (y * width + x) * 4
      if (out[i + 3] > 0) {
        out[i] = r
        out[i + 1] = g
        out[i + 2] = b
      }
    }
  }
  return encodePNG(width, height, out)
}

// ---- 扁平蓝底白鲸鱼（与网站图标一致；浅/深色任务栏均可见，故 light/dark 相同） ----
// 用透明底鲸鱼轮廓（whale-src.png）合成到纯色圆角蓝底上；鲸鱼缩放至 80% 居中留边。
const whaleSrc = decodePNG(readFileSync(join(root, 'whale-src.png')))
const WSRC = whaleSrc.width, HSRC = whaleSrc.height
let wxmin = WSRC, wymin = HSRC, wxmax = -1, wymax = -1
for (let y = 0; y < HSRC; y++) for (let x = 0; x < WSRC; x++) if (whaleSrc.data[(y * WSRC + x) * 4 + 3] > 0) {
  if (x < wxmin) wxmin = x; if (x > wxmax) wxmax = x; if (y < wymin) wymin = y; if (y > wymax) wymax = y
}
if (wxmax < 0) { wxmin = 0; wymin = 0; wxmax = WSRC - 1; wymax = HSRC - 1 }
const ww = wxmax - wxmin + 1, wh = wymax - wymin + 1
function renderTray(size) {
  const W = size, H = size
  const R = Math.round(size * 0.21)
  const blue = [77, 107, 254] // #4d6bfe
  const fit = 0.80
  const s = Math.min((W * fit) / ww, (H * fit) / wh)
  const nw = ww * s, nh = wh * s
  const ox = (W - nw) / 2, oy = (H - nh) / 2
  const out = Buffer.alloc(W * H * 4)
  const inRound = (x, y) => {
    if (x < R && y < R) return (x - R) * (x - R) + (y - R) * (y - R) <= R * R
    if (x > W - R && y < R) return (x - (W - R)) * (x - (W - R)) + (y - R) * (y - R) <= R * R
    if (x < R && y > H - R) return (x - R) * (x - R) + (y - (H - R)) * (y - (H - R)) <= R * R
    if (x > W - R && y > H - R) return (x - (W - R)) * (x - (W - R)) + (y - (H - R)) * (y - (H - R)) <= R * R
    return true
  }
  for (let y = 0; y < H; y++) {
    for (let x = 0; x < W; x++) {
      const i = (y * W + x) * 4
      if (!inRound(x, y)) { out[i] = 0; out[i + 1] = 0; out[i + 2] = 0; out[i + 3] = 0; continue }
      let white = false
      if (x >= ox && x < ox + nw && y >= oy && y < oy + nh) {
        const sx = Math.floor(wxmin + (x - ox) / s)
        const sy = Math.floor(wymin + (y - oy) / s)
        if (sx >= 0 && sx < WSRC && sy >= 0 && sy < HSRC && whaleSrc.data[(sy * WSRC + sx) * 4 + 3] > 0) white = true
      }
      if (white) { out[i] = 255; out[i + 1] = 255; out[i + 2] = 255; out[i + 3] = 255 }
      else { out[i] = blue[0]; out[i + 1] = blue[1]; out[i + 2] = blue[2]; out[i + 3] = 255 }
    }
  }
  return encodePNG(W, H, out)
}
const icoLight = makeICO([16, 24, 32, 48, 64, 256].map((size) => ({ size, data: renderTray(size) })))
const icoDark = makeICO([16, 24, 32, 48, 64, 256].map((size) => ({ size, data: renderTray(size) })))

// ---- 写出 icon_gen.go（浅色 + 深色 + 模板） ----
const b64 = (buf) => buf.toString('base64').match(/.{1,96}/g).join('\n')
const go =
  '// Code generated by scripts/gen-icon.mjs; DO NOT EDIT.\n' +
  'package main\n' +
  '\n' +
  'import "encoding/base64"\n' +
  '\n' +
  '// iconData 浅色鲸鱼主图标（深色任务栏 / macOS 回退），多尺寸 ICO。\n' +
  'var iconData = mustDecodeIcon(iconDataB64)\n' +
  '\n' +
  '// iconDataDark 深色鲸鱼主图标（浅色任务栏，配合主题自适应切换）。\n' +
  'var iconDataDark = mustDecodeIcon(iconDataDarkB64)\n' +
  '\n' +
  '// iconDataTemplate macOS 菜单栏模板图标：纯黑鲸鱼、透明背景。\n' +
  'var iconDataTemplate = mustDecodeIcon(iconTemplateB64)\n' +
  '\n' +
  'const iconDataB64 = `' + b64(icoLight) + '`\n' +
  '\n' +
  'const iconDataDarkB64 = `' + b64(icoDark) + '`\n' +
  '\n' +
  'const iconTemplateB64 = `' + b64(template) + '`\n' +
  '\n' +
  'func mustDecodeIcon(b64 string) []byte {\n' +
  '    b, err := base64.StdEncoding.DecodeString(b64)\n' +
  '    if err != nil {\n' +
  '        panic("invalid embedded icon: " + err.Error())\n' +
  '    }\n' +
  '    return b\n' +
  '}\n'

const out = process.argv[2] ? resolve(process.argv[2]) : genPath
writeFileSync(out, go)

// 可执行图标/预览由 restyle-appicon.mjs 负责（app-icon.png / icon.ico）；此处仅写深色预览供肉眼检查
const darkBest = parseICO(icoDark).sort((a, b) => b.size - a.size)[0]
writeFileSync(join(root, 'preview-dark.png'), darkBest.data)

console.log('wrote', out, '| icoLight:', icoLight.length, '| icoDark:', icoDark.length, '| template:', template.length, '| go:', go.length)