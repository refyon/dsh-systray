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
const icoLight = Buffer.from(extractConst('iconDataB64'), 'base64')
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

// ---- 深色鲸鱼 ICO：浅色任务栏使用（同款深灰渐变 #4A4A4A → #2B2B2B） ----
const icoDark = makeICO(parseICO(icoLight).map(({ size, data }) => ({
  size,
  data: recolorPNG(data, [0x4A, 0x4A, 0x4A], [0x2B, 0x2B, 0x2B]),
})))

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

// 可执行文件图标资源与预览图（浅色主图标保持不变；深色预览便于肉眼检查）
writeFileSync(join(root, 'icon.ico'), icoLight)
const darkBest = parseICO(icoDark).sort((a, b) => b.size - a.size)[0]
writeFileSync(join(root, 'preview-dark.png'), darkBest.data)

console.log('wrote', out, '| icoLight:', icoLight.length, '| icoDark:', icoDark.length, '| template:', template.length, '| go:', go.length)