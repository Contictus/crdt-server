/**
 * Golden vectors for the lib0 primitive codecs, produced by the real lib0.
 *
 * The Go implementation in internal/crdt/lib0 is checked against these bytes:
 * encode must produce `hex` exactly, decode must recover `value` exactly.
 */
import * as encoding from 'lib0/encoding'
import * as decoding from 'lib0/decoding'

const hex = (bytes) => Buffer.from(bytes).toString('hex')

const enc = (fn) => {
  const e = encoding.createEncoder()
  fn(e)
  return encoding.toUint8Array(e)
}

const MAX_SAFE = Number.MAX_SAFE_INTEGER // 2^53 - 1

const varUintValues = [
  0, 1, 63, 64, 127, 128, 129, 255, 256, 300,
  16383, 16384, 16385,
  2097151, 2097152,
  268435455, 268435456,
  4294967295, 4294967296,
  MAX_SAFE
]

const varIntValues = [
  0, 1, -1, 5, -5, 63, -63, 64, -64, 127, -127, 128, -128, 255, -255,
  8191, -8191, 8192, -8192,
  1048575, -1048575, 1048576, -1048576,
  2147483647, -2147483647,
  4294967296, -4294967296,
  MAX_SAFE, -MAX_SAFE
]

const varStringValues = [
  '',
  'a',
  'Hello',
  'text',
  'a'.repeat(127),
  'a'.repeat(128),
  'a'.repeat(300),
  'ünïcödé',
  'ÅÄÖ åäö',
  '日本語テキスト',
  '🎉', // surrogate pair: 1 code point, 2 UTF-16 units, 4 UTF-8 bytes
  'é', // combining accent
  'mixed 🎉 日本 ünï',
  'ünïcödé-'.repeat(40)
]

const varUint8ArrayValues = [
  [],
  [0],
  [255],
  [0, 1, 127, 128, 255],
  Array.from({ length: 127 }, (_, i) => i & 0xff),
  Array.from({ length: 128 }, (_, i) => (i * 7) & 0xff),
  Array.from({ length: 300 }, (_, i) => (i * 31) & 0xff)
]

export const buildLib0Vectors = () => ({
  notes: 'Golden vectors produced by lib0 itself. hex is the exact byte sequence lib0 writes.',
  varUint: varUintValues.map((value) => ({
    value,
    hex: hex(enc((e) => encoding.writeVarUint(e, value)))
  })),
  varInt: [
    ...varIntValues.map((value) => ({
      value,
      hex: hex(enc((e) => encoding.writeVarInt(e, value)))
    })),
    {
      // lib0 distinguishes -0 from 0 (math.isNegativeZero); Go's int64 cannot.
      // Decoders must accept this byte and yield 0.
      value: 0,
      hex: hex(enc((e) => encoding.writeVarInt(e, -0))),
      decodeOnly: true,
      note: 'negative zero: lib0 sets the sign bit, Go decodes it as 0'
    }
  ],
  varString: varStringValues.map((value) => ({
    value,
    utf16Length: value.length, // what Yjs uses as a struct length
    byteLength: Buffer.byteLength(value, 'utf8'),
    hex: hex(enc((e) => encoding.writeVarString(e, value)))
  })),
  varUint8Array: varUint8ArrayValues.map((bytes) => ({
    bytes,
    hex: hex(enc((e) => encoding.writeVarUint8Array(e, new Uint8Array(bytes))))
  })),
  // Byte sequences a decoder must reject rather than misread.
  invalid: [
    { kind: 'varUint', hex: '', reason: 'empty input' },
    { kind: 'varUint', hex: '80', reason: 'continuation bit set but input ends' },
    { kind: 'varUint', hex: 'ffffffffffffffff7f', reason: 'value exceeds 2^53-1' },
    { kind: 'varInt', hex: 'c0', reason: 'continuation bit set but input ends' },
    { kind: 'varInt', hex: 'ffffffffffffffffff7f', reason: 'value exceeds 2^53-1' },
    { kind: 'varString', hex: '05', reason: 'length 5 but no payload' },
    { kind: 'varString', hex: '05616263', reason: 'length 5 but only 3 bytes follow' },
    { kind: 'varUint8Array', hex: '0501020304', reason: 'length 5 but only 4 bytes follow' }
  ],
  // Input that lib0 does NOT reject: it reads past the end of its buffer, gets
  // `undefined`, and returns a bogus value. Go must return an error instead -
  // being stricter than lib0 here is deliberate, and only affects input Yjs
  // never produces.
  goStricter: [
    { kind: 'varInt', hex: '', reason: 'empty input: lib0 returns 0, Go must error' }
  ]
})

/** Round-trip every vector through lib0's own decoder as a sanity check. */
export const selfCheckLib0Vectors = (vectors) => {
  const bytesOf = (h) => new Uint8Array(Buffer.from(h, 'hex'))
  for (const v of vectors.varUint) {
    const got = decoding.readVarUint(decoding.createDecoder(bytesOf(v.hex)))
    if (got !== v.value) throw new Error(`varUint self-check failed for ${v.value}: got ${got}`)
  }
  for (const v of vectors.varInt) {
    const got = decoding.readVarInt(decoding.createDecoder(bytesOf(v.hex)))
    if (got !== v.value && !(v.decodeOnly && got === 0)) {
      throw new Error(`varInt self-check failed for ${v.value}: got ${got}`)
    }
  }
  for (const v of vectors.varString) {
    const got = decoding.readVarString(decoding.createDecoder(bytesOf(v.hex)))
    if (got !== v.value) throw new Error(`varString self-check failed for ${JSON.stringify(v.value)}`)
  }
  for (const v of vectors.varUint8Array) {
    const got = decoding.readVarUint8Array(decoding.createDecoder(bytesOf(v.hex)))
    if (hex(got) !== hex(new Uint8Array(v.bytes))) throw new Error('varUint8Array self-check failed')
  }
  // Every "invalid" vector must actually be rejected by lib0, otherwise the Go
  // tests would be asserting a stricter contract than Yjs implements.
  const readers = {
    varUint: decoding.readVarUint,
    varInt: decoding.readVarInt,
    varString: decoding.readVarString,
    varUint8Array: decoding.readVarUint8Array
  }
  const throwsInLib0 = (v) => {
    try {
      readers[v.kind](decoding.createDecoder(bytesOf(v.hex)))
      return false
    } catch {
      return true
    }
  }
  for (const v of vectors.invalid) {
    if (!throwsInLib0(v)) throw new Error(`invalid vector ${v.kind}/${v.hex} did not throw in lib0`)
  }
  for (const v of vectors.goStricter) {
    if (throwsInLib0(v)) {
      throw new Error(`goStricter vector ${v.kind}/${v.hex} now throws in lib0; move it to invalid`)
    }
  }
}
