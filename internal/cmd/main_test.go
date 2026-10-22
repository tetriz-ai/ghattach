// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps every test in this package away from the machine's real
// session: HOME (and USERPROFILE) point at an empty temp dir, and both the
// Chrome reader and the keychain read panic unless a test installs a fake. A
// test that forgets therefore fails instead of reading the real cookie store or
// keychain.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	home, err := os.MkdirTemp("", "ghattach-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating the test HOME:", err)
		return 1
	}
	defer os.RemoveAll(home) //nolint:errcheck // best-effort cleanup of our own temp dir
	for _, k := range []string{"HOME", "USERPROFILE"} {
		if err := os.Setenv(k, home); err != nil {
			fmt.Fprintln(os.Stderr, "setting", k, ":", err)
			return 1
		}
	}
	readChromeSession = func(string) (string, error) {
		panic("a test reached the Chrome reader without installing a fake (useFakeChrome)")
	}
	readKeychainSecret = func() (string, error) {
		panic("a test reached the keychain without installing a fake")
	}
	return m.Run()
}
