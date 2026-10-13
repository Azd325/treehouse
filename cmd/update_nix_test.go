package cmd

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type countingTransport struct{ requests int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.requests++
	return nil, http.ErrNotSupported
}

func setNixStore(t *testing.T, inStore bool) {
	t.Helper()
	prev := runningFromNixStore
	runningFromNixStore = func() bool { return inStore }
	t.Cleanup(func() { runningFromNixStore = prev })
}

func setReleaseVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	t.Setenv("TREEHOUSE_NO_UPDATE_CHECK", "")
	return home
}

func writeUpdateCache(t *testing.T, home, latest string) {
	t.Helper()
	dir := filepath.Join(home, ".treehouse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"checked_at":"` + time.Now().Format(time.RFC3339) + `","latest_version":"` + latest + `"}`
	if err := os.WriteFile(filepath.Join(dir, "update-check.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prev := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = prev }()
	fn()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestUpdateRefusedInNixStore(t *testing.T) {
	home := isolateHome(t)
	setNixStore(t, true)
	setReleaseVersion(t, "v3.0.0")
	transport := &countingTransport{}
	prev := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = prev })

	err := updateCmd.RunE(updateCmd, nil)
	if err == nil {
		t.Fatal("update in the Nix store returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "Nix") {
		t.Fatalf("error %q does not mention Nix", err)
	}
	if code := ExitCode(err); code == 0 {
		t.Fatalf("exit status = %d, want non-zero", code)
	}
	if transport.requests != 0 {
		t.Fatalf("update made %d network request(s) in the Nix store, want 0", transport.requests)
	}
	if _, err := os.Stat(filepath.Join(home, ".treehouse")); !os.IsNotExist(err) {
		t.Fatalf("update wrote under the home directory in the Nix store: %v", err)
	}
}

func TestUpdateNoticeSilentInNixStore(t *testing.T) {
	probe := &cobra.Command{Use: "status"}

	t.Run("shows the cached notice outside the store", func(t *testing.T) {
		home := isolateHome(t)
		setNixStore(t, false)
		setReleaseVersion(t, "v3.0.0")
		writeUpdateCache(t, home, "v9.9.9")
		spawned := 0
		prev := spawnBackgroundCheck
		spawnBackgroundCheck = func(string) error { spawned++; return nil }
		t.Cleanup(func() { spawnBackgroundCheck = prev })

		out := captureStderr(t, func() { rootCmd.PersistentPreRun(probe, nil) })
		if !strings.Contains(out, "A new version of treehouse is available") {
			t.Fatalf("control run printed no notice: %q", out)
		}
	})

	t.Run("prints nothing and spawns nothing in the store", func(t *testing.T) {
		home := isolateHome(t)
		setNixStore(t, true)
		setReleaseVersion(t, "v3.0.0")
		writeUpdateCache(t, home, "v9.9.9")
		spawned := 0
		prev := spawnBackgroundCheck
		spawnBackgroundCheck = func(string) error { spawned++; return nil }
		t.Cleanup(func() { spawnBackgroundCheck = prev })

		out := captureStderr(t, func() { rootCmd.PersistentPreRun(probe, nil) })
		if out != "" {
			t.Fatalf("notice printed in the Nix store: %q", out)
		}
		if spawned != 0 {
			t.Fatalf("background check spawned %d time(s) in the Nix store, want 0", spawned)
		}

		if err := os.Remove(filepath.Join(home, ".treehouse", "update-check.json")); err != nil {
			t.Fatal(err)
		}
		captureStderr(t, func() { rootCmd.PersistentPreRun(probe, nil) })
		if spawned != 0 {
			t.Fatalf("background check spawned %d time(s) for a stale cache in the Nix store, want 0", spawned)
		}
	})

	t.Run("spawns the background check outside the store", func(t *testing.T) {
		isolateHome(t)
		setNixStore(t, false)
		setReleaseVersion(t, "v3.0.0")
		spawned := 0
		prev := spawnBackgroundCheck
		spawnBackgroundCheck = func(string) error { spawned++; return nil }
		t.Cleanup(func() { spawnBackgroundCheck = prev })

		captureStderr(t, func() { rootCmd.PersistentPreRun(probe, nil) })
		if spawned != 1 {
			t.Fatalf("control run spawned %d background check(s), want 1", spawned)
		}
	})
}
