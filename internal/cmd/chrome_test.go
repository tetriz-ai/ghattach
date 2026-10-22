// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1" //nolint:gosec // mirrors Chrome's fixed KDF so the test encrypts exactly as the browser does
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealChromeCookie encrypts a value the way Chrome does, so the decrypt path is
// tested against an independently-constructed blob rather than a fixture whose
// provenance the reader has to trust.
func sealChromeCookie(t *testing.T, secret, hostKey, value string) []byte {
	t.Helper()
	key, err := pbkdf2.Key(sha1.New, secret, []byte(chromeKDFSalt), chromeKDFIterations, chromeKDFKeyLen)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)

	hostHash := sha256.Sum256([]byte(hostKey))
	plain := append(hostHash[:], value...)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)

	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(out, plain)
	return append([]byte(chromeCipherPrefix), out...)
}

// sealChromeCookieWithoutHostHash encrypts a value the way Chrome did before
// it prefixed the plaintext with the host's SHA-256.
func sealChromeCookieWithoutHostHash(t *testing.T, secret, value string) []byte {
	t.Helper()
	key, err := pbkdf2.Key(sha1.New, secret, []byte(chromeKDFSalt), chromeKDFIterations, chromeKDFKeyLen)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	plain := []byte(value)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(out, plain)
	return append([]byte(chromeCipherPrefix), out...)
}

func TestDecryptChromeCookie(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3tKeychainVal" // fake keychain secret, test fixture. gitleaks:allow
	const host = "github.com"
	sealed := sealChromeCookie(t, secret, host, "abc123XYZ_-")

	tests := []struct {
		name    string
		secret  string
		blob    []byte
		host    string
		want    string
		wantErr string
	}{
		{name: "round trip", secret: secret, blob: sealed, host: host, want: "abc123XYZ_-"},
		{
			// The signal that matters most: a wrong key still decrypts to
			// well-formed bytes, and only the host-hash check catches it.
			name: "wrong keychain secret", secret: "wrong", blob: sealed, host: host,
			wantErr: "wrong Keychain secret",
		},
		{
			name: "value sealed for a different host", secret: secret, host: "other.example",
			blob: sealed, wantErr: "wrong Keychain secret",
		},
		{
			name: "app-bound v20 blob", secret: secret, host: host,
			blob: append([]byte("v20"), bytes.Repeat([]byte{0xAB}, 32)...),
			// Names the cause so an operator is not left guessing when Chrome
			// eventually migrates macOS profiles.
			wantErr: "app-bound encryption",
		},
		{
			name: "unencrypted legacy value", secret: secret, host: host,
			blob: []byte("plaintextcookie"), wantErr: "not \"v10\"",
		},
		{
			name: "truncated ciphertext", secret: secret, host: host,
			blob: append([]byte(chromeCipherPrefix), 0x01, 0x02), wantErr: "AES blocks",
		},
		{
			name: "empty ciphertext", secret: secret, host: host,
			blob: []byte(chromeCipherPrefix), wantErr: "AES blocks",
		},
		{
			name: "host hash present but value empty", secret: secret, host: host,
			blob: sealChromeCookie(t, secret, host, ""), wantErr: "empty",
		},
		{
			name: "value carrying a cookie-invalid character", secret: secret, host: host,
			blob: sealChromeCookie(t, secret, host, "abc;def"), wantErr: "invalid in a cookie",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decryptChromeCookie(tt.secret, tt.blob, tt.host)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Empty(t, got, "no value may leak alongside an error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPickChromeSession(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	row := func(profile string, expires time.Time) chromeCookieRow {
		return chromeCookieRow{profile: profile, encrypted: []byte("x"), expires: expires}
	}
	future := now.Add(14 * 24 * time.Hour)
	nearer := now.Add(2 * 24 * time.Hour)
	past := now.Add(-time.Hour)

	tests := []struct {
		name        string
		rows        []chromeCookieRow
		scope       string
		wantProfile string
		wantErr     string
	}{
		{name: "no candidates", wantErr: "no github.com user_session cookie in any Chrome profile"},
		{
			// An explicit --chrome-profile must not be reported as though every
			// profile had been searched.
			name:    "named profile scope is reported verbatim",
			scope:   `Chrome profile "Profile 1"`,
			wantErr: `no github.com user_session cookie in Chrome profile "Profile 1"`,
		},
		{
			name: "single live profile",
			rows: []chromeCookieRow{row("Default", future)}, wantProfile: "Default",
		},
		{
			// Several signed-in profiles must converge on the freshest session,
			// not on whichever the directory scan yielded first.
			name:        "prefers the latest expiry regardless of scan order",
			rows:        []chromeCookieRow{row("Profile 1", nearer), row("Default", future)},
			wantProfile: "Default",
		},
		{
			name:        "skips expired profiles",
			rows:        []chromeCookieRow{row("Default", past), row("Profile 3", nearer)},
			wantProfile: "Profile 3",
		},
		{
			name:    "all expired reports the newest expiry",
			rows:    []chromeCookieRow{row("Default", past), row("Profile 1", now.Add(-48*time.Hour))},
			wantErr: "every github.com session in any Chrome profile is expired",
		},
		{
			// A cookie with no stored expiry is usable, but a dated one is the
			// stronger signal, so it must not outrank it.
			name:        "dated cookie outranks an undated one",
			rows:        []chromeCookieRow{row("Guest Profile", time.Time{}), row("Default", nearer)},
			wantProfile: "Default",
		},
		{
			name: "undated cookie is still usable alone",
			rows: []chromeCookieRow{row("Guest Profile", time.Time{})}, wantProfile: "Guest Profile",
		},
		{
			// Boundary: expiry exactly at now is spent, not live.
			name: "expiry exactly at now counts as expired",
			rows: []chromeCookieRow{row("Default", now)}, wantErr: "is expired",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scope := tt.scope
			if scope == "" {
				scope = "any Chrome profile"
			}
			got, err := pickChromeSession(tt.rows, now, scope)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantProfile, got.profile)
		})
	}
}

func TestParseChromeSessionRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		out         string
		wantFound   bool
		wantErr     bool
		wantValue   []byte
		wantExpires time.Time
	}{
		{
			name: "hex value and expiry", out: "76313041|13400000000000000\n",
			wantFound: true, wantValue: []byte("v10A"),
			wantExpires: time.Unix(13400000000000000/1_000_000-11644473600, 0).UTC(),
		},
		{
			// A profile with no GitHub cookie is normal, not an error.
			name: "no rows", out: "\n", wantFound: false,
		},
		{
			name: "zero expiry means no stored expiry", out: "76313041|0",
			wantFound: true, wantValue: []byte("v10A"), wantExpires: time.Time{},
		},
		{name: "missing separator", out: "76313041", wantErr: true},
		{name: "odd-length hex", out: "7631304|0", wantErr: true},
		{name: "non-numeric expiry", out: "76313041|soon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := parseChromeSessionRow(tt.out)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				assert.Equal(t, tt.wantValue, got.encrypted)
				assert.True(t, tt.wantExpires.Equal(got.expires), "expires: want %s, got %s", tt.wantExpires, got.expires)
			}
		})
	}
}

func TestTrimPKCS7(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      []byte
		want    []byte
		wantErr bool
	}{
		{name: "full block of padding", in: bytes.Repeat([]byte{16}, 16), want: []byte{}},
		{
			name: "partial padding",
			in:   append(bytes.Repeat([]byte{'a'}, 13), 3, 3, 3), want: bytes.Repeat([]byte{'a'}, 13),
		},
		{name: "zero padding byte", in: bytes.Repeat([]byte{0}, 16), wantErr: true},
		{name: "padding longer than block", in: bytes.Repeat([]byte{17}, 16), wantErr: true},
		{name: "inconsistent padding", in: append(bytes.Repeat([]byte{'a'}, 13), 3, 2, 3), wantErr: true},
		{name: "not block aligned", in: []byte{1, 2, 3}, wantErr: true},
		{name: "empty", in: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := trimPKCS7(tt.in, aes.BlockSize)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
