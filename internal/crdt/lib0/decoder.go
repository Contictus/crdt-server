package lib0

// Decoder reads lib0-encoded values from a byte slice.
//
// The Decoder does not copy: ReadVarUint8Array and ReadBytes return sub-slices
// of the input. Callers that keep the result beyond the lifetime of the input
// buffer must copy it.
type Decoder struct {
	buf []byte
	pos int
}

// NewDecoder returns a Decoder reading from buf.
func NewDecoder(buf []byte) *Decoder { return &Decoder{buf: buf} }

// Pos returns the read offset.
func (d *Decoder) Pos() int { return d.pos }

// Remaining returns the number of unread bytes.
func (d *Decoder) Remaining() int { return len(d.buf) - d.pos }

// Done reports whether the whole input has been consumed.
func (d *Decoder) Done() bool { return d.pos >= len(d.buf) }

// ReadUint8 reads one raw byte.
func (d *Decoder) ReadUint8() (byte, error) {
	if d.pos >= len(d.buf) {
		return 0, ErrUnexpectedEOF
	}
	b := d.buf[d.pos]
	d.pos++
	return b, nil
}

// ReadBytes returns the next n bytes as a sub-slice of the input.
func (d *Decoder) ReadBytes(n int) ([]byte, error) {
	if n < 0 {
		return nil, ErrNegativeLength
	}
	if d.Remaining() < n {
		return nil, ErrUnexpectedEOF
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

// ReadVarUint reads an unsigned variable length integer.
//
// Mirrors lib0/decoding.js readVarUint, including where it checks for overflow:
// a value is returned as soon as a byte without the continuation bit arrives,
// and the range check only applies when more bytes follow. That is why
// MaxSafeInteger itself decodes fine while a longer sequence does not.
func (d *Decoder) ReadVarUint() (uint64, error) {
	var num, mult uint64 = 0, 1
	for d.pos < len(d.buf) {
		r := d.buf[d.pos]
		d.pos++
		num += uint64(r&0x7F) * mult
		mult *= 128
		if r < 0x80 {
			return num, nil
		}
		if num > MaxSafeInteger {
			return 0, ErrIntegerOutOfRange
		}
	}
	return 0, ErrUnexpectedEOF
}

// ReadVarInt reads a signed variable length integer: six value bits and a sign
// bit at 0x40 in the first byte, seven value bits in every following byte.
//
// Mirrors lib0/decoding.js readVarInt. lib0's negative zero (the single byte
// 0x40) decodes to 0.
func (d *Decoder) ReadVarInt() (int64, error) {
	if d.pos >= len(d.buf) {
		return 0, ErrUnexpectedEOF
	}
	r := d.buf[d.pos]
	d.pos++
	num := int64(r & 0x3F)
	sign := int64(1)
	if r&0x40 != 0 {
		sign = -1
	}
	if r&0x80 == 0 {
		return sign * num, nil
	}
	mult := int64(64)
	for d.pos < len(d.buf) {
		r = d.buf[d.pos]
		d.pos++
		num += int64(r&0x7F) * mult
		mult *= 128
		if r < 0x80 {
			return sign * num, nil
		}
		if num > MaxSafeInteger {
			return 0, ErrIntegerOutOfRange
		}
	}
	return 0, ErrUnexpectedEOF
}

// ReadVarString reads a length-prefixed UTF-8 string.
//
// Mirrors lib0/decoding.js readVarString. Invalid UTF-8 is passed through
// unchanged, where the browser's TextDecoder would substitute U+FFFD; that only
// differs for input Yjs itself never produces.
func (d *Decoder) ReadVarString() (string, error) {
	b, err := d.ReadVarUint8Array()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadVarUint8Array reads a length-prefixed byte slice. The result aliases the
// decoder's input.
func (d *Decoder) ReadVarUint8Array() ([]byte, error) {
	n, err := d.ReadVarUint()
	if err != nil {
		return nil, err
	}
	if n > uint64(d.Remaining()) {
		return nil, ErrUnexpectedEOF
	}
	return d.ReadBytes(int(n))
}
