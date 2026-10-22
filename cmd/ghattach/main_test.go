// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveVersion(t *testing.T) {
	t.Parallel()
	info := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Path: "github.com/tetriz-ai/ghattach", Version: v}}, true
		}
	}
	noInfo := func() (*debug.BuildInfo, bool) { return nil, false }
	tests := []struct {
		name   string
		linked string
		read   func() (*debug.BuildInfo, bool)
		want   string
	}{
		{name: "the linker value wins", linked: "v9.9.9", read: info("v0.1.0"), want: "v9.9.9"},
		{name: "a tagged install reports its tag", read: info("v0.1.0"), want: "v0.1.0"},
		{name: "a checkout build reports its pseudo-version", read: info("v0.1.1-0.20260925000000-abcdef123456+dirty"), want: "v0.1.1-0.20260925000000-abcdef123456+dirty"},
		{name: "a devel build says so", read: info("(devel)"), want: "(devel)"},
		{name: "an empty module version falls back to (devel)", read: info(""), want: "(devel)"},
		{name: "no build info falls back to (devel)", read: noInfo, want: "(devel)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, resolveVersion(tt.linked, tt.read))
		})
	}
}

// buildBinary compiles the command once per test into a temp dir, with the
// given linker flags.
func buildBinary(t *testing.T, ldflags string) string {
	t.Helper()
	name := "ghattach"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	args := []string{"build", "-o", bin}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	out, err := exec.Command("go", append(args, ".")...).CombinedOutput() //nolint:gosec // fixed go build of this package
	require.NoError(t, err, string(out))
	return bin
}

type runResult struct {
	stdout, stderr string
	code           int
}

// runBinary runs bin in dir with an empty HOME, so no real cookie store is
// ever read.
func runBinary(t *testing.T, bin, dir string, args ...string) runResult {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // the binary this test built
	cmd.Dir = dir
	home := t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else {
		require.NoError(t, err)
	}
	return runResult{stdout: out.String(), stderr: errOut.String(), code: code}
}

// TestBinaryVersion pins where the version comes from, and that running the
// binary inside another Go module prints the binary's own version and nothing
// else, and writes nothing there.
func TestBinaryVersion(t *testing.T) {
	t.Parallel()
	linked := buildBinary(t, "-X main.version=v9.9.9")
	plain := buildBinary(t, "")

	otherModule := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(otherModule, "go.mod"), []byte("module example.com/x\n\ngo 1.25\n"), 0o600))

	tests := []struct {
		name string
		bin  string
		dir  string
		want string
	}{
		{name: "linker value", bin: linked, dir: t.TempDir(), want: "ghattach version v9.9.9\n"},
		{name: "linker value inside another Go module", bin: linked, dir: otherModule, want: "ghattach version v9.9.9\n"},
		// A build from a checkout has a VCS-derived or (devel) version; it is
		// never empty, so --version always exists.
		{name: "build info", bin: plain, dir: t.TempDir()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := runBinary(t, tt.bin, tt.dir, "--version")
			assert.Equal(t, 0, res.code)
			assert.Empty(t, res.stderr)
			if tt.want != "" {
				assert.Equal(t, tt.want, res.stdout)
			} else {
				assert.Regexp(t, `^ghattach version (\(devel\)|v\S+)\n$`, res.stdout)
			}
		})
	}
	entries, err := os.ReadDir(otherModule)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "nothing is written into the working directory")
}

// TestBinaryExitCodes pins the process contract: exit 0 on success, exit 1
// with one "Error: <msg>" line on stderr for any failure, and no usage text.
func TestBinaryExitCodes(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t, "-X main.version=v9.9.9")
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "unknown flag", args: []string{"upload", "--nope"}, wantCode: 1, wantStderr: "Error: unknown flag: --nope\n"},
		{name: "missing --repo", args: []string{"upload", "x.png"}, wantCode: 1, wantStderr: "Error: required flag(s) \"repo\" not set\n"},
		{name: "malformed --repo", args: []string{"upload", "x.png", "--repo", "acme"}, wantCode: 1, wantStderr: "Error: --repo must be OWNER/REPO, got \"acme\"\n"},
		{
			name: "not logged in", args: []string{"upload", "x.png", "--repo", "acme/widgets"}, wantCode: 1,
			wantStderr: "Error: loading session: not logged in — run: ghattach login --from-chrome (macOS) or: ghattach login --cookie -\n",
		},
		{name: "unknown command", args: []string{"nosuch"}, wantCode: 1, wantStderr: "Error: unknown command \"nosuch\" for \"ghattach\"\n"},
		{name: "logout with nothing stored", args: []string{"logout"}, wantCode: 0, wantStdout: "No stored session.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := runBinary(t, bin, t.TempDir(), tt.args...)
			assert.Equal(t, tt.wantCode, res.code)
			assert.Equal(t, tt.wantStdout, res.stdout)
			assert.Equal(t, tt.wantStderr, res.stderr)
		})
	}
}

// TestBinaryCompletion pins that completion prints a script for each shell
// docs/reference.md names. It runs the binary, since cobra binds the completion
// command's output once per process.
func TestBinaryCompletion(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t, "")
	for _, shell := range []string{"bash", "fish", "powershell", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			res := runBinary(t, bin, t.TempDir(), "completion", shell)
			assert.Equal(t, 0, res.code)
			assert.Contains(t, res.stdout, "ghattach")
			assert.Empty(t, res.stderr)
		})
	}
}
