package auth_test

import (
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mesutokul/ycollab/internal/auth"
)

var (
	secret    = []byte("a-test-secret-that-is-long-enough-for-hs256")
	otherKey  = []byte("a-different-secret-that-is-also-long-enough")
	testClock = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
)

func testVerifier(t *testing.T, cfg auth.Config) *auth.Verifier {
	t.Helper()
	if cfg.Secrets == nil {
		cfg.Secrets = [][]byte{secret}
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return testClock }
	}
	v, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v
}

func mint(t *testing.T, key []byte, claims auth.Claims) string {
	t.Helper()
	token, err := auth.Sign(key, claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

// writeToken is a valid write token for doc, expiring an hour from the test
// clock.
func writeToken(t *testing.T, doc string) string {
	t.Helper()
	return mint(t, secret, auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "ada",
			ExpiresAt: jwt.NewNumericDate(testClock.Add(time.Hour)),
		},
		Doc:  doc,
		Perm: auth.PermissionWrite,
	})
}

func TestVerifyAcceptsAGoodToken(t *testing.T) {
	v := testVerifier(t, auth.Config{})
	grant, err := v.Verify(writeToken(t, "notes"), "notes")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !grant.Write {
		t.Fatal("a write token did not grant write")
	}
	if grant.Subject != "ada" || grant.Doc != "notes" {
		t.Fatalf("grant is %+v", grant)
	}
	if !grant.ExpiresAt.Equal(testClock.Add(time.Hour)) {
		t.Fatalf("expiry came back as %v", grant.ExpiresAt)
	}
}

// An absent permission means read. A field somebody forgot to set must fail
// closed, and read is the closed direction.
func TestAnAbsentPermissionMeansRead(t *testing.T) {
	v := testVerifier(t, auth.Config{})
	token := mint(t, secret, auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(testClock.Add(time.Hour))},
		Doc:              "notes",
	})
	grant, err := v.Verify(token, "notes")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if grant.Write {
		t.Fatal("a token with no permission granted write")
	}
}

func TestVerifyRejections(t *testing.T) {
	hour := func(d time.Duration) *jwt.NumericDate { return jwt.NewNumericDate(testClock.Add(d)) }

	cases := []struct {
		name  string
		token func(t *testing.T) string
		doc   string
		want  error
	}{
		{
			name:  "no token",
			token: func(*testing.T) string { return "" },
			doc:   "notes",
			want:  auth.ErrMissingToken,
		},
		{
			name:  "not a token at all",
			token: func(*testing.T) string { return "hello" },
			doc:   "notes",
			want:  auth.ErrInvalidToken,
		},
		{
			name: "signed with another key",
			token: func(t *testing.T) string {
				return mint(t, otherKey, auth.Claims{
					RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: hour(time.Hour)},
					Doc:              "notes", Perm: auth.PermissionWrite,
				})
			},
			doc:  "notes",
			want: auth.ErrInvalidToken,
		},
		{
			name: "expired",
			token: func(t *testing.T) string {
				return mint(t, secret, auth.Claims{
					RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: hour(-time.Hour)},
					Doc:              "notes", Perm: auth.PermissionWrite,
				})
			},
			doc:  "notes",
			want: auth.ErrExpired,
		},
		{
			name: "not valid yet",
			token: func(t *testing.T) string {
				return mint(t, secret, auth.Claims{
					RegisteredClaims: jwt.RegisteredClaims{
						NotBefore: hour(time.Hour),
						ExpiresAt: hour(2 * time.Hour),
					},
					Doc: "notes", Perm: auth.PermissionWrite,
				})
			},
			doc:  "notes",
			want: auth.ErrExpired,
		},
		{
			// The point of naming the document in the token: a capability for one
			// document must not open another.
			name:  "minted for another document",
			token: func(t *testing.T) string { return writeToken(t, "payroll") },
			doc:   "notes",
			want:  auth.ErrWrongDoc,
		},
		{
			name: "no document",
			token: func(t *testing.T) string {
				return mint(t, secret, auth.Claims{
					RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: hour(time.Hour)},
					Perm:             auth.PermissionWrite,
				})
			},
			doc:  "notes",
			want: auth.ErrInvalidToken,
		},
		{
			name: "a permission we do not know",
			token: func(t *testing.T) string {
				return mint(t, secret, auth.Claims{
					RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: hour(time.Hour)},
					Doc:              "notes", Perm: "admin",
				})
			},
			doc:  "notes",
			want: auth.ErrInvalidToken,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := testVerifier(t, auth.Config{})
			if _, err := v.Verify(tc.token(t), tc.doc); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// The oldest JWT bug: a token whose header says the signature algorithm is
// "none", or one signed with a different family of algorithm, must not be
// accepted just because the library will happily parse it.
func TestAlgorithmIsPinned(t *testing.T) {
	v := testVerifier(t, auth.Config{})
	claims := &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(testClock.Add(time.Hour))},
		Doc:              "notes",
		Perm:             auth.PermissionWrite,
	}

	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign with none: %v", err)
	}
	if _, err := v.Verify(none, "notes"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("an unsigned token was answered with %v", err)
	}

	// And a stronger HMAC is still not the one we agreed on: accepting any HS*
	// would mean the header, which the attacker writes, decides the check.
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString(secret)
	if err != nil {
		t.Fatalf("sign with hs512: %v", err)
	}
	if _, err := v.Verify(hs512, "notes"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("a token signed with another algorithm was answered with %v", err)
	}
}

