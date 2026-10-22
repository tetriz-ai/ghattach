// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "rewrite the help golden files in testdata/help")

// TestHelpText pins every command's help, so a flag or wording change shows
// up in review as a golden-file diff. Regenerate with: go test ./internal/cmd -run TestHelpText -update
func TestHelpText(t *testing.T) {
	tests := []struct {
		golden string
		args   []string
	}{
		{golden: "root.txt", args: []string{"--help"}},
		{golden: "upload.txt", args: []string{"upload", "--help"}},
		{golden: "login.txt", args: []string{"login", "--help"}},
		{golden: "logout.txt", args: []string{"logout", "--help"}},
		// -h is short for --help on every command.
		{golden: "root.txt", args: []string{"-h"}},
		{golden: "upload.txt", args: []string{"upload", "-h"}},
		{golden: "login.txt", args: []string{"login", "-h"}},
		{golden: "logout.txt", args: []string{"logout", "-h"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			res := runCLI(t, "", tt.args...)
			require.NoError(t, res.err)
			assert.Empty(t, res.stderr)
			path := filepath.Join("testdata", "help", tt.golden)
			if *updateGolden {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))        //nolint:gosec // G301: testdata is checked in, world-readable like the rest of the repo
				require.NoError(t, os.WriteFile(path, []byte(res.stdout), 0o644)) //nolint:gosec // golden file checked into the repo
			}
			want, err := os.ReadFile(path) //nolint:gosec // fixed path under testdata
			require.NoError(t, err)
			assert.Equal(t, string(want), res.stdout)
		})
	}
}

// TestVersionFlag pins cobra's version line for the version Execute is given.
func TestVersionFlag(t *testing.T) {
	res := runCLI(t, "", "--version")
	require.NoError(t, res.err)
	assert.Equal(t, "ghattach version v0.0.0-test\n", res.stdout)
}

func TestVersionShortFlag(t *testing.T) {
	res := runCLI(t, "", "-v")
	require.NoError(t, res.err)
	assert.Equal(t, "ghattach version v0.0.0-test\n", res.stdout)
}

// TestFlagErrorsPrintNoUsage pins that a flag-parse error comes back as an
// error with nothing printed: Execute prints it once, and no usage text.
func TestFlagErrorsPrintNoUsage(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr string
	}{
		{args: []string{"upload", "--nope"}, wantErr: "unknown flag: --nope"},
		// A value given without --cookie is refused, not ignored.
		{args: []string{"login", "SECRET"}, wantErr: "login takes no arguments; pass the value with --cookie -"},
		{args: []string{"logout", "extra"}, wantErr: `unknown command "extra" for "ghattach logout"`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			home := tempHome(t)
			_, err := storeSession("kept", "")
			require.NoError(t, err)
			res := runCLI(t, "", tt.args...)
			require.Error(t, res.err)
			assert.Equal(t, tt.wantErr, res.err.Error())
			assert.NotContains(t, res.err.Error(), "SECRET", "an argument may be a credential, so it is never echoed")
			assert.Empty(t, res.stdout)
			assert.Empty(t, res.stderr)
			assert.Equal(t, "kept", readStore(t, home).UserSession, "the store is untouched")
		})
	}
}

// TestErrorLineIsOneLine pins that the printed error has no control bytes,
// whatever it quotes, such as a file name holding a newline or an escape.
func TestErrorLineIsOneLine(t *testing.T) {
	assert.Equal(t, "Error: stat missing  [2Jx.png", errorLine(errors.New("stat missing\n\x1b[2Jx.png")))
}
