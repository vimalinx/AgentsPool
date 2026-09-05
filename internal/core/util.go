package core

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SecondsDuration saturates at the duration representation limit instead of
// wrapping a large, explicitly requested timeout into an immediate expiry.
func SecondsDuration(seconds int) time.Duration {
	const maximum = time.Duration(1<<63 - 1)
	if int64(seconds) > int64(maximum/time.Second) {
		return maximum
	}
	return time.Duration(seconds) * time.Second
}

func NewID(prefix string) (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(buf), nil
}

func NewSecret(bytes int) (string, error) {
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func SecureHashEqual(wantHash, candidate string) bool {
	got := HashSecret(candidate)
	if len(wantHash) != len(got) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(wantHash), []byte(got)) == 1
}

func ContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func NormalizedSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}
