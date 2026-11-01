//go:build unix

package tracepanel

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stderrOf is what went to the process's stderr (its file descriptor 2) while f ran.
func stderrOf(t *testing.T, f func()) string {
	t.Helper()
	file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := dup2(int(file.Fd()), 2); err != nil {
		t.Fatal(err)
	}
	f()
	if err := dup2(saved, 2); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Close(saved)
	written, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(written)
}

func TestQuietlyKeepsWhatElseWentToStderr(t *testing.T) {
	ran := false

	got := stderrOf(t, func() {
		ran = quietly(func() {
			// The runtime's own print, as runtime.SetCPUProfileRate writes its warning.
			print(cpuRateWarning)
			_, _ = os.Stderr.WriteString("the app's line\n")
		})
	})

	if !ran || got != "the app's line\n" {
		t.Fatalf("ran %v, stderr %q", ran, got)
	}
}

func TestTheProfileStartsAtItsRateWithoutAWord(t *testing.T) {
	var out bytes.Buffer

	got := stderrOf(t, func() {
		if err := startCPUProfile(&out, 1000); err != nil {
			t.Fatal(err)
		}
		burnProfiled(20 * time.Millisecond)
		pprof.StopCPUProfile()
	})

	if strings.TrimSpace(got) != "" {
		t.Fatalf("stderr: %q", got)
	}
	parsed, err := parseCPUProfile(out.Bytes(), profileLabel)
	if err != nil || parsed.periodNs != 1_000_000 {
		t.Fatalf("profile: %+v, %v", parsed, err)
	}
}
