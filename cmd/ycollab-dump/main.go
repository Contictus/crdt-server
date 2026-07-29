// Command ycollab-dump re-encodes a fixture's document with the Go CRDT engine.
//
// It exists so that tools/verify/apply.mjs can feed Go-produced bytes into a
// real Yjs document - the acceptance test for wire compatibility, which no
// amount of Go-side testing can stand in for.
//
//	go run ./cmd/ycollab-dump -fixtures testdata/fixtures -out tmp/go-state
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mesutokul/ycollab/internal/crdt"
)

func main() {
	fixtures := flag.String("fixtures", filepath.Join("testdata", "fixtures"), "fixtures directory")
	out := flag.String("out", "", "directory to write <scenario>.bin into (required)")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	if err := run(*fixtures, *out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(fixtures, out string) error {
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	written := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		state := filepath.Join(fixtures, e.Name(), "state.bin")
		raw, err := os.ReadFile(state)
		if err != nil {
			continue // not a document fixture
		}
		doc := crdt.NewDoc(1)
		if err := doc.ApplyUpdate(raw); err != nil {
			return fmt.Errorf("%s: apply: %w", e.Name(), err)
		}
		if n := doc.PendingCount(); n != 0 {
			return fmt.Errorf("%s: %d updates left pending", e.Name(), n)
		}
		encoded, err := doc.EncodeStateAsUpdate(nil)
		if err != nil {
			return fmt.Errorf("%s: encode: %w", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(out, e.Name()+".bin"), encoded, 0o644); err != nil {
			return err
		}
		written++
		fmt.Printf("%-40s %6d bytes in, %6d bytes out\n", e.Name(), len(raw), len(encoded))
	}
	if written == 0 {
		return fmt.Errorf("no fixtures found in %s", fixtures)
	}
	return nil
}
