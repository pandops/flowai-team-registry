//go:build !integration

package test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// writeCreds writes a syntactically valid NATS user .creds file (a user JWT
// signed by a throwaway account key, plus the user seed).
func writeCreds(t *testing.T, dir string) (string, string) {
	t.Helper()
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := user.PublicKey()
	claims := jwt.NewUserClaims(pub)
	claims.Name = "team-registry"
	token, err := claims.Encode(account)
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := user.Seed()
	creds, err := jwt.FormatUserConfig(token, seed)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "user.creds")
	if err := os.WriteFile(path, creds, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, token
}
