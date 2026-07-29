// Package lib0 implements the subset of the lib0 binary encoding that the Yjs
// wire protocol is built from: variable length integers, strings and byte
// arrays.
//
// The encoding is derived from the lib0 source, not from a specification:
// lib0/encoding.js and lib0/decoding.js as shipped with lib0@0.2.117. See
// DECISIONS.md §2.1 for the derivation, and testdata/fixtures/lib0/vectors.json
// for golden vectors produced by lib0 itself.
//
// Two details are easy to get wrong and are therefore spelled out here:
//
//   - varInt is not zigzag encoded. The first byte carries six value bits plus a
//     sign bit at 0x40; every following byte carries seven value bits. The
//     continuation bit is 0x80 throughout.
//   - lib0 decodes with floating point arithmetic, so values are limited to
//     2^53-1. Emitting anything larger produces bytes no browser can read, so
//     this package refuses to.
//
// This package uses the standard library only.
package lib0

import "errors"

// MaxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER. lib0 accumulates
// varints in a float64, so it cannot represent anything larger.
const MaxSafeInteger = 1<<53 - 1

var (
	// ErrUnexpectedEOF means the input ended in the middle of a value.
	ErrUnexpectedEOF = errors.New("lib0: unexpected end of input")
	// ErrIntegerOutOfRange means a value exceeds MaxSafeInteger and could not
	// be read (or written) without diverging from lib0.
	ErrIntegerOutOfRange = errors.New("lib0: integer out of range")
	// ErrNegativeLength means a length prefix did not fit in an int.
	ErrNegativeLength = errors.New("lib0: length prefix too large")
	// ErrUnknownAnyTag means an any value carried a type tag lib0 does not
	// define.
	ErrUnknownAnyTag = errors.New("lib0: unknown any type tag")
	// ErrUnsupportedType means a Go value has no representation in the any
	// format.
	ErrUnsupportedType = errors.New("lib0: unsupported any type")
	// ErrDepthExceeded means an any value nested deeper than maxAnyDepth.
	ErrDepthExceeded = errors.New("lib0: any nesting too deep")
)

// UTF16Length returns the number of UTF-16 code units in s.
//
// Yjs struct lengths for string content are JavaScript string lengths, i.e.
// UTF-16 code units: "🎉" has length 2, not 1 (one rune) and not 4 (UTF-8
// bytes). Clock arithmetic that uses len(s) or utf8.RuneCountInString instead
// will silently diverge as soon as a document contains an emoji.
func UTF16Length(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}
