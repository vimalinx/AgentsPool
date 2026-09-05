package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxSecretFileBytes = 16 << 10

// resolveSecret accepts either a direct value or a file locator. The locator
// is never treated as the secret itself and the file contents are never
// included in an error.
func resolveSecret(value, filePath, label string) (string, error) {
	value = strings.TrimSpace(value)
	filePath = strings.TrimSpace(filePath)
	if value != "" && filePath != "" {
		return "", fmt.Errorf("%s must use either a direct value or a token file, not both", label)
	}
	if filePath == "" {
		return validateSecretValue(value, label)
	}

	pathInfo, err := os.Lstat(filePath)
	if err != nil {
		return "", fmt.Errorf("inspect %s token file: %w", label, err)
	}
	if !pathInfo.Mode().IsRegular() {
		return "", fmt.Errorf("%s token file must be a regular file", label)
	}
	// Owner-only files and read-only group sharing are accepted. Group write or
	// execute access and all access for other users are rejected.
	if pathInfo.Mode().Perm()&0o037 != 0 {
		return "", fmt.Errorf("%s token file permissions are too broad; use 0600, 0400, or 0640", label)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open %s token file: %w", label, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect open %s token file: %w", label, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return "", fmt.Errorf("%s token file changed while opening", label)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s token file: %w", label, err)
	}
	if len(data) > maxSecretFileBytes {
		return "", fmt.Errorf("%s token file exceeds %d bytes", label, maxSecretFileBytes)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("%s token file is empty", label)
	}
	return validateSecretValue(secret, label)
}

func validateSecretValue(secret, label string) (string, error) {
	if secret == "" {
		return "", nil
	}
	if len(secret) > maxSecretFileBytes {
		return "", fmt.Errorf("%s token exceeds %d bytes", label, maxSecretFileBytes)
	}
	if strings.IndexFunc(secret, func(r rune) bool { return r == '\x00' || r == '\n' || r == '\r' }) >= 0 {
		return "", errors.New(label + " must contain exactly one token")
	}
	return secret, nil
}
