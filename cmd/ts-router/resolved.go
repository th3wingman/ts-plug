package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const resolvedDir = "/etc/systemd/resolved.conf.d"

func resolvedPath(name string) string {
	return filepath.Join(resolvedDir, "ts-router-"+name+".conf")
}

func resolvedContent(cfg *Config) string {
	return fmt.Sprintf("[Resolve]\nDNS=%s\nDomains=~%s\n", cfg.DNSListen, cfg.Domain)
}

func validateResolvedFull(cfg *Config) error {
	if cfg.Name == "" {
		return errors.New("name is required")
	}
	if cfg.Domain == "" {
		return errors.New("domain is required")
	}
	if cfg.DNSListen == "" {
		return errors.New("dns_listen is required")
	}
	return nil
}

func printResolved(cfg *Config, w io.Writer) error {
	if err := validateResolvedFull(cfg); err != nil {
		return err
	}
	_, err := io.WriteString(w, resolvedContent(cfg))
	return err
}

func installResolved(cfg *Config) error {
	if err := validateResolvedFull(cfg); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("this command requires root; re-run with sudo")
	}

	path := resolvedPath(cfg.Name)
	want := resolvedContent(cfg)

	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) == want {
			fmt.Printf("already installed at %s\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if err := os.MkdirAll(resolvedDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", resolvedDir, err)
	}
	if err := os.WriteFile(path, []byte(want), 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("installed %s\nrun: sudo systemctl restart systemd-resolved\n", path)
	return nil
}

func uninstallResolved(cfg *Config) error {
	if cfg.Name == "" {
		return errors.New("name is required")
	}
	if os.Geteuid() != 0 {
		return errors.New("this command requires root; re-run with sudo")
	}

	path := resolvedPath(cfg.Name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("not installed")
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	fmt.Printf("removed %s\nrun: sudo systemctl restart systemd-resolved\n", path)
	return nil
}
