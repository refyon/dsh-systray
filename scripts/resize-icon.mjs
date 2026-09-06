// scripts/resize-icon.mjs — 以画布中心为基准缩放图标内容（有内容像素等比缩放，透明区保持不变，
// 四周留白随之增减）。预乘 alpha + 双线性 4x4 子采样面积平均，边缘抗锯齿、无渗色。
// 用法：node scripts/resize-icon.mjs <png> <scaleFactor>
//   scaleFactor < 1 → 内容缩小、四周留白增大；> 1 → 内容放大。
import { readFileSync, writeFileSync } from 'node:fs'
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

const file = process.argv[2]
const factor = parseFloat(process.argv[3])
if (!file || !(factor > 0)) { console.error('usage: node resize-icon.mjs <png> <scaleFactor>'); process.exit(1) }
const { width: W, height: H, data: src } = decodePNG(readFileSync(file))
if (W !== H) { console.error('canvas must be square'); process.exit(1) }
const c = (W - 1) / 2
const inv = 1 / factor
const out = Buffer.alloc(W * H * 4)
const SS = 4
const n = SS * SS
for (let y = 0; y < H; y++) {
  const sy = c + (y - c) * inv
  for (let x = 0; x < W; x++) {
    const sx = c + (x - c) * inv
    let rSum = 0, gSum = 0, bSum = 0, aSum = 0
    for (let j = 0; j < SS; j++) for (let i = 0; i < SS; i++) {
      const fx = sx + ((i + 0.5) / SS - 0.5) * inv
      const fy = sy + ((j + 0.5) / SS - 0.5) * inv
      if (fx < 0 || fx > W - 1 || fy < 0 || fy > H - 1) continue
      const x0 = Math.floor(fx), y0 = Math.floor(fy)
      const x1 = Math.min(x0 + 1, W - 1), y1 = Math.min(y0 + 1, H - 1)
      const tx = fx - x0, ty = fy - y0
      const w00 = (1 - tx) * (1 - ty), w01 = tx * (1 - ty), w10 = (1 - tx) * ty, w11 = tx * ty
      const p00 = (y0 * W + x0) * 4, p01 = (y0 * W + x1) * 4, p10 = (y1 * W + x0) * 4, p11 = (y1 * W + x1) * 4
      // 预乘 alpha 双线性（每分量 ×alpha 后插值），避免透明区残留 RGB 渗色
      rSum += (src[p00] * src[p00 + 3] * w00 + src[p01] * src[p01 + 3] * w01 + src[p10] * src[p10 + 3] * w10 + src[p11] * src[p11 + 3] * w11)
      gSum += (src[p00 + 1] * src[p00 + 3] * w00 + src[p01 + 1] * src[p01 + 3] * w01 + src[p10 + 1] * src[p10 + 3] * w10 + src[p11 + 1] * src[p11 + 3] * w11)
      bSum += (src[p00 + 2] * src[p00 + 3] * w00 + src[p01 + 2] * src[p01 + 3] * w01 + src[p10 + 2] * src[p10 + 3] * w10 + src[p11 + 2] * src[p11 + 3] * w11)
      aSum += (src[p00 + 3] * w00 + src[p01 + 3] * w01 + src[p10 + 3] * w10 + src[p11 + 3] * w11)
    }
    const o = (y * W + x) * 4
    const a = Math.min(255, Math.round(aSum / n))
    out[o + 3] = a
    if (a > 0) {
      out[o] = Math.min(255, Math.round(rSum / aSum))
      out[o + 1] = Math.min(255, Math.round(gSum / aSum))
      out[o + 2] = Math.min(255, Math.round(bSum / aSum))
    }
  }
}
writeFileSync(file, encodePNG(W, H, out))
console.log('rescaled', file, 'content by', factor)
