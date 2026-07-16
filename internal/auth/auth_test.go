package auth_test

import (
	"strings"
	"testing"

	"github.com/shellsecrets/peekastokk/internal/auth"
)

func TestHashAndVerifyRoundtrip(t *testing.T) {
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.IsHash(hash) {
		t.Fatalf("generated hash not recognized: %s", hash)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("unexpected format: %s", hash)
	}

	ok, err := auth.VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = auth.VerifyPassword("wrong", hash)
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := auth.HashPassword("same")
	b, _ := auth.HashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
}

func TestIsHash(t *testing.T) {
	if auth.IsHash("s3cret") || auth.IsHash("$2a$10$bcryptstyle") {
		t.Error("plaintext or foreign hashes misdetected")
	}
	if !auth.IsHash("$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$a2V5") {
		t.Error("argon2id string not detected")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for name, encoded := range map[string]string{
		"not a hash":     "hello",
		"wrong algo":     "$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$a2V5",
		"wrong version":  "$argon2id$v=18$m=65536,t=3,p=4$c2FsdA$a2V5",
		"bad params":     "$argon2id$v=19$m=abc,t=3,p=4$c2FsdA$a2V5",
		"zero time":      "$argon2id$v=19$m=65536,t=0,p=4$c2FsdA$a2V5",
		"huge memory":    "$argon2id$v=19$m=99999999,t=3,p=4$c2FsdA$a2V5",
		"bad salt b64":   "$argon2id$v=19$m=65536,t=3,p=4$!!!$a2V5",
		"bad key b64":    "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$!!!",
		"missing fields": "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA",
	} {
		if ok, err := auth.VerifyPassword("pw", encoded); err == nil || ok {
			t.Errorf("%s: want error, got ok=%v err=%v", name, ok, err)
		}
	}
}
