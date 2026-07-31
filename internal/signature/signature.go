// Package signature is the one HMAC scheme this server uses to prove that a
// request came from it.
//
// Two things send requests outward - the webhook, and the authorisation
// callback - and both need the receiver to be able to tell our request from
// anybody else's. They share this rather than each growing their own, because
// two implementations of a signing format are two implementations that can
// drift, and the receiver only ever writes one verifier.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sign returns the value of the signature header for a body.
//
// The timestamp is inside the signed text, not just beside it, so a captured
// request cannot be replayed later with a fresh timestamp: changing it breaks
// the signature. A receiver should reject a timestamp far from its own clock
// for the same reason.
func Sign(secret []byte, at time.Time, body []byte) string {
	t := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(t))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a signature header against a body. It is what a receiver
// written in Go would do, and what this package's tests use, so the format has
// exactly one reader and one writer.
//
// tolerance rejects a signature whose timestamp is too far from now; zero
// accepts any age.
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts, sig string
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	if ts == "" || sig == "" {
		return errors.New("signature: malformed header")
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("signature: bad timestamp")
	}
	if tolerance > 0 {
		age := now.Sub(time.Unix(secs, 0))
		if age < -tolerance || age > tolerance {
			return fmt.Errorf("signature: is %s old", age)
		}
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return errors.New("signature: not hex")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("signature: does not match")
	}
	return nil
}
