package pack_test

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mesutokul/ycollab/internal/pack"
)

// The only property that really matters: whatever comes back is what went in.
func TestPackRoundTrips(t *testing.T) {
	cases := map[string][]byte{
		"empty":         {},
		"one byte":      {0x00},
		"short":         []byte("a document"),
		"compressible":  bytes.Repeat([]byte("the quick brown fox "), 500),
		"random":        randomBytes(64 << 10),
		"zeroes":        make([]byte, 32<<10),
		"binary at min": randomBytes(256),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, codec := pack.Pack(in)
			back, err := pack.Unpack(out, codec)
			if err != nil {
				t.Fatalf("unpack: %v", err)
			}
			if !bytes.Equal(back, in) {
				t.Fatalf("%d bytes in, %d bytes out", len(in), len(back))
			}
		})
	}
}

// Every real Yjs update this repository has, through the round trip. These are
// the bytes the store will actually be handed.
func TestEveryFixtureRoundTrips(t *testing.T) {
	files, err := filepath.Glob("../../testdata/fixtures/*/*.bin")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	for _, f := range files {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out, codec := pack.Pack(in)
		back, err := pack.Unpack(out, codec)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !bytes.Equal(back, in) {
			t.Errorf("%s: round trip changed the bytes", f)
		}
	}
}

// Storing must never make a payload bigger. A storage layer that can grow a
// document by writing it is a storage layer with a surprising bill, and
// incompressible content is not exotic - a document full of ids is exactly what
// a CRDT update is.
func TestPackingNeverGrowsAPayload(t *testing.T) {
	for size := 0; size <= 1<<16; size = size*2 + 1 {
		for _, in := range [][]byte{randomBytes(size), make([]byte, size)} {
			out, codec := pack.Pack(in)
			if len(out) > len(in) {
				t.Fatalf("%d bytes became %d as %s", len(in), len(out), codec)
			}
		}
	}
}

// Random bytes cannot be compressed, so they must come back marked Raw rather
// than stored larger with a codec that claims otherwise.
func TestIncompressibleContentIsStoredRaw(t *testing.T) {
	in := randomBytes(128 << 10)
	out, codec := pack.Pack(in)
	if codec != pack.Raw {
		t.Errorf("random bytes were stored as %s at %d bytes, from %d", codec, len(out), len(in))
	}
}

// Below the threshold nothing is attempted, because deflate's own overhead is
// larger than anything it could save.
func TestShortPayloadsAreNotCompressed(t *testing.T) {
	for _, size := range []int{0, 1, 100, 255} {
		if _, codec := pack.Pack(bytes.Repeat([]byte("a"), size)); codec != pack.Raw {
			t.Errorf("%d bytes were compressed", size)
		}
	}
	// And just above it, something that clearly compresses does get compressed -
	// otherwise the threshold could be anything and this test would still pass.
	if _, codec := pack.Pack(bytes.Repeat([]byte("a"), 4096)); codec != pack.Deflate {
		t.Error("4 KiB of one repeated byte was not compressed")
	}
}

// A codec this binary does not know is an error, not a guess. It is what a
// rollback to an older binary looks like, and reading those bytes as something
// else would produce a document that is wrong rather than one that is missing.
func TestAnUnknownCodecIsRefused(t *testing.T) {
	_, err := pack.Unpack([]byte("whatever"), pack.Codec(99))
	if err == nil {
		t.Fatal("an unknown codec was accepted")
	}
	if !strings.Contains(err.Error(), "older") {
		t.Errorf("the error does not say what to do about it: %v", err)
	}
}

// Corrupt compressed bytes are an error rather than a partial document. Half a
// document that looks whole is worse than a document that failed to load.
func TestCorruptCompressedBytesAreRefused(t *testing.T) {
	in := bytes.Repeat([]byte("the quick brown fox "), 500)
	out, codec := pack.Pack(in)
	if codec != pack.Deflate {
		t.Fatal("the fixture for this test was not compressed")
	}
	corrupt := append([]byte(nil), out...)
	for i := range corrupt {
		corrupt[i] ^= 0xff
	}
	if _, err := pack.Unpack(corrupt, codec); err == nil {
		t.Error("corrupt bytes decompressed without complaint")
	}
}

// Raw is zero, which is what makes every row written before this existed - and
// every column default - already say the truth about itself.
func TestRawIsTheZeroValue(t *testing.T) {
	if pack.Raw != 0 {
		t.Fatalf("Raw is %d; existing rows default to 0 and would be read as something else", pack.Raw)
	}
	var unset pack.Codec
	in := []byte("bytes written before this package existed")
	back, err := pack.Unpack(in, unset)
	if err != nil || !bytes.Equal(back, in) {
		t.Fatalf("the zero codec did not read stored bytes back: %v", err)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	// Deterministic, so a failure is reproducible.
	r := rand.New(rand.NewPCG(1, 2))
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}

func BenchmarkPack(b *testing.B) {
	in, err := os.ReadFile("../../testdata/fixtures/varint-boundaries/state.bin")
	if err != nil {
		b.Skip(err)
	}
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		pack.Pack(in)
	}
}
