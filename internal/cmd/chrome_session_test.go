// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useFakeKeychain replaces the keychain read for the rest of the test.
func useFakeKeychain(t *testing.T, secret string, err error) {
	t.Helper()
	old := readKeychainSecret
	readKeychainSecret = func() (string, error) { return secret, err }
	t.Cleanup(func() { readKeychainSecret = old })
}

// TestChromeUserSessionEndToEnd drives the whole --from-chrome chain on macOS
// (store lookup, profile pick, key derivation, decrypt, host-hash check)
// against a fake Chrome store under the test's own HOME and a fake keychain
// secret. It never touches the real store or keychain.
func TestChromeUserSessionEndToEnd(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("chromeUserSession reads Chrome only on macOS")
	}
	if _, err := os.Stat(sqlite3Bin); err != nil {
		t.Skipf("%s not present", sqlite3Bin)
	}
	home := tempHome(t)
	dataDir := filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	const key = "fake-key"
	micros := (time.Now().Add(24*time.Hour).Unix() + chromeEpochOffsetSeconds) * chromeEpochMicrosPerSecond
	blob := sealChromeCookie(t, key, chromeCookieHost, "sess-from-chrome")
	makeFakeChromeStore(t, dataDir, "Default",
		fmt.Sprintf("INSERT INTO cookies VALUES ('github.com','user_session',X'%X',%d);", blob, micros))

	tests := []struct {
		name        string
		profile     string
		keychainErr error
		want        string
		wantErr     string
	}{
		{name: "any profile", want: "sess-from-chrome"},
		{name: "a named profile", profile: "Default", want: "sess-from-chrome"},
		{name: "a missing profile", profile: "Work", wantErr: `no cookie store for Chrome profile "Work"`},
		{name: "a denied keychain prompt", keychainErr: errors.New("denied"), wantErr: "unlocking Chrome's cookie key: denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useFakeKeychain(t, key, tt.keychainErr)
			got, err := chromeUserSession(tt.profile)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseKeychainOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		out     string
		want    string
		wantErr bool
	}{
		{out: "abc\n", want: "abc"},
		{out: "abc\n\n", want: "abc"},
		{out: "abc", want: "abc"},
		{out: "\n", wantErr: true},
		{out: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.out), func(t *testing.T) {
			t.Parallel()
			got, err := parseKeychainOutput([]byte(tt.out))
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "keychain item is empty")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestHashlessChromeCookieIsRefused pins that a cookie sealed without the
// host-hash prefix, as Chrome versions before 24 stored them, is refused.
func TestHashlessChromeCookieIsRefused(t *testing.T) {
	t.Parallel()
	const key = "fake-key"
	blob := sealChromeCookieWithoutHostHash(t, key, "sess-value-from-an-old-chrome-cookie-db")
	_, err := decryptChromeCookie(key, blob, chromeCookieHost)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not carry the expected github.com host hash")
}
