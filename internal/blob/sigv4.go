package blob

// AWS Signature Version 4, the part of it S3 needs.
//
// Written out rather than taken from a library for the same reason the CRDT
// engine and the lib0 codec are: the surface actually used here is four
// requests, and the alternative was fifteen modules in go.mod to sign them. What
// that costs is stated plainly - see the package comment in s3.go for what a
// hand-rolled signer does not do.
//
// The failure mode is a good one. A signature that is wrong is refused by the
// service with 403 SignatureDoesNotMatch, every time, on the first request. It
// cannot be subtly wrong in a way that works today and leaks later.
//
// Derived from the AWS "Signature Version 4 signing process" documentation. The
// S3-specific parts, which are the ones people get wrong:
//
//   - the canonical URI is the path encoded once, not twice, and slashes are
//     left alone. Other services encode twice; S3 does not.
//   - x-amz-content-sha256 is required, and carries the payload hash rather
//     than the literal UNSIGNED-PAYLOAD, because we always have the bytes.
//   - the service name in the credential scope is "s3".

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	algorithm   = "AWS4-HMAC-SHA256"
	serviceName = "s3"
	// isoLayout is the x-amz-date format: ISO 8601 basic, always UTC.
	isoLayout  = "20060102T150405Z"
	dateLayout = "20060102"
)

// Credentials are what the signer needs to prove who it is.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is set for temporary credentials. When present it is both
	// sent as a header and signed, so it cannot be swapped for another.
	SessionToken string
}

// sign adds the Authorization header to a request, in place.
//
// payloadHash is the hex SHA-256 of the body. It is a parameter rather than
// something computed here because the caller already has the bytes and a second
// pass over a multi-megabyte snapshot to hash it twice is a waste.
func sign(r *http.Request, creds Credentials, region string, now time.Time, payloadHash string) {
	now = now.UTC()
	amzDate := now.Format(isoLayout)
	scopeDate := now.Format(dateLayout)

	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	// Host is not in r.Header - net/http keeps it on the request - but it has to
	// be signed, so it is added to the canonical form by hand below.

	canonicalHeaders, signedHeaders := canonicalizeHeaders(r)
	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.EscapedPath()),
		canonicalQuery(r),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := scopeDate + "/" + region + "/" + serviceName + "/aws4_request"
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, scopeDate, region), stringToSign))
	r.Header.Set("Authorization", algorithm+
		" Credential="+creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

// signingKey derives the request's key. The chain is what makes a leaked
// signature useless tomorrow and useless in another region: each step folds in
// one more piece of scope, and none of them is reversible.
func signingKey(secret, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, serviceName)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalizeHeaders returns the canonical header block and the signed-header
// list. Host is included explicitly because net/http does not keep it in the
// header map, and a signature that does not cover the host is a signature that
// can be replayed against another bucket.
func canonicalizeHeaders(r *http.Request) (string, string) {
	names := make([]string, 0, len(r.Header)+1)
	values := make(map[string]string, len(r.Header)+1)

	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	names = append(names, "host")
	values["host"] = host

	for name, vs := range r.Header {
		lower := strings.ToLower(name)
		// Only the headers that matter to S3 are signed. Signing everything
		// would break the moment a proxy or Go itself added one: Go sets
		// Accept-Encoding and Content-Length on the way out, after this runs.
		if lower != "content-type" && !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		names = append(names, lower)
		// Sequential whitespace collapses and the value is trimmed, per the
		// specification. Our own values contain none of either, but a caller's
		// content type might.
		values[lower] = strings.Join(strings.Fields(strings.Join(vs, ",")), " ")
	}
	sort.Strings(names)

	var canonical strings.Builder
	for _, n := range names {
		canonical.WriteString(n)
		canonical.WriteByte(':')
		canonical.WriteString(values[n])
		canonical.WriteByte('\n')
	}
	return canonical.String(), strings.Join(names, ";")
}

// canonicalURI encodes the path the way S3 wants it: once, with slashes left as
// separators. net/url's EscapedPath is close but leaves some characters S3
// expects encoded, so the path is re-encoded segment by segment from its decoded
// form.
func canonicalURI(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	segments := strings.Split(escapedPath, "/")
	for i, seg := range segments {
		// Undo net/url's encoding first, so a segment is encoded exactly once
		// however it arrived.
		segments[i] = uriEncode(unescape(seg))
	}
	return strings.Join(segments, "/")
}

// canonicalQuery sorts and encodes the query string. Sorting is by the encoded
// key, then by the encoded value, which is what the specification says and what
// a naive sort of the decoded form gets wrong.
func canonicalQuery(r *http.Request) string {
	q := r.URL.Query()
	if len(q) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(q))
	for key, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, uriEncode(key)+"="+uriEncode(v))
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// uriEncode is RFC 3986 percent-encoding with AWS's unreserved set. Written out
// because net/url has no function that matches: QueryEscape turns a space into
// "+", and PathEscape leaves characters AWS wants encoded.
func uriEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

const upperHex = "0123456789ABCDEF"

// unescape reverses percent-encoding, leaving anything malformed alone. A key
// this server writes is hex and slashes, so the malformed case cannot arise from
// our own paths; it is handled rather than assumed away.
func unescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := fromHex(s[i+1])
			lo, ok2 := fromHex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
