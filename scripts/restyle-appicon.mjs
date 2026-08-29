// scripts/restyle-appicon.mjs — APP 图标重制为「品牌蓝渐变圆角底 + 白色鲸鱼」
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
  const { width, height, data } = decodePNG(src)
  const out = Buffer.alloc(width * height * 4)
  const W = width, H = height
  const R = Math.round(W * 0.21) // 圆角（macOS app icon 风格）
  const bgTop = [91, 107, 255], bgBot = [47, 67, 214] // #5b6bff -> #2f43d6
  const inRound = (x, y) => {
    if (x < 0 || x >= W || y < 0 || y >= H) return false
    if (x >= R && x <= W - R) return true
    if (y >= R && y <= H - R) return true
    const cx = x < R ? R : W - R
    const cy = y < R ? R : H - R
    const dx = x - cx, dy = y - cy
    return dx * dx + dy * dy <= R * R
  }
  for (let y = 0; y < H; y++) {
    const k = H > 1 ? y / (H - 1) : 0
    const br = bgTop[0] + (bgBot[0] - bgTop[0]) * k
    const bg = bgTop[1] + (bgBot[1] - bgTop[1]) * k
    const bb = bgTop[2] + (bgBot[2] - bgTop[2]) * k
    for (let x = 0; x < W; x++) {
      const i = (y * W + x) * 4
      if (!inRound(x, y)) { out[i] = 0; out[i + 1] = 0; out[i + 2] = 0; out[i + 3] = 0; continue }
      const whale = data[i + 3] > 0
      if (whale) { out[i] = 255; out[i + 1] = 255; out[i + 2] = 255; out[i + 3] = 255 }
      else { out[i] = Math.round(br); out[i + 1] = Math.round(bg); out[i + 2] = Math.round(bb); out[i + 3] = 255 }
    }
  }
  return encodePNG(W, H, out)
}
function downsample(img, factor) {
  const w = Math.floor(img.width / factor), h = Math.floor(img.height / factor)
  const out = Buffer.alloc(w * h * 4)
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      let r = 0, g = 0, b = 0, a = 0
      for (let dy = 0; dy < factor; dy++) for (let dx = 0; dx < factor; dx++) {
        const i = ((y * factor + dy) * img.width + (x * factor + dx)) * 4
        r += img.data[i]; g += img.data[i + 1]; b += img.data[i + 2]; a += img.data[i + 3]
      }
      const n = factor * factor; const o = (y * w + x) * 4
      out[o] = Math.round(r / n); out[o + 1] = Math.round(g / n); out[o + 2] = Math.round(b / n); out[o + 3] = Math.round(a / n)
    }
  }
  return { width: w, height: h, data: out }
}

const app = readFileSync(join(root, 'app-icon.png'))
const styled = restyle(app)
writeFileSync(join(root, 'app-icon.png'), styled)

const dec = decodePNG(styled)
const small = downsample(dec, 4)
const ico = makeICO([{ size: 256, data: encodePNG(small.width, small.height, small.data) }])
writeFileSync(join(root, 'icon.ico'), ico)
writeFileSync(join(root, 'preview-new.png'), styled)

console.log('done | app-icon:', styled.length, '| icon.ico:', ico.length, '| preview-new:', styled.length)
