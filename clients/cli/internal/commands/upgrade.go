package commands

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/creativeprojects/go-selfupdate"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

func NewUpgrade() *cobra.Command {
	var checkOnly bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Check for and apply digitorn updates",
		Long: "Detect the latest release on GitHub, compare with the installed " +
			"version, download and install the bundle if a newer version is available.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd.Context(), checkOnly)
		},
	}
	cmd.Flags().BoolVarP(&checkOnly, "check", "c", false, "only check for updates, don't install")
	return cmd
}

func runUpgrade(ctx context.Context, checkOnly bool) error {
	current := Version
	if current == "dev" {
		fmt.Println("digitorn upgrade: development build — skipping update check")
		return nil
	}
	if !strings.HasPrefix(current, "v") {
		current = "v" + current
	}

	repo := selfupdate.NewRepositorySlug("digitornai", "digitorn")
	latest, found, err := selfupdate.DetectLatest(ctx, repo)
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}
	if !found {
		fmt.Println("No releases found for digitornai/digitorn")
		return nil
	}

	latestVer := latest.Version()
	if !strings.HasPrefix(latestVer, "v") {
		latestVer = "v" + latestVer
	}

	cmp := semver.Compare(latestVer, current)
	if cmp <= 0 {
		fmt.Printf("digitorn %s is up to date\n", Version)
		return nil
	}

	fmt.Printf("Update available: %s → %s\n", Version, latestVer)
	if checkOnly {
		return nil
	}

	versionDir := strings.TrimPrefix(latestVer, "v")
	assetName := fmt.Sprintf("digitorn-%s-%s-%s.tar.gz", versionDir, runtime.GOOS, runtime.GOARCH)
	downloadURL := fmt.Sprintf("https://github.com/digitornai/digitorn/releases/download/%s/%s", latestVer, assetName)

	instDir, err := installDir()
	if err != nil {
		return fmt.Errorf("install directory: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "digitorn-upgrade-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("Downloading %s...\n", assetName)
	tarball := filepath.Join(tmpDir, assetName)
	if err := downloadFile(ctx, tarball, downloadURL); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	// Extract into the version-specific install directory
	verDir := filepath.Join(instDir, latestVer)
	if err := extractTarball(tarball, verDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Update the "current" symlink / junction
	currentLink := filepath.Join(instDir, "current")
	if _, err := os.Lstat(currentLink); err == nil {
		os.Remove(currentLink)
	}
	if runtime.GOOS == "windows" {
		if err := exec.Command("cmd", "/c", "mklink", "/j", currentLink, verDir).Run(); err != nil {
			return fmt.Errorf("junction current: %w", err)
		}
	} else {
		if err := os.Symlink(verDir, currentLink); err != nil {
			return fmt.Errorf("symlink current: %w", err)
		}
	}

	// Ensure ~/.local/bin/ symlinks point to the new binaries
	if err := ensureBinLinks(instDir); err != nil {
		return fmt.Errorf("bin links: %w", err)
	}

	fmt.Printf("digitorn %s installed in %s\n", latestVer, verDir)
	fmt.Println("Binaries updated.")

	// Try to restart the daemon if it's running
	restartDaemon()

	return nil
}

func installDir() (string, error) {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "digitorn"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "digitorn"), nil
}

func userBinDir() (string, error) {
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "digitorn", "bin"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

func ensureBinLinks(instDir string) error {
	binDir, err := userBinDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}

	exe := func(name string) string {
		if runtime.GOOS == "windows" {
			return name + ".exe"
		}
		return name
	}

	currentLink := filepath.Join(instDir, "current")
	for _, name := range []string{"digitornd", "digitorn", "digitorn-tui"} {
		target := filepath.Join(currentLink, exe(name))
		dst := filepath.Join(binDir, exe(name))
		if _, err := os.Stat(target); err != nil {
			continue
		}
		os.Remove(dst)
		if runtime.GOOS == "windows" {
			copyFile(target, dst)
		} else {
			os.Symlink(target, dst)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// downloadFile downloads url into dst.
func downloadFile(ctx context.Context, dst, url string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// extractTarball extracts src (tar.gz) into dst, preserving the
// bundle directory structure (services/, workers/, clients/…).
func extractTarball(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Strip top-level directory from the path (the tarball root)
		parts := strings.SplitN(hdr.Name, "/", 2)
		if len(parts) < 2 {
			continue
		}
		relPath := parts[1]
		fullPath := filepath.Join(dst, relPath)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(fullPath, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(fullPath), 0755)
			of, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(of, tr); err != nil {
				of.Close()
				return err
			}
			of.Close()
		}
	}
	return nil
}

func restartDaemon() {
	switch runtime.GOOS {
	case "linux":
		out, err := exec.Command("systemctl", "--user", "is-active", "digitornd").Output()
		if err == nil && strings.TrimSpace(string(out)) == "active" {
			if exec.Command("systemctl", "--user", "restart", "digitornd").Run() == nil {
				fmt.Println("→ Restarted digitornd (systemd)")
			}
		}
	case "darwin":
		uid := fmt.Sprintf("%d", os.Getuid())
		svc := "gui/" + uid + "/digitornd"
		if _, err := exec.Command("launchctl", "print", svc).Output(); err == nil {
			exec.Command("launchctl", "bootout", svc).Run()
			plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "digitornd.plist")
			if _, err := os.Stat(plist); err == nil {
				if exec.Command("launchctl", "bootstrap", "gui/"+uid, plist).Run() == nil {
					fmt.Println("→ Restarted digitornd (launchd)")
				}
			}
		}
	case "windows":
		out, _ := exec.Command("sc", "query", "digitornd").Output()
		if strings.Contains(string(out), "RUNNING") {
			exec.Command("sc", "stop", "digitornd").Run()
			time.Sleep(2 * time.Second)
			if exec.Command("sc", "start", "digitornd").Run() == nil {
				fmt.Println("→ Restarted digitornd (Windows SCM)")
			}
		}
	}
}
