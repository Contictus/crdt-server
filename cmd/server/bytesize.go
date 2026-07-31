package main

import (
	"fmt"
	"strconv"
	"strings"
)

// parseBytes reads a size written the way an operator writes one: a plain number
// of bytes, or a number with a suffix.
//
// Both conventions are accepted because both are in use and arguing about which
// is correct helps nobody: KB and KiB are both 1024 here, which is what a
// container's memory limit means and what `free` prints. An empty string is
// zero, which every caller reads as "no limit".
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	upper := strings.ToUpper(s)
	multiplier := int64(1)
	for _, suffix := range []struct {
		text string
		mult int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
		{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"TB", 1 << 40},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, suffix.text) {
			multiplier = suffix.mult
			upper = strings.TrimSpace(strings.TrimSuffix(upper, suffix.text))
			break
		}
	}
	n, err := strconv.ParseFloat(upper, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size: write bytes, or a number with a suffix like 512MB or 4GiB", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return int64(n * float64(multiplier)), nil
}
