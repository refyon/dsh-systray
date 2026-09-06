// scripts/icon-metrics.mjs — 分析图标几何：测量内容 bbox 与鲸鱼占比（纯 Node 零依赖）
// 用法：node scripts/icon-metrics.mjs <png路径>...
import { readFileSync } from 'node:fs'
import { deflateSync, inflateSync } from 'node:zlib'

const CRC_TABLE = (() => { const t = new Int32Array(256); for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xEDB88320 ^ (c >>> 1) : c >>> 1; t[n] = c } return t })()
function crc32(buf) { let c = 0xFFFFFFFF; for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xFF] ^ (c >>> 8); return (c ^ 0xFFFFFFFF) >>> 0 }
function chunk(type, data) { const len = Buffer.alloc(4); len.writeUInt32BE(data.length); const t = Buffer.from(type, 'ascii'); const crc = Buffer.alloc(4); crc.writeUInt32BE(crc32(Buffer.concat([t, data]))); return Buffer.concat([len, t, data, crc]) }
function encodePNG(width, height, rgba) { const stride = width * 4; const raw = Buffer.alloc((stride + 1) * height); for (let y = 0; y < height; y++) { raw[y * (stride + 1)] = 0; rgba.copy(raw, y * (stride + 1) + 1, y * stride, (y + 1) * stride) } const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(width, 0); ihdr.writeUInt32BE(height, 4); ihdr[8] = 8; ihdr[9] = 6; const sig = Buffer.from([0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A]); return Buffer.concat([sig, chunk('IHDR', ihdr), chunk('IDAT', deflateSync(raw, { level: 9 })), chunk('IEND', Buffer.alloc(0))]) }
function decodePNG(buf) {
  if (buf.length < 8 || buf.readUInt32BE(0) !== 0x89504E47) throw new Error('not a png')
  let width = 0, height = 0, bit = 0, color = 0; const idat = []; let off = 8
  while (off + 8 <= buf.length) { const len = buf.readUInt32BE(off); const type = buf.toString('ascii', off + 4, off + 8); const data = buf.subarray(off + 8, off + 8 + len); if (type === 'IHDR') { width = data.readUInt32BE(0); height = data.readUInt32BE(4); bit = data[8]; color = data[9] } else if (type === 'IDAT') { idat.push(data) } else if (type === 'IEND') break; off += 12 + len }
  if (bit !== 8 || color !== 6) throw new Error('unsupported png bit=' + bit + ' color=' + color)
  const raw = inflateSync(Buffer.concat(idat)); const stride = width * 4; const out = Buffer.alloc(width * height * 4)
  const paeth = (a, b, c) => { const p = a + b - c; const pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c); return pa <= pb && pa <= pc ? a : pb <= pc ? b : c }
  for (let y = 0; y < height; y++) { const f = raw[y * (stride + 1)]; const line = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1)); const prev = y > 0 ? out.subarray((y - 1) * stride, y * stride) : null; for (let x = 0; x < stride; x++) { const a = x >= 4 ? line[x - 4] : 0; const b = prev ? prev[x] : 0; const c = x >= 4 && prev ? prev[x - 4] : 0; let v = line[x]; if (f === 1) v = (v + a) & 0xFF; else if (f === 2) v = (v + b) & 0xFF; else if (f === 3) v = (v + ((a + b) >> 1)) & 0xFF; else if (f === 4) v = (v + paeth(a, b, c)) & 0xFF; out[y * stride + x] = v } }
  return { width, height, data: out }
}

for (const p of process.argv.slice(2)) {
  const { width: W, height: H, data } = decodePNG(readFileSync(p))
  let opMinX = W, opMinY = H, opMaxX = -1, opMaxY = -1
  let blMinX = W, blMinY = H, blMaxX = -1, blMaxY = -1 // 蓝底（b 显著高于 r,g）
  let whMinX = W, whMinY = H, whMaxX = -1, whMaxY = -1 // 白/亮鲸鱼
  let gMinX = W, gMinY = H, gMaxX = -1, gMaxY = -1     // 任意非透明内容
  let darkCnt = 0, blueCnt = 0, whiteCnt = 0, opq = 0
  for (let y = 0; y < H; y++) for (let x = 0; x < W; x++) {
    const i = (y * W + x) * 4, a = data[i + 3]
    if (a === 0) continue
    const r = data[i], g = data[i + 1], b = data[i + 2]
    opq++
    if (x < gMinX) gMinX = x; if (x > gMaxX) gMaxX = x; if (y < gMinY) gMinY = y; if (y > gMaxY) gMaxY = y
    if (b - Math.max(r, g) > 26 && b > 90) { blueCnt++; if (x < blMinX) blMinX = x; if (x > blMaxX) blMaxX = x; if (y < blMinY) blMinY = y; if (y > blMaxY) blMaxY = y }
    else if (Math.min(r, g, b) > 175) { whiteCnt++; if (x < whMinX) whMinX = x; if (x > whMaxX) whMaxX = x; if (y < whMinY) whMinY = y; if (y > whMaxY) whMaxY = y }
    else darkCnt++
  }
  const f = (n) => (n < 0 ? 'n/a' : n)
  console.log('\n==', p, `(${W}x${H}, opaque=${opq})`)
  console.log('  non-transparent bbox: x[' + gMinX + ',' + gMaxX + '] y[' + gMinY + ',' + gMaxY + ']  -> inset L/R/T/B:', gMinX, W - 1 - gMaxX, gMinY, H - 1 - gMaxY)
  if (whiteCnt > 0) console.log('  whale(white) bbox: x[' + whMinX + ',' + whMaxX + '] y[' + whMinY + ',' + whMaxY + ']  px=' + whiteCnt + '  widthFrac(canvas)=' + ((whMaxX - whMinX + 1) / W).toFixed(3) + '  heightFrac(canvas)=' + ((whMaxY - whMinY + 1) / H).toFixed(3))
  if (blueCnt > 0) console.log('  blue-bg  bbox: x[' + f(blMinX) + ',' + f(blMaxX) + '] y[' + f(blMinY) + ',' + f(blMaxY) + ']  px=' + blueCnt + '  widthFrac=' + (blMaxX - blMinX + 1) / W, 'heightFrac=' + (blMaxY - blMinY + 1) / H)
  if (blueCnt > 0 && whiteCnt > 0) console.log('  whale/bluebox size ratio: w=' + ((whMaxX - whMinX + 1) / (blMaxX - blMinX + 1)).toFixed(3), 'h=' + ((whMaxY - whMinY + 1) / (blMaxY - blMinY + 1)).toFixed(3))
  if (darkCnt > 0) console.log('  dark glyph px=' + darkCnt)
}
