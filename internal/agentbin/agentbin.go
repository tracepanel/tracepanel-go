// Package agentbin finds the Tracepanel agent's binary for a Go app, which has no pip or npm to install it: the
// agent this SDK was released with, downloaded once from its npm platform package (the same binary as on PyPI and
// Maven Central), checked against the SHA-512 this SDK holds for it (integrity.go), and kept in the user's cache
// (~/.cache/tracepanel/agent/VERSION/PLATFORM/), shared by every project and every SDK. The cached binary is checked
// again the first time a process runs it, so a file changed in the cache is downloaded anew, never run.
package agentbin

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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// npm is npm's registry, where the agent's platform packages are.
	npm = "https://registry.npmjs.org/"
	// Binary is the agent's executable name, as on the PATH.
	Binary = "tracepanel-agent"

	downloadTimeout = 2 * time.Minute
	// The largest platform package and binary a download takes: about 20 MB each now, so a body that runs on is cut
	// off rather than filling the memory.
	maxPackage = 128 << 20
	maxBinary  = 256 << 20
)

// Path is the agent's binary: $TRACEPANEL_AGENT_BIN, else this SDK's own version of the agent (in the cache, else
// downloaded there now), else tracepanel-agent on the PATH (without a network, the one pip or npm installed); ""
// when none can be had.
func Path() string {
	if explicit := os.Getenv("TRACEPANEL_AGENT_BIN"); explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		return ""
	}
	if downloaded := Downloaded(); downloaded != "" {
		return downloaded
	}
	path, err := exec.LookPath(Binary)
	if err != nil {
		return ""
	}
	return path
}

var (
	downloadMu sync.Mutex
	// The cached binaries this process checked already: a file is hashed once per process.
	verified = map[string]bool{}
)

// Downloaded is the agent's binary for this platform, at Version: in the cache, else downloaded there now; "" when
// it can't be had.
func Downloaded() string {
	platform := Platform(runtime.GOOS, runtime.GOARCH, isMusl())
	checksum, known := checksums[platform]
	if platform == "" || !known {
		return ""
	}
	downloadMu.Lock()
	defer downloadMu.Unlock()
	binary := cached(platform)
	if verified[binary] {
		return binary
	}
	if info, err := os.Stat(binary); err == nil && !info.IsDir() {
		if fileMatches(binary, checksum) {
			verified[binary] = true
			return binary
		}
		log.Printf("tracepanel: %s isn't the agent this SDK was released with (its checksum differs): downloading it again", binary)
	}
	log.Printf("tracepanel: downloading the agent %s for %s (once, into %s)", Version, platform, filepath.Dir(binary))
	if err := download(tarball(platform, Version), checksum, platform, binary); err != nil {
		log.Printf("tracepanel: the agent couldn't be downloaded: %v. Behind a proxy, set HTTPS_PROXY; with an npm "+
			"mirror, TRACEPANEL_NPM_REGISTRY; or set TRACEPANEL_AGENT_BIN to an agent's binary", err)
		return ""
	}
	verified[binary] = true
	return binary
}

// tarball is where the platform's package is: $TRACEPANEL_NPM_REGISTRY, else npm's own setting
// ($npm_config_registry), else npm's registry.
func tarball(platform, version string) string {
	registry := npm
	for _, name := range []string{"TRACEPANEL_NPM_REGISTRY", "npm_config_registry", "NPM_CONFIG_REGISTRY"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			registry = value
			break
		}
	}
	if !strings.HasSuffix(registry, "/") {
		registry += "/"
	}
	return fmt.Sprintf("%s@tracepanel/agent-%[2]s/-/agent-%[2]s-%[3]s.tgz", registry, platform, version)
}

// download fetches the platform's package, takes the binary out of it, and keeps it at `binary` once its SHA-512 is
// `checksum`.
func download(url, checksum, platform, binary string) error {
	client := &http.Client{
		Timeout: downloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("more than 5 redirects")
			}
			if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("a redirect from https to %s", req.URL.Scheme)
			}
			return nil
		},
	}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", response.StatusCode, url)
	}
	if response.ContentLength > maxPackage {
		return fmt.Errorf("%s is %d bytes, more than an agent's package", url, response.ContentLength)
	}
	tgz, err := io.ReadAll(io.LimitReader(response.Body, maxPackage+1))
	if err != nil {
		return err
	}
	if len(tgz) > maxPackage {
		return fmt.Errorf("%s is more than %d bytes, more than an agent's package", url, maxPackage)
	}
	file, err := Entry(tgz, "package/bin/"+executableName(platform))
	if err != nil {
		return err
	}
	if Checksum(file) != checksum {
		return errors.New("the download isn't the agent this SDK was released with (its checksum differs): not used")
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

// Checksum is a file's SHA-512 as npm writes an integrity: "sha512-<base64>".
func Checksum(data []byte) string {
	digest := sha512.Sum512(data)
	return "sha512-" + base64.StdEncoding.EncodeToString(digest[:])
}

// fileMatches: the file's SHA-512 is `checksum`.
func fileMatches(path, checksum string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	hash := sha512.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxBinary+1)); err != nil {
		return false
	}
	return "sha512-"+base64.StdEncoding.EncodeToString(hash.Sum(nil)) == checksum
}

// Entry is one file of a .tgz, at most maxBinary bytes.
func Entry(tgz []byte, name string) ([]byte, error) {
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
		if header.Name != name {
			continue
		}
		if header.Size > maxBinary {
			return nil, fmt.Errorf("%s is %d bytes, more than an agent", name, header.Size)
		}
		return io.ReadAll(io.LimitReader(archive, maxBinary))
	}
}

// Platform is npm's name for a platform, as the agent's packages are named; "" where there is no agent.
func Platform(goos, goarch string, musl bool) string {
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

// Platforms are the platforms the agent is built for, as Platform names them.
var Platforms = []string{
	"darwin-arm64", "darwin-x64",
	"linux-x64-gnu", "linux-arm64-gnu", "linux-ia32-gnu", "linux-ppc64-gnu", "linux-s390x-gnu", "linux-riscv64-gnu",
	"linux-x64-musl", "linux-arm64-musl", "linux-ia32-musl", "linux-arm",
	"win32-x64", "win32-ia32", "win32-arm64",
}

// isMusl: Linux on musl (Alpine and the like), whose dynamic loader is ld-musl.
func isMusl() bool {
	matches, _ := filepath.Glob("/lib/ld-musl-*")
	return len(matches) > 0
}

// cached is where the platform's agent at Version is kept: <cache>/agent/<version>/<platform>/tracepanel-agent
// (.exe on Windows), in Windows' cache for a win32 platform.
func cached(platform string) string {
	goos := runtime.GOOS
	if strings.HasPrefix(platform, "win32-") {
		goos = "windows"
	}
	return filepath.Join(cacheDir(goos), "agent", Version, platform, executableName(platform))
}

func executableName(platform string) string {
	if strings.HasPrefix(platform, "win32-") {
		return Binary + ".exe"
	}
	return Binary
}

// CacheDir is TRACEPANEL_CACHE_DIR, else the user's cache, as every SDK keeps it: %LOCALAPPDATA%\tracepanel on
// Windows; $XDG_CACHE_HOME/tracepanel, then ~/.cache/tracepanel elsewhere (macOS too).
func CacheDir() string {
	return cacheDir(runtime.GOOS)
}

// cacheDir is CacheDir on goos.
func cacheDir(goos string) string {
	if named := os.Getenv("TRACEPANEL_CACHE_DIR"); named != "" {
		return named
	}
	if local := os.Getenv("LOCALAPPDATA"); goos == "windows" && local != "" {
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