// Key rotation: for a while both keys are configured, and tokens signed with
// either must work.
func TestBothKeysWorkDuringRotation(t *testing.T) {
	v := testVerifier(t, auth.Config{Secrets: [][]byte{otherKey, secret}})
	for _, key := range [][]byte{secret, otherKey} {
		token := mint(t, key, auth.Claims{
			RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(testClock.Add(time.Hour))},
			Doc:              "notes", Perm: auth.PermissionWrite,
		})
		if _, err := v.Verify(token, "notes"); err != nil {
			t.Fatalf("a token signed with a configured key was rejected: %v", err)
		}
	}
}

// Expiry is what makes a token in a URL acceptable, so the server can insist on
// it and on it being short.
func TestExpiryPolicy(t *testing.T) {
	forever := mint(t, secret, auth.Claims{Doc: "notes", Perm: auth.PermissionWrite})

	if _, err := testVerifier(t, auth.Config{}).Verify(forever, "notes"); err != nil {
		t.Fatalf("a token with no expiry was rejected without the policy on: %v", err)
	}
	strict := testVerifier(t, auth.Config{RequireExpiry: true})
	if _, err := strict.Verify(forever, "notes"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("a token with no expiry was accepted: %v", err)
	}

	long := mint(t, secret, auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(testClock.Add(48 * time.Hour))},
		Doc:              "notes", Perm: auth.PermissionWrite,
	})
	bounded := testVerifier(t, auth.Config{MaxLifetime: time.Hour})
	if _, err := bounded.Verify(long, "notes"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("a 48-hour token was accepted with a 1-hour limit: %v", err)
	}
	if _, err := bounded.Verify(writeToken(t, "notes"), "notes"); err != nil {
		t.Fatalf("a 1-hour token was rejected by a 1-hour limit: %v", err)
	}
}

// Clock skew between whatever mints tokens and this server is normal, and a
// token that expired a second ago by our clock is not an attack.
func TestLeewayAbsorbsSkew(t *testing.T) {
	token := mint(t, secret, auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(testClock)},
		Doc:              "notes", Perm: auth.PermissionWrite,
	})
	v := testVerifier(t, auth.Config{
		Leeway: 30 * time.Second,
		Now:    func() time.Time { return testClock.Add(10 * time.Second) },
	})
	if _, err := v.Verify(token, "notes"); err != nil {
		t.Fatalf("a token ten seconds past expiry was rejected with 30s leeway: %v", err)
	}
	late := testVerifier(t, auth.Config{
		Leeway: 30 * time.Second,
		Now:    func() time.Time { return testClock.Add(time.Minute) },
	})
	if _, err := late.Verify(token, "notes"); !errors.Is(err, auth.ErrExpired) {
		t.Fatalf("a token a minute past expiry was accepted: %v", err)
	}
}

func TestAShortSecretIsRefused(t *testing.T) {
	if _, err := auth.NewVerifier(auth.Config{Secrets: [][]byte{[]byte("short")}}); err == nil {
		t.Fatal("a five-byte HMAC secret was accepted")
	}
	if _, err := auth.NewVerifier(auth.Config{}); err == nil {
		t.Fatal("a verifier with no secret at all was accepted")
	}
	if _, err := auth.Sign([]byte("short"), auth.Claims{Doc: "notes"}); err == nil {
		t.Fatal("signing with a five-byte secret was accepted")
	}
}

func TestTokenFromRequest(t *testing.T) {
	cases := []struct {
		name string
		set  func(*http.Request)
		want string
	}{
		{
			// The one that matters: a browser cannot set a header on a WebSocket,
			// and y-websocket's params option writes exactly this.
			name: "query parameter",
			set:  func(r *http.Request) { r.URL.RawQuery = "token=abc" },
			want: "abc",
		},
		{
			name: "bearer header",
			set:  func(r *http.Request) { r.Header.Set("Authorization", "Bearer abc") },
			want: "abc",
		},
		{
			name: "the query parameter wins",
			set: func(r *http.Request) {
				r.URL.RawQuery = "token=fromquery"
				r.Header.Set("Authorization", "Bearer fromheader")
			},
			want: "fromquery",
		},
		{
			name: "a scheme we do not accept",
			set:  func(r *http.Request) { r.Header.Set("Authorization", "Basic abc") },
			want: "",
		},
		{
			name: "nothing at all",
			set:  func(*http.Request) {},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/notes", nil)
			tc.set(r)
			if got := auth.TokenFromRequest(r); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A generated secret must be usable, which is worth checking because the server
// generates one when it is asked to and the length rule is enforced in two
// places.
func TestAGeneratedSecretWorks(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	v := testVerifier(t, auth.Config{Secrets: [][]byte{key}})
	token := mint(t, key, auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(testClock.Add(time.Hour))},
		Doc:              "notes", Perm: auth.PermissionWrite,
	})
	if _, err := v.Verify(token, "notes"); err != nil {
		t.Fatalf("verify: %v", err)
	}
}
