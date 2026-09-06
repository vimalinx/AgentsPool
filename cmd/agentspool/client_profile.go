package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vimalinx/AgentsPool/internal/client"
)

type clientProfile struct {
	Server             string `json:"server"`
	AllocatorTokenFile string `json:"allocator_token_file,omitempty"`
	AdminTokenFile     string `json:"admin_token_file,omitempty"`
}

func defaultClientProfilePath() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("AGENTSPOOL_CONFIG")); configured != "" {
		if !filepath.IsAbs(configured) {
			return "", errors.New("AGENTSPOOL_CONFIG must be an absolute path")
		}
		return filepath.Clean(configured), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "agentspool", "client.json"), nil
}

func loadClientProfile() (clientProfile, string, error) {
	path, err := defaultClientProfilePath()
	if err != nil {
		return clientProfile{}, "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return clientProfile{}, path, nil
	}
	if err != nil {
		return clientProfile{}, path, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return clientProfile{}, path, fmt.Errorf("client profile %s must be a private regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return clientProfile{}, path, err
	}
	var profile clientProfile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return clientProfile{}, path, fmt.Errorf("decode client profile: %w", err)
	}
	profile.Server = strings.TrimRight(strings.TrimSpace(profile.Server), "/")
	for label, value := range map[string]string{"allocator_token_file": profile.AllocatorTokenFile, "admin_token_file": profile.AdminTokenFile} {
		if value != "" && !filepath.IsAbs(value) {
			return clientProfile{}, path, fmt.Errorf("%s in client profile must be absolute", label)
		}
	}
	return profile, path, nil
}

func runConfigure(args []string) error {
	current, defaultPath, err := loadClientProfile()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	server := fs.String("server", current.Server, "control-plane URL")
	allocatorTokenFile := fs.String("allocator-token-file", current.AllocatorTokenFile, "absolute allocator token file path")
	adminTokenFile := fs.String("admin-token-file", current.AdminTokenFile, "optional absolute admin token file path")
	output := fs.String("output", defaultPath, "absolute client profile path")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("configure does not accept positional arguments")
	}
	profile := clientProfile{
		Server:             strings.TrimRight(strings.TrimSpace(*server), "/"),
		AllocatorTokenFile: strings.TrimSpace(*allocatorTokenFile),
		AdminTokenFile:     strings.TrimSpace(*adminTokenFile),
	}
	if profile.Server == "" {
		return errors.New("--server is required")
	}
	if _, err := client.New(profile.Server, "", 0, false); err != nil {
		return err
	}
	if profile.AllocatorTokenFile == "" && profile.AdminTokenFile == "" {
		return errors.New("an allocator or admin token file is required")
	}
	for label, value := range map[string]string{"allocator": profile.AllocatorTokenFile, "admin": profile.AdminTokenFile} {
		if value == "" {
			continue
		}
		if !filepath.IsAbs(value) {
			return fmt.Errorf("--%s-token-file must be absolute", label)
		}
		if _, err := resolveSecret("", value, label); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(*output) {
		return errors.New("--output must be absolute")
	}
	encoded, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	dir := filepath.Dir(*output)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".client-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, *output); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if directory, err := os.Open(dir); err == nil {
		defer directory.Close()
		if err := directory.Sync(); err != nil {
			return err
		}
	}
	fmt.Println(*output)
	return nil
}
