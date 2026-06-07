package mcp

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

var pipPackageRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func validatePackage(pkg string) error {
	if pkg == "" || len(pkg) > 100 {
		return fmt.Errorf("mcp: invalid package length")
	}
	if !pipPackageRe.MatchString(pkg) {
		return fmt.Errorf("mcp: invalid package name")
	}
	return nil
}

func tryAutoInstall(pkg string) error {
	if err := validatePackage(pkg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if uv, err := exec.LookPath("uv"); err == nil {
		return exec.CommandContext(ctx, uv, "pip", "install", pkg).Run()
	}
	if pip, err := exec.LookPath("pip"); err == nil {
		return exec.CommandContext(ctx, pip, "install", pkg).Run()
	}
	return fmt.Errorf("mcp: no installer (uv/pip) available")
}
