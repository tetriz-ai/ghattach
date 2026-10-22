// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin the limits listed in docs/limitations.md: design
// choices the docs state as rules. Changing one is a
// deliberate change to the contract.

// TestUploadSelfHealUnpinnedSessionTakesChromesAccount pins that a pasted
// session has no profile pin, so on expiry the freshest Chrome profile's
// cookie replaces it, whichever GitHub account that profile is signed in to.
func TestUploadSelfHealUnpinnedSessionTakesChromesAccount(t *testing.T) {
	home := tempHome(t)
	chrome := &fakeChrome{session: "sess-account-b"}
	useFakeChrome(t, chrome)
	res := runCLI(t, "sess-account-a\n", "login", "--cookie", "-")
	require.NoError(t, res.err)
	require.Empty(t, readStore(t, home).ChromeProfile, "a pasted session carries no pin")

	s := newCLIStub(t, "sess-account-b") // account A's session is dead
	useStub(t, s)
	shot := writeFile(t, t.TempDir(), "shot.png", "png")
	res = runCLI(t, "", "upload", shot, "-R", "acme/widgets", "--repo-id", "7")
	require.NoError(t, res.err)
	assert.Equal(t, assetURL(1)+"\n", res.stdout)
	assert.Equal(t, "Re-read the session from Chrome; retrying.\n", res.stderr)
	assert.Equal(t, []string{""}, chrome.profiles, "no pin, so Chrome is free to pick any profile")
	assert.Equal(t, "sess-account-b", readStore(t, home).UserSession, "the stored session is now Chrome's")
}

// TestChromeProfileIsAGlob pins that --chrome-profile is used as a glob
// pattern: "Profile *" reads every matching profile, and login stores the
// pattern itself as the pin.
func TestChromeProfileIsAGlob(t *testing.T) {
	if _, err := os.Stat(sqlite3Bin); err != nil {
		t.Skipf("%s not present", sqlite3Bin)
	}
	dir := t.TempDir()
	for _, profile := range []string{"Default", "Profile 1", "Profile 2"} {
		makeFakeChromeStore(t, dir, profile, "INSERT INTO cookies VALUES ('github.com','user_session',X'763130AB',13400000000000000);")
	}
	rows, err := readChromeSessionRows(dir, "Profile *")
	require.NoError(t, err)
	var got []string
	for _, r := range rows {
		got = append(got, r.profile)
	}
	assert.Equal(t, []string{"Profile 1", "Profile 2"}, got)

	home := tempHome(t)
	useFakeChrome(t, &fakeChrome{session: "sess"})
	res := runCLI(t, "", "login", "--from-chrome", "--chrome-profile", "Profile *")
	require.NoError(t, res.err)
	assert.Equal(t, "Profile *", readStore(t, home).ChromeProfile)
}
