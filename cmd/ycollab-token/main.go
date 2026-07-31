// Command ycollab-token mints a token for a document.
//
//	ycollab-token -secret "$YCOLLAB_JWT_SECRET" -doc notes -perm write -ttl 1h
//
// It exists because a server that requires tokens is unusable without something
// that produces them, and because the demo and the integration tests both need
// one. In a real deployment the application that knows who the user is mints
// these, using the same secret; this is the smallest possible stand-in for it.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mesutokul/ycollab/internal/auth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		secret  = flag.String("secret", os.Getenv("YCOLLAB_JWT_SECRET"), "HS256 signing secret, the same one the server was given")
		doc     = flag.String("doc", "", "document name the token is for (required)")
		perm    = flag.String("perm", "write", "read or write")
		subject = flag.String("sub", "", "who the token is for; appears in the server's logs")
		owner   = flag.String("own", "", "tenant the token belongs to; empty reaches only documents that have no owner")
		ttl     = flag.Duration("ttl", time.Hour, "how long the token is valid")
		wsURL   = flag.String("url", "", "if given, print a ready-to-use WebSocket URL instead of the bare token")
		genKey  = flag.Bool("gen-secret", false, "print a new random secret and exit")
	)
	flag.Parse()

	if *genKey {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		fmt.Println(base64.RawURLEncoding.EncodeToString(key))
		return nil
	}

	if *secret == "" {
		return fmt.Errorf("-secret is required (or set YCOLLAB_JWT_SECRET); -gen-secret makes one")
	}
	if *doc == "" {
		return fmt.Errorf("-doc is required: a token names the document it opens")
	}
	permission := auth.Permission(*perm)
	switch permission {
	case auth.PermissionRead, auth.PermissionWrite:
	default:
		return fmt.Errorf("-perm must be read or write, not %q", *perm)
	}

	now := time.Now()
	token, err := auth.Sign([]byte(*secret), auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   *subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(*ttl)),
		},
		Doc:  *doc,
		Perm: permission,
		Own:  *owner,
	})
	if err != nil {
		return err
	}

	if *wsURL == "" {
		fmt.Println(token)
		return nil
	}
	u, err := url.Parse(*wsURL)
	if err != nil {
		return fmt.Errorf("bad -url: %w", err)
	}
	u = u.JoinPath(*doc)
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	fmt.Println(u.String())
	return nil
}
