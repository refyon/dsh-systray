// scripts/restyle-appicon.mjs — APP 图标重制为「品牌蓝圆角底 + 白色鲸鱼」
// 输入：whale-src.png（透明底灰色鲸鱼剪影）；输出：app-icon.png、icon.ico、preview-new.png
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import { deflateSync, inflateSync } from 'node:zlib'

const __dirname = dirname(fileURLToPath(import.meta.url))
const root = join(__dirname, '..')

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
  ihdr[9] = 6
  const sig = Buffer.from([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A])
  return Buffer.concat([sig, chunk('IHDR', ihdr), chunk('IDAT', deflateSync(raw, { level: 9 })), chunk('IEND', Buffer.alloc(0))])
}
function decodePNG(buf) {
  if (buf.length < 8 || buf.readUInt32BE(0) !== 0x89504E47) throw new Error('not a png')
  let width = 0, height = 0, bit = 0, color = 0
  const idat = []
  let off = 8
  while (off + 8 <= buf.length) {
    const len = buf.readUInt32BE(off)
    const type = buf.toString('ascii', off + 4, off + 8)
    const data = buf.subarray(off + 8, off + 8 + len)
    if (type === 'IHDR') { width = data.readUInt32BE(0); height = data.readUInt32BE(4); bit = data[8]; color = data[9] }
    else if (type === 'IDAT') { idat.push(data) }
    else if (type === 'IEND') { break }
    off += 12 + len
  }
  if (bit !== 8 || color !== 6) throw new Error('unsupported png bit=' + bit + ' color=' + color)
  const raw = inflateSync(Buffer.concat(idat))
  const stride = width * 4
  const out = Buffer.alloc(width * height * 4)
  const paeth = (a, b, c) => {
    const p = a + b - c
    const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c)
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
function makeICO(pngs) {
  const header = Buffer.alloc(6)
  header.writeUInt16LE(0, 0); header.writeUInt16LE(1, 2); header.writeUInt16LE(pngs.length, 4)
  let offset = 6 + 16 * pngs.length
  const entries = []
  for (const { size, data } of pngs) {
    const e = Buffer.alloc(16)
    e[0] = size >= 256 ? 0 : size; e[1] = size >= 256 ? 0 : size; e[2] = 0; e[3] = 0
    e.writeUInt16LE(1, 4); e.writeUInt16LE(32, 6); e.writeUInt32LE(data.length, 8); e.writeUInt32LE(offset, 12)
    entries.push(e); offset += data.length
  }
  return Buffer.concat([header, ...entries, ...pngs.map((p) => p.data)])
}

function restyle(src) {
  const whale = decodePNG(src)
  const srcW = whale.width, srcH = whale.height
  const SS = 4 // 超采样倍数：先渲染 4 倍，再盒式下采样回源分辨率，抗锯齿平滑边缘
  const W = srcW * SS, H = srcH * SS
  const R = Math.round(W * 0.21) // 圆角（macOS app icon 风格）
  // 鲸鱼轮廓 bbox（原图为透明底灰鲸鱼剪影）
  let xmin = srcW, ymin = srcH, xmax = -1, ymax = -1
  for (let y = 0; y < srcH; y++) {
    for (let x = 0; x < srcW; x++) {
      if (whale.data[(y * srcW + x) * 4 + 3] > 0) {
        if (x < xmin) xmin = x; if (x > xmax) xmax = x
        if (y < ymin) ymin = y; if (y > ymax) ymax = y
      }
    }
  }
  if (xmax < 0) { xmin = 0; ymin = 0; xmax = srcW - 1; ymax = srcH - 1 }
  const whaleW = xmax - xmin + 1, whaleH = ymax - ymin + 1
  const fit = 0.80 // 鲸鱼占图标宽/高 80%，四周留均匀边距，避免拥挤
  const s = Math.min((W * fit) / whaleW, (H * fit) / whaleH)
  const nw = whaleW * s, nh = whaleH * s
  const ox = (W - nw) / 2, oy = (H - nh) / 2

  const out = Buffer.alloc(W * H * 4)
  const blue = [29, 78, 216] // #1d4ed8 品牌蓝（云隙蓝主题）
  // 圆角矩形有符号距离 → 边界 1px 渐变覆盖度（消除圆角硬边锯齿）
  const roundCover = (x, y) => {
    const cx = x + 0.5, cy = y + 0.5
    const half = W / 2
    const qx = Math.abs(cx - half) - (half - R)
    const qy = Math.abs(cy - half) - (half - R)
    const dx = Math.max(qx, 0), dy = Math.max(qy, 0)
    const dist = Math.hypot(dx, dy) - R
    const a = 0.5 - dist
    return a < 0 ? 0 : a > 1 ? 1 : a
  }
  // 鲸鱼双线性 alpha（浮点源坐标）
  const whaleAlpha = (fx, fy) => {
    if (fx < 0 || fy < 0 || fx > srcW - 1 || fy > srcH - 1) return 0
    const x0 = Math.floor(fx), y0 = Math.floor(fy)
    const x1 = Math.min(x0 + 1, srcW - 1), y1 = Math.min(y0 + 1, srcH - 1)
    const tx = fx - x0, ty = fy - y0
    const a00 = whale.data[(y0 * srcW + x0) * 4 + 3] / 255
    const a10 = whale.data[(y0 * srcW + x1) * 4 + 3] / 255
    const a01 = whale.data[(y1 * srcW + x0) * 4 + 3] / 255
    const a11 = whale.data[(y1 * srcW + x1) * 4 + 3] / 255
    return (a00 * (1 - tx) + a10 * tx) * (1 - ty) + (a01 * (1 - tx) + a11 * tx) * ty
  }
  for (let y = 0; y < H; y++) {
    for (let x = 0; x < W; x++) {
      const i = (y * W + x) * 4
      const ra = roundCover(x, y)
      if (ra <= 0) continue
      // 鲸鱼覆盖度（双线性），蓝底与白鲸鱼按覆盖度混合 → 鲸鱼边缘平滑过渡
      let wa = 0
      const fx = xmin + (x + 0.5 - ox) / s - 0.5
      const fy = ymin + (y + 0.5 - oy) / s - 0.5
      if (fx >= 0 && fy >= 0 && fx < srcW && fy < srcH) wa = whaleAlpha(fx, fy)
      out[i] = Math.round(blue[0] + (255 - blue[0]) * wa)
      out[i + 1] = Math.round(blue[1] + (255 - blue[1]) * wa)
      out[i + 2] = Math.round(blue[2] + (255 - blue[2]) * wa)
      out[i + 3] = Math.round(ra * 255)
    }
  }
  // 盒式下采样回源分辨率（alpha 加权，透明像素不污染边缘色），边缘平滑抗锯齿
  const small = downsample({ width: W, height: H, data: out }, SS)
  return encodePNG(small.width, small.height, small.data)
}
function downsample(img, factor) {
  const w = Math.floor(img.width / factor), h = Math.floor(img.height / factor)
  const out = Buffer.alloc(w * h * 4)
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      let r = 0, g = 0, b = 0, a = 0
      for (let dy = 0; dy < factor; dy++) for (let dx = 0; dx < factor; dx++) {
        const i = ((y * factor + dy) * img.width + (x * factor + dx)) * 4
        const pa = img.data[i + 3]
        a += pa
        r += img.data[i] * pa; g += img.data[i + 1] * pa; b += img.data[i + 2] * pa
      }
      const n = factor * factor; const o = (y * w + x) * 4
      out[o + 3] = Math.round(a / n)
      if (a > 0) {
        out[o] = Math.round(r / a); out[o + 1] = Math.round(g / a); out[o + 2] = Math.round(b / a)
      }
    }
  }
  return { width: w, height: h, data: out }
}

const app = readFileSync(join(root, 'whale-src.png'))
const styled = restyle(app)
writeFileSync(join(root, 'app-icon.png'), styled)

const dec = decodePNG(styled)
const small = downsample(dec, 4)
const ico = makeICO([{ size: 256, data: encodePNG(small.width, small.height, small.data) }])
writeFileSync(join(root, 'icon.ico'), ico)
writeFileSync(join(root, 'preview-new.png'), styled)

console.log('done | app-icon:', styled.length, '| icon.ico:', ico.length, '| preview-new:', styled.length)
