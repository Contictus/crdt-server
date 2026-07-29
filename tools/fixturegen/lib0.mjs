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

/**
 * `any` vectors are named rather than self-describing: the Go test holds a
 * table of name -> expected Go value, so the expectation stays strongly typed
 * and a new vector here fails the Go test until it is given an expectation.
 */
const anyValues = [
  ['undefined', undefined],
  ['null', null],
  ['true', true],
  ['false', false],
  ['int-0', 0],
  ['int-1', 1],
  ['int-neg-1', -1],
  ['int-127', 127],
  ['int-128', 128],
  ['int-max31', 2147483647], // BITS31: still an integer
  ['int-neg-max31', -2147483647],
  // 2^31 is one past the integer range, but is exactly representable as a
  // float32, so lib0 emits tag 124 - not 123 and not 125.
  ['num-2pow31', 2147483648],
  ['float32-1.5', 1.5],
  ['float32-neg-0.5', -0.5],
  ['float64-0.1', 0.1], // 0.1 does not survive a float32 round trip
  ['float64-1e300', 1e300],
  // isFloat32(NaN) compares NaN === NaN, which is false, so NaN takes the
  // float64 branch while Infinity takes the float32 one.
  ['nan', NaN],
  ['infinity', Infinity],
  ['neg-infinity', -Infinity],
  ['negative-zero', -0], // integer branch, writeVarInt(-0) sets the sign bit
  ['bigint-0', 0n],
  ['bigint-max-safe-plus', 9007199254740993n],
  ['bigint-neg', -9007199254740993n],
  ['string-empty', ''],
  ['string-emoji', 'hi 🎉'],
  ['bytes-empty', new Uint8Array([])],
  ['bytes', new Uint8Array([0, 1, 255, 128])],
  ['array-empty', []],
  ['array-mixed', [1, 'two', null, true, [3.5]]],
  ['object-empty', {}],
  // Insertion order is what lib0 writes; a Go encoder that sorts keys would
  // produce different bytes for this vector.
  ['object-nested', { zeta: 1, alpha: { b: [1, 2] }, m: 'x' }]
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
  any: anyValues.map(([name, value]) => ({
    name,
    tag: enc((e) => encoding.writeAny(e, value))[0],
    hex: hex(enc((e) => encoding.writeAny(e, value)))
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
    { kind: 'varUint8Array', hex: '0501020304', reason: 'length 5 but only 4 bytes follow' },
    { kind: 'any', hex: '00', reason: 'tag 0 is not an any type' },
    { kind: 'any', hex: '77', reason: 'string tag with no payload' },
    { kind: 'any', hex: '75', reason: 'array tag with no length' }
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
  // `any` vectors round-trip against the original JS value, so a wrong tag or a
  // wrong endianness shows up here before the Go side ever sees the bytes.
  const sameAny = (a, b) => {
    if (typeof a === 'number' && typeof b === 'number' && isNaN(a) && isNaN(b)) return true
    if (a instanceof Uint8Array || b instanceof Uint8Array) return hex(a) === hex(b)
    if (typeof a === 'bigint' || typeof b === 'bigint') return a === b
    if (a === null || b === null || typeof a !== 'object') return Object.is(a, b) || a === b
    return JSON.stringify(a) === JSON.stringify(b)
  }
  for (const [i, v] of vectors.any.entries()) {
    const got = decoding.readAny(decoding.createDecoder(bytesOf(v.hex)))
    if (!sameAny(anyValues[i][1], got)) {
      throw new Error(`any self-check failed for ${v.name}: got ${String(got)}`)
    }
  }
  // Every "invalid" vector must actually be rejected by lib0, otherwise the Go
  // tests would be asserting a stricter contract than Yjs implements.
  const readers = {
    varUint: decoding.readVarUint,
    varInt: decoding.readVarInt,
    varString: decoding.readVarString,
    varUint8Array: decoding.readVarUint8Array,
    any: decoding.readAny
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
