package agentbin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// platformPackage builds an npm platform package: package/bin/<executable> in a .tgz.
func platformPackage(t *testing.T, executable string, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zipped)
	for name, data := range map[string][]byte{"package/package.json": []byte("{}"), "package/bin/" + executable: content} {
		_ = archive.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))})
		_, _ = archive.Write(data)
	}
	_ = archive.Close()
	_ = zipped.Close()
	return buffer.Bytes()
}

const script = "#!/bin/sh\necho agent\n"

func TestTheAgentIsDownloadedCheckedAgainstItsBinarysChecksum(t *testing.T) {
	tgz := platformPackage(t, "tracepanel-agent", []byte(script))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(tgz) }))
	defer server.Close()
	binary := filepath.Join(t.TempDir(), "agent", "0.10.0", "linux-x64-gnu", "tracepanel-agent")

	err := download(server.URL+"/agent.tgz", Checksum([]byte(script)), "linux-x64-gnu", binary)
	data, _ := os.ReadFile(binary)
	info, _ := os.Stat(binary)
	refused := download(server.URL+"/agent.tgz", Checksum([]byte("another")), "linux-x64-gnu", binary+"2")

	// Windows has no executable bit: a file is run by its name.
	executable := runtime.GOOS == "windows" || info != nil && info.Mode().Perm()&0o100 != 0
	if err != nil || string(data) != script || !executable {
		t.Fatalf("%v: %q, %v", err, data, info)
	}
	if refused == nil || !strings.Contains(refused.Error(), "checksum") {
		t.Fatalf("a tampered download: %v", refused)
	}
	if _, err := os.Stat(binary + "2"); err == nil {
		t.Fatal("a tampered download was kept")
	}
}

func TestADownloadThatRunsOnIsCutOff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte{0}, 1<<20)
		for range maxPackage>>20 + 2 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	err := download(server.URL+"/agent.tgz", Checksum([]byte(script)), "linux-x64-gnu", filepath.Join(t.TempDir(), "a"))

	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("an endless download: %v", err)
	}
}

func TestTheCachedBinaryIsCheckedOnceAndDownloadedAgainWhenChanged(t *testing.T) {
	platform := Platform(runtime.GOOS, runtime.GOARCH, isMusl())
	if platform == "" {
		t.Skip("no agent for this platform")
	}
	tgz := platformPackage(t, executableName(platform), []byte(script))
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		if r.URL.Path != "/mirror/@tracepanel/agent-"+platform+"/-/agent-"+platform+"-"+Version+".tgz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(tgz)
	}))
	defer server.Close()
	cache := t.TempDir()
	t.Setenv("TRACEPANEL_CACHE_DIR", cache)
	t.Setenv("TRACEPANEL_NPM_REGISTRY", server.URL+"/mirror")
	saved := checksums[platform]
	checksums[platform] = Checksum([]byte(script))
	t.Cleanup(func() { checksums[platform] = saved; clear(verified) })
	binary := filepath.Join(cache, "agent", Version, platform, executableName(platform))

	first := Downloaded()
	_ = os.WriteFile(binary, []byte("changed after the check"), 0o755)
	// This process checked it already: the file isn't hashed again.
	again := Downloaded()
	clear(verified)
	// A new process finds the change, and downloads the agent anew.
	anew := Downloaded()
	data, _ := os.ReadFile(binary)

	if first != binary || again != binary || anew != binary || downloads != 2 || string(data) != script {
		t.Fatalf("%q %q %q, %d downloads, %q", first, again, anew, downloads, data)
	}
}

func TestTheRegistryIsNpmsOrTheOneNamed(t *testing.T) {
	t.Setenv("TRACEPANEL_NPM_REGISTRY", "")
	t.Setenv("npm_config_registry", "")
	t.Setenv("NPM_CONFIG_REGISTRY", "")
	npmURL := tarball("linux-x64-gnu", "0.10.0")
	t.Setenv("npm_config_registry", "https://npm.example.com/repository/npm")
	npmSetting := tarball("linux-x64-gnu", "0.10.0")
	t.Setenv("TRACEPANEL_NPM_REGISTRY", "https://mirror.example.com/")
	named := tarball("linux-x64-gnu", "0.10.0")

	want := []string{
		"https://registry.npmjs.org/@tracepanel/agent-linux-x64-gnu/-/agent-linux-x64-gnu-0.10.0.tgz",
		"https://npm.example.com/repository/npm/@tracepanel/agent-linux-x64-gnu/-/agent-linux-x64-gnu-0.10.0.tgz",
		"https://mirror.example.com/@tracepanel/agent-linux-x64-gnu/-/agent-linux-x64-gnu-0.10.0.tgz",
	}
	if got := []string{npmURL, npmSetting, named}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%q", got)
	}
}

func TestEachPlatformHasItsPackageAndItsChecksum(t *testing.T) {
	cases := map[[3]string]string{
		{"darwin", "arm64", ""}: "darwin-arm64", {"darwin", "amd64", ""}: "darwin-x64",
		{"linux", "amd64", ""}: "linux-x64-gnu", {"linux", "amd64", "musl"}: "linux-x64-musl",
		{"linux", "arm", ""}: "linux-arm", {"linux", "ppc64le", ""}: "linux-ppc64-gnu",
		{"windows", "amd64", ""}: "win32-x64", {"freebsd", "amd64", ""}: "", {"linux", "riscv64", "musl"}: "",
	}
	for input, want := range cases {
		got := Platform(input[0], input[1], input[2] == "musl")
		if got != want {
			t.Errorf("%v: %q, want %q", input, got, want)
		}
	}
	for _, platform := range Platforms {
		if !strings.HasPrefix(checksums[platform], "sha512-") {
			t.Errorf("no checksum for %s", platform)
		}
	}
}
