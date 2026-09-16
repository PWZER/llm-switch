package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// GenerateClientKey returns (plaintext, sha256hex, prefix). The plaintext
// "sk-lsw-<32 hex>" is shown exactly once; storage keeps only the hash.
func GenerateClientKey() (plain, hash, prefix string) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	plain = "sk-lsw-" + hex.EncodeToString(raw)
	hash = sha256Hex(plain)
	prefix = plain[:len("sk-lsw-")+4] + "…" + plain[len(plain)-4:]
	return plain, hash, prefix
}
