// Package auth turns a request into a decision: may this connection open this
// document, and may it write to it.
//
// The token is a JWT signed with HMAC-SHA256. It names the document it is for
// and the permission it grants, so a token that leaks is a token for one
// document with a short life, not a key to the server.
//
// Nothing here talks to the network or to a database. A verifier is a set of
// keys and a clock, which is what makes the whole thing testable without either.
package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Permission is what a token grants on a document.
type Permission string

const (
	// PermissionRead lets a connection receive the document and publish its own
	// cursor, and nothing else.
	PermissionRead Permission = "read"
	// PermissionWrite additionally lets it send updates.
	PermissionWrite Permission = "write"
)

// Claims is the token's payload: the registered claims plus the two this
// server cares about.
//
// The document is named in the token rather than inferred from the URL, so a
// token minted for one document cannot be replayed against another. That is the
// difference between a capability and a login.
type Claims struct {
	jwt.RegisteredClaims
	// Doc is the document name the token is for. Required.
	Doc string `json:"doc"`
	// Perm is "read" or "write". An absent value means read: the safe direction
	// for a field somebody forgot to set.
	Perm Permission `json:"perm,omitempty"`
}

// A Grant is the answer to "who is this and what may they do".
type Grant struct {
	// Subject is the token's sub claim, for logging. It may be empty.
	Subject string
	// Doc is the document the token names.
	Doc string
	// Write says whether this connection may send updates.
	Write bool
	// ExpiresAt is the token's expiry, zero if it has none.
	ExpiresAt time.Time
}

// Errors returned by Verify. The messages are deliberately plain: they are
// written to the client in a permission-denied frame, so they must say enough
// to fix a misconfiguration and nothing about why a signature failed.
var (
	ErrMissingToken = errors.New("no token")
	ErrInvalidToken = errors.New("invalid token")
	ErrExpired      = errors.New("token expired")
	ErrWrongDoc     = errors.New("token is not valid for this document")
)

// Config configures a Verifier.
type Config struct {
	// Secrets are the HMAC keys, tried in order. More than one exists for key
	// rotation: a new key is added at the front, and the old one is removed once
	// every token signed with it has expired.
	Secrets [][]byte
	// Leeway absorbs clock skew between whatever mints tokens and this server.
	Leeway time.Duration
	// Now is the clock, injectable so expiry can be tested without sleeping.
	Now func() time.Time
	// RequireExpiry rejects tokens that never expire. On by default: a
	// capability with no expiry is a permanent one, and the whole safety of
	// putting a token in a URL rests on it being short-lived.
	RequireExpiry bool
	// MaxLifetime rejects tokens whose expiry is further away than this. Zero
	// means no limit.
	MaxLifetime time.Duration
}

// DefaultLeeway is the clock skew allowed between the token's issuer and us.
const DefaultLeeway = 30 * time.Second

// A Verifier checks tokens. It is safe for concurrent use: it never mutates.
type Verifier struct {
	secrets [][]byte
	leeway  time.Duration
	now     func() time.Time
	// requireExpiry and maxLifetime are policy on top of the signature.
	requireExpiry bool
	maxLifetime   time.Duration
	parser        *jwt.Parser
}

// NewVerifier returns a Verifier, or an error if it was given no usable key.
func NewVerifier(cfg Config) (*Verifier, error) {
	secrets := make([][]byte, 0, len(cfg.Secrets))
	for _, s := range cfg.Secrets {
		if len(s) == 0 {
			continue
		}
		// 32 bytes is the output size of the hash the signature uses; a shorter
		// key does not make the signature shorter, it just makes it guessable.
		if len(s) < 32 {
			return nil, fmt.Errorf("auth: a signing secret is %d bytes; at least 32 are needed for HS256", len(s))
		}
		secrets = append(secrets, s)
	}
	if len(secrets) == 0 {
		return nil, errors.New("auth: no signing secret")
	}
	leeway := cfg.Leeway
	if leeway <= 0 {
		leeway = DefaultLeeway
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		secrets:       secrets,
		leeway:        leeway,
		now:           now,
		requireExpiry: cfg.RequireExpiry,
		maxLifetime:   cfg.MaxLifetime,
		parser: jwt.NewParser(
			// The algorithm is pinned. Without this, a token whose header says
			// "alg":"none" - or one signed with our public key treated as an HMAC
			// secret - would be accepted, which is the oldest bug in JWT.
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithLeeway(leeway),
			jwt.WithTimeFunc(now),
		),
	}, nil
}

