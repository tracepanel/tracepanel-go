package tracepanel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The agent for a Go app, which has no pip or npm to install it: this SDK's own version of it, downloaded once from
// the agent's npm platform package (the same binary as on PyPI), checked against the checksum this SDK was released
// with (integrity.go), and kept in the user's cache (~/.cache/tracepanel/agent/VERSION/PLATFORM/), shared by every
// project and every SDK.

// registry is where an agent platform package is: npm's registry.
var registry = "https://registry.npmjs.org/@tracepanel/agent-%[1]s/-/agent-%[1]s-%[2]s.tgz"

const downloadTimeout = 60 * time.Second

// downloadedAgent is the agent's binary for this platform: in the cache, else downloaded there now; "" when it can't
// be had.
func downloadedAgent() string {
	platform := agentPlatform(runtime.GOOS, runtime.GOARCH, isMusl())
	integrity, known := agentIntegrity[platform]
	if platform == "" || !known {
		return ""
	}
	binary := filepath.Join(cacheDir(), "agent", AgentVersion, platform, executableName(platform))
	if info, err := os.Stat(binary); err == nil && !info.IsDir() {
		return binary
	}
	log.Printf("tracepanel: downloading the agent %s for %s (once, into %s)", AgentVersion, platform, filepath.Dir(binary))
	if err := download(fmt.Sprintf(registry, platform, AgentVersion), integrity, platform, binary); err != nil {
		log.Printf("tracepanel: the agent couldn't be downloaded: %v", err)
		return ""
	}
	return binary
}

func download(url, integrity, platform, binary string) error {
	client := &http.Client{Timeout: downloadTimeout}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", response.StatusCode, url)
	}
	tarball, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	digest := sha512.Sum512(tarball)
	if "sha512-"+base64.StdEncoding.EncodeToString(digest[:]) != integrity {
		return errors.New("the download didn't match its checksum: not used")
	}
	file, err := entry(tarball, "package/bin/"+executableName(platform))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		return err
	}
	partial, err := os.CreateTemp(filepath.Dir(binary), "tracepanel-agent-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(partial.Name())
	if _, err := partial.Write(file); err != nil {
		partial.Close()
		return err
	}
	if err := partial.Close(); err != nil {
		return err
	}
	if err := os.Chmod(partial.Name(), 0o755); err != nil {
		return err
	}
	// Another process may download it too: the last whole file wins, never a half-written one.
	return os.Rename(partial.Name(), binary)
}

// entry is one file of a .tgz.
func entry(tgz []byte, name string) ([]byte, error) {
	unzipped, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	archive := tar.NewReader(unzipped)
	for {
		header, err := archive.Next()
		if err != nil {
			return nil, fmt.Errorf("no %s in the package: %w", name, err)
		}
		if header.Name == name {
			return io.ReadAll(archive)
		}
	}
}

// agentPlatform is npm's name for a platform, as the agent's packages are named; "" where there is no agent.
func agentPlatform(goos, goarch string, musl bool) string {
	arch := map[string]string{
		"amd64": "x64", "arm64": "arm64", "386": "ia32", "ppc64le": "ppc64", "s390x": "s390x", "riscv64": "riscv64",
		"arm": "arm",
	}[goarch]
	if arch == "" {
		return ""
	}
	switch goos {
	case "darwin":
		if arch == "arm64" || arch == "x64" {
			return "darwin-" + arch
		}
	case "windows":
		if arch == "x64" || arch == "ia32" || arch == "arm64" {
			return "win32-" + arch
		}
	case "linux":
		// 32-bit ARM has one static (musl) build, which runs on either.
		if arch == "arm" {
			return "linux-arm"
		}
		if !musl {
			return "linux-" + arch + "-gnu"
		}
		if arch == "x64" || arch == "arm64" || arch == "ia32" {
			return "linux-" + arch + "-musl"
		}
	}
	return ""
}

// isMusl: Linux on musl (Alpine and the like), whose dynamic loader is ld-musl.
func isMusl() bool {
	matches, _ := filepath.Glob("/lib/ld-musl-*")
	return len(matches) > 0
}

func executableName(platform string) string {
	if strings.HasPrefix(platform, "win32-") {
		return "tracepanel-agent.exe"
	}
	return "tracepanel-agent"
}

// cacheDir is TRACEPANEL_CACHE_DIR, else the user's cache: %LOCALAPPDATA% on Windows, $XDG_CACHE_HOME or ~/.cache
// elsewhere, as every SDK keeps it.
func cacheDir() string {
	if named := os.Getenv("TRACEPANEL_CACHE_DIR"); named != "" {
		return named
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "tracepanel")
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "tracepanel")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "tracepanel")
	}
	return filepath.Join(home, ".cache", "tracepanel")
}
