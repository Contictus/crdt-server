package lib0

// Encoder appends lib0-encoded values to a growable buffer.
//
// Write methods do not return errors; the first failure is recorded and every
// later write becomes a no-op, like bufio.Writer. Check Err once before using
// Bytes.
type Encoder struct {
	buf []byte
	err error
}

// NewEncoder returns an Encoder with no buffered data.
func NewEncoder() *Encoder { return &Encoder{} }

// NewEncoderSize returns an Encoder whose buffer has room for n bytes.
func NewEncoderSize(n int) *Encoder { return &Encoder{buf: make([]byte, 0, n)} }

// Bytes returns the encoded bytes. The slice is valid until the next write.
func (e *Encoder) Bytes() []byte { return e.buf }

// Len returns the number of bytes written so far.
func (e *Encoder) Len() int { return len(e.buf) }

// Err returns the first error encountered while writing, if any.
func (e *Encoder) Err() error { return e.err }

// Reset discards the buffered bytes and the recorded error, keeping capacity.
func (e *Encoder) Reset() {
	e.buf = e.buf[:0]
	e.err = nil
}

func (e *Encoder) fail(err error) {
	if e.err == nil {
		e.err = err
	}
}

// WriteUint8 writes one raw byte. Used for struct info bytes, which are not
// varint encoded (yjs/src/utils/UpdateEncoder.js writeInfo).
func (e *Encoder) WriteUint8(b byte) {
	if e.err != nil {
		return
	}
	e.buf = append(e.buf, b)
}

// WriteBytes appends raw bytes without a length prefix.
func (e *Encoder) WriteBytes(b []byte) {
	if e.err != nil {
		return
	}
	e.buf = append(e.buf, b...)
}

// WriteVarUint writes an unsigned variable length integer: seven bits per byte,
// least significant group first, 0x80 marking continuation.
//
// Mirrors lib0/encoding.js writeVarUint.
func (e *Encoder) WriteVarUint(v uint64) {
	if e.err != nil {
		return
	}
	if v > MaxSafeInteger {
		e.fail(ErrIntegerOutOfRange)
		return
	}
	for v > 0x7F {
		e.buf = append(e.buf, byte(0x80|(v&0x7F)))
		v >>= 7
	}
	e.buf = append(e.buf, byte(v))
}

// WriteVarInt writes a signed variable length integer. The first byte holds six
// value bits and the sign at 0x40; following bytes hold seven value bits each.
// The magnitude is written unsigned - this is neither zigzag nor two's
// complement.
//
// Mirrors lib0/encoding.js writeVarInt. Note that lib0 can encode negative
// zero (0x40) and Go cannot; the decoder accepts it and yields 0.
func (e *Encoder) WriteVarInt(v int64) {
	if e.err != nil {
		return
	}
	negative := v < 0
	// Guard before negating so math.MinInt64 cannot overflow.
	if v < -MaxSafeInteger || v > MaxSafeInteger {
		e.fail(ErrIntegerOutOfRange)
		return
	}
	num := v
	if negative {
		num = -num
	}
	first := byte(num & 0x3F)
	if num > 0x3F {
		first |= 0x80
	}
	if negative {
		first |= 0x40
	}
	e.buf = append(e.buf, first)
	num >>= 6
	for num > 0 {
		b := byte(num & 0x7F)
		if num > 0x7F {
			b |= 0x80
		}
		e.buf = append(e.buf, b)
		num >>= 7
	}
}

// WriteVarString writes the UTF-8 bytes of s prefixed by their length in bytes
// (not code points, not UTF-16 units).
//
// Mirrors lib0/encoding.js writeVarString.
func (e *Encoder) WriteVarString(s string) {
	if e.err != nil {
		return
	}
	e.WriteVarUint(uint64(len(s)))
	e.WriteBytes([]byte(s))
}

// WriteVarUint8Array writes b prefixed by its length.
//
// Mirrors lib0/encoding.js writeVarUint8Array.
func (e *Encoder) WriteVarUint8Array(b []byte) {
	if e.err != nil {
		return
	}
	e.WriteVarUint(uint64(len(b)))
	e.WriteBytes(b)
}