// Verify checks a token and reports what it grants on doc.
func (v *Verifier) Verify(token, doc string) (Grant, error) {
	if token == "" {
		return Grant{}, ErrMissingToken
	}

	var claims *Claims
	var lastErr error
	for _, secret := range v.secrets {
		parsed, err := v.parser.ParseWithClaims(token, &Claims{}, func(*jwt.Token) (any, error) {
			return secret, nil
		})
		if err != nil {
			lastErr = err
			continue
		}
		claims = parsed.Claims.(*Claims)
		break
	}
	if claims == nil {
		// Expiry is worth telling the client apart from everything else: it is
		// the one failure a correct client can hit on its own, and the one it can
		// do something about by asking for a new token.
		if errors.Is(lastErr, jwt.ErrTokenExpired) || errors.Is(lastErr, jwt.ErrTokenNotValidYet) {
			return Grant{}, ErrExpired
		}
		return Grant{}, ErrInvalidToken
	}

	if claims.Doc == "" {
		return Grant{}, ErrInvalidToken
	}
	// Constant-time comparison is pointless here - the document name is in the
	// URL the client just sent - but a mismatch must be its own error, so a
	// misconfigured client is told what is wrong instead of being told its
	// perfectly good token is invalid.
	if claims.Doc != doc {
		return Grant{}, ErrWrongDoc
	}

	expiry, err := claims.GetExpirationTime()
	if err != nil {
		return Grant{}, ErrInvalidToken
	}
	if expiry == nil {
		if v.requireExpiry {
			return Grant{}, ErrInvalidToken
		}
	} else if v.maxLifetime > 0 && expiry.Sub(v.now()) > v.maxLifetime+v.leeway {
		// A token good for a year is a token that will outlive the reason it was
		// issued. Refusing it here means the mistake surfaces at the first
		// connection rather than in a year.
		return Grant{}, ErrInvalidToken
	}

	perm := claims.Perm
	if perm == "" {
		perm = PermissionRead
	}
	switch perm {
	case PermissionRead, PermissionWrite:
	default:
		// An unknown permission is not silently downgraded to read: it means the
		// issuer and this server disagree about the vocabulary, and guessing
		// which way is generous.
		return Grant{}, ErrInvalidToken
	}

	grant := Grant{
		Subject: claims.Subject,
		Doc:     claims.Doc,
		Write:   perm == PermissionWrite,
	}
	if expiry != nil {
		grant.ExpiresAt = expiry.Time
	}
	return grant, nil
}

// TokenFromRequest pulls the token out of a request.
//
// The query parameter is the one that matters, because it is the only place a
// browser can put anything: the WebSocket API gives no way to set a header, and
// y-websocket's params option writes exactly this. The Authorization header is
// accepted too, for clients that are not browsers.
func TokenFromRequest(r *http.Request) string {
	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}
	if header := r.Header.Get("Authorization"); header != "" {
		if rest, ok := strings.CutPrefix(header, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// Sign mints a token. It is here rather than in a tool because the tests need
// it, and because a signer that lives next to the verifier cannot drift from it.
func Sign(secret []byte, claims Claims) (string, error) {
	if len(secret) < 32 {
		return "", fmt.Errorf("auth: a signing secret is %d bytes; at least 32 are needed for HS256", len(secret))
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}
