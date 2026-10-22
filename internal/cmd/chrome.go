// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"

	// SHA-1 is required here: Chrome derives its macOS cookie key with
	// PBKDF2-SHA1, so reading the cookies back needs the same hash.
	"crypto/sha1" //nolint:gosec // G505: Chrome's cookie key derivation fixes SHA-1
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Chrome's macOS cookie encryption, fixed by the browser's own implementation:
// PBKDF2-SHA1 over the "Chrome Safe Storage" Keychain secret, then
// AES-128-CBC under an all-spaces IV. These must match Chrome byte for byte —
// they are not tunable parameters.
const (
	chromeKDFSalt         = "saltysalt"
	chromeKDFIterations   = 1003
	chromeKDFKeyLen       = 16
	chromeKeychainService = "Chrome Safe Storage"
	chromeKeychainAccount = "Chrome"

	// chromeCipherPrefix marks a value encrypted with the Keychain-derived
	// key. Chrome's app-bound variant writes "v20", readable only inside the
	// browser process.
	chromeCipherPrefix = "v10"

	// Chrome stores timestamps as microseconds since 1601-01-01; converting to
	// Unix seconds means dividing by chromeEpochMicrosPerSecond and shifting by
	// chromeEpochOffsetSeconds.
	chromeEpochOffsetSeconds   = 11644473600
	chromeEpochMicrosPerSecond = 1_000_000

	// chromeCookieHost is the cookie host to read, and doubles as the domain
	// whose hash the plaintext must carry.
	chromeCookieHost = "github.com"
)

// Absolute paths, not PATH lookups: both subprocesses sit on the path of a
// password-equivalent credential, so neither may resolve to something earlier
// in a caller's PATH. Both ship with macOS.
const (
	securityBin = "/usr/bin/security"
	sqlite3Bin  = "/usr/bin/sqlite3"
)

// chromeCookieRow is one profile's stored user_session, still encrypted.
type chromeCookieRow struct {
	profile   string
	encrypted []byte
	// expires is the cookie's expiry, or the zero Time when Chrome stored none
	// (a browser-session cookie).
	expires time.Time
}

// readKeychainSecret is the keychain read chromeUserSession makes. It is a
// variable only so that tests can drive chromeUserSession without the real
// keychain.
var readKeychainSecret = chromeKeychainSecret

// readChromeSession is the Chrome reader the login and upload commands call.
// Tests replace it, so that no test can reach the real keychain.
var readChromeSession = chromeUserSession

// chromeUserSession returns github.com's user_session value from Chrome's
// cookie store. profile selects one Chrome profile directory by name
// ("Default", "Profile 1"); empty scans every profile and takes the unexpired
// session with the latest expiry.
//
// Every step below is portable Go, so this file carries no build constraint and
// stays type-checked and tested on every platform; what is macOS-specific is
// the cookie-protection scheme itself — the Keychain-derived key, the
// `security` binary, and the profile paths — hence the runtime guard rather
// than a per-GOOS stub.
func chromeUserSession(profile string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("reading cookies from Chrome is supported on macOS only — Chrome protects them with gnome-keyring/kwallet on Linux and DPAPI on Windows; use: ghattach login --cookie -")
	}
	userDataDir, err := chromeUserDataDir()
	if err != nil {
		return "", fmt.Errorf("locating Chrome's user data dir: %w", err)
	}
	rows, err := readChromeSessionRows(userDataDir, profile)
	if err != nil {
		return "", fmt.Errorf("reading Chrome's cookie stores: %w", err)
	}
	scope := "any Chrome profile"
	if profile != "" {
		scope = fmt.Sprintf("Chrome profile %q", profile)
	}
	row, err := pickChromeSession(rows, time.Now(), scope)
	if err != nil {
		return "", fmt.Errorf("selecting a Chrome profile: %w", err)
	}
	secret, err := readKeychainSecret()
	if err != nil {
		return "", fmt.Errorf("unlocking Chrome's cookie key: %w", err)
	}
	value, err := decryptChromeCookie(secret, row.encrypted, chromeCookieHost)
	if err != nil {
		return "", fmt.Errorf("decrypting the %q profile's session: %w", row.profile, err)
	}
	return value, nil
}

func chromeUserDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "Google", "Chrome"), nil
}

// readChromeSessionRows collects the github.com user_session from each Chrome
// profile that has one. A profile whose store cannot be read is skipped rather
// than fatal — one broken profile must not mask a working sibling — but when
// no profile yields a session and at least one failed, the joined failures
// surface.
func readChromeSessionRows(userDataDir, profile string) ([]chromeCookieRow, error) {
	glob := filepath.Join(userDataDir, "*", "Cookies")
	if profile != "" {
		glob = filepath.Join(userDataDir, profile, "Cookies")
	}
	stores, err := filepath.Glob(glob)
	if err != nil {
		return nil, fmt.Errorf("scanning Chrome profiles under %s: %w", userDataDir, err)
	}
	if len(stores) == 0 {
		if profile != "" {
			return nil, fmt.Errorf("no cookie store for Chrome profile %q under %s", profile, userDataDir)
		}
		return nil, fmt.Errorf("no Chrome cookie store under %s — is Chrome installed and signed in?", userDataDir)
	}

	// The copies below hold every cookie for every site, not just GitHub's, so
	// they live in a 0700 temp dir that is removed before returning.
	tmpDir, err := os.MkdirTemp("", "ghattach-chrome-")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir for the cookie-store copy: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup of our own temp dir

	var rows []chromeCookieRow
	var readErrs []error
	for _, store := range stores {
		row, found, err := readChromeSessionRow(store, tmpDir)
		switch {
		case err != nil:
			readErrs = append(readErrs, err)
		case found:
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 && len(readErrs) > 0 {
		return nil, fmt.Errorf("reading Chrome cookie stores: %w", errors.Join(readErrs...))
	}
	return rows, nil
}

// readChromeSessionRow reads one profile's store. Chrome keeps the database
// open, so the file is copied before querying — along with any rollback
// journal, without which SQLite could not replay a write that was in flight at
// copy time.
func readChromeSessionRow(storePath, tmpDir string) (chromeCookieRow, bool, error) {
	profile := filepath.Base(filepath.Dir(storePath))
	dbCopy := filepath.Join(tmpDir, profile+".db")
	if err := copyFile(storePath, dbCopy); err != nil {
		return chromeCookieRow{}, false, fmt.Errorf("copying the %q cookie store: %w", profile, err)
	}
	if err := copyFile(storePath+"-journal", dbCopy+"-journal"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return chromeCookieRow{}, false, fmt.Errorf("copying the %q rollback journal: %w", profile, err)
	}
	// hex() keeps the ciphertext binary-safe across the pipe; what crosses is
	// still encrypted, so no plaintext credential transits a subprocess.
	out, err := runSQLite(dbCopy, "SELECT hex(encrypted_value), expires_utc FROM cookies "+
		"WHERE host_key = '"+chromeCookieHost+"' AND name = '"+userSessionCookie+"';")
	if err != nil {
		return chromeCookieRow{}, false, fmt.Errorf("querying the %q cookie store: %w", profile, err)
	}
	row, found, err := parseChromeSessionRow(out)
	if err != nil {
		return chromeCookieRow{}, false, fmt.Errorf("reading the %q profile's session: %w", profile, err)
	}
	row.profile = profile
	return row, found, nil
}

// parseChromeSessionRow parses sqlite3's default list output,
// "<hex>|<expires_utc>". An empty result is not an error: a profile simply may
// not be signed in. The returned row carries no profile name — the caller,
// which knows it, sets it.
func parseChromeSessionRow(out string) (chromeCookieRow, bool, error) {
	line := strings.TrimSpace(out)
	if line == "" {
		return chromeCookieRow{}, false, nil
	}
	// A unique index on (host_key, name, path) makes duplicates impossible;
	// taking the first line defensively costs nothing.
	line, _, _ = strings.Cut(line, "\n")
	hexValue, expiresRaw, ok := strings.Cut(line, "|")
	if !ok {
		return chromeCookieRow{}, false, fmt.Errorf("unexpected sqlite output %q", line)
	}
	encrypted, err := hex.DecodeString(hexValue)
	if err != nil {
		return chromeCookieRow{}, false, fmt.Errorf("decoding the encrypted value: %w", err)
	}
	micros, err := strconv.ParseInt(strings.TrimSpace(expiresRaw), 10, 64)
	if err != nil {
		return chromeCookieRow{}, false, fmt.Errorf("parsing expiry %q: %w", expiresRaw, err)
	}
	return chromeCookieRow{encrypted: encrypted, expires: chromeEpochTime(micros)}, true, nil
}

// chromeEpochTime converts Chrome's microseconds-since-1601 stamp to a Time.
// Zero means no stored expiry and maps to the zero Time.
func chromeEpochTime(micros int64) time.Time {
	if micros == 0 {
		return time.Time{}
	}
	return time.Unix(micros/chromeEpochMicrosPerSecond-chromeEpochOffsetSeconds, 0).UTC()
}

// pickChromeSession chooses which profile's session to install: the unexpired
// candidate with the latest expiry, so a machine with several signed-in
// profiles converges on the freshest session instead of whichever profile the
// directory scan happened to yield first. A candidate with no stored expiry is
// valid but sorts last, a dated cookie being the stronger signal. scope names
// the search space in error messages ("any Chrome profile", or one named
// profile), so a failure cannot imply a wider search than actually ran.
func pickChromeSession(rows []chromeCookieRow, now time.Time, scope string) (chromeCookieRow, error) {
	if len(rows) == 0 {
		return chromeCookieRow{}, fmt.Errorf("no %s %s cookie in %s — sign in to %s in Chrome, then retry",
			chromeCookieHost, userSessionCookie, scope, chromeCookieHost)
	}
	var live []chromeCookieRow
	var newestExpired time.Time
	for _, row := range rows {
		if !row.expires.IsZero() && !row.expires.After(now) {
			if row.expires.After(newestExpired) {
				newestExpired = row.expires
			}
			continue
		}
		live = append(live, row)
	}
	if len(live) == 0 {
		return chromeCookieRow{}, fmt.Errorf("every %s session in %s is expired (newest expired %s) — sign in to %s in Chrome, then retry",
			chromeCookieHost, scope, newestExpired.Format(time.RFC3339), chromeCookieHost)
	}
	slices.SortStableFunc(live, func(a, b chromeCookieRow) int { return b.expires.Compare(a.expires) })
	return live[0], nil
}

// decryptChromeCookie decrypts one Chrome cookie value. hostKey is the
// cookie's host, whose SHA-256 Chrome prepends to the plaintext; checking it
// turns a wrong Keychain secret or a changed Chrome format into a clear error
// rather than a silently-corrupt cookie that would later look like an
// inexplicable auth failure.
func decryptChromeCookie(secret string, blob []byte, hostKey string) (string, error) {
	if !bytes.HasPrefix(blob, []byte(chromeCipherPrefix)) {
		return "", fmt.Errorf("value carries version tag %q, not %q — this profile has likely moved to Chrome's app-bound encryption, which only the browser can read; use: ghattach login --cookie -",
			versionTag(blob), chromeCipherPrefix)
	}
	ciphertext := blob[len(chromeCipherPrefix):]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("ciphertext is %d bytes, not a whole number of %d-byte AES blocks", len(ciphertext), aes.BlockSize)
	}
	key, err := pbkdf2.Key(sha1.New, secret, []byte(chromeKDFSalt), chromeKDFIterations, chromeKDFKeyLen)
	if err != nil {
		return "", fmt.Errorf("deriving the cookie key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("initializing AES: %w", err)
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(plain, ciphertext)
	plain, err = trimPKCS7(plain, aes.BlockSize)
	if err != nil {
		return "", fmt.Errorf("%w — wrong Keychain secret?", err)
	}
	wantPrefix := sha256.Sum256([]byte(hostKey))
	if len(plain) < len(wantPrefix) || !bytes.Equal(plain[:len(wantPrefix)], wantPrefix[:]) {
		return "", fmt.Errorf("decrypted value does not carry the expected %s host hash — wrong Keychain secret, or Chrome changed its cookie layout; use: ghattach login --cookie -", hostKey)
	}
	value := string(plain[len(wantPrefix):])
	if err := validateSessionValue(value); err != nil {
		return "", fmt.Errorf("decrypt produced an unusable value: %w", err)
	}
	return value, nil
}

// validateSessionValue rejects what net/http cannot send unchanged in a
// cookie: control characters, spaces, non-ASCII bytes, '\' and the separators
// ';', ',' and '"'. It guards both a pasted value and a decrypt under the
// wrong key, which can still produce well-padded bytes.
func validateSessionValue(value string) error {
	if value == "" {
		return fmt.Errorf("session value is empty")
	}
	for _, r := range value {
		if r <= ' ' || r > '~' || r == ';' || r == ',' || r == '"' || r == '\\' {
			return fmt.Errorf("session value contains %q, which is invalid in a cookie", r)
		}
	}
	return nil
}

// trimPKCS7 removes and validates PKCS#7 padding.
func trimPKCS7(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 || len(b)%blockSize != 0 {
		return nil, fmt.Errorf("padded plaintext is %d bytes, not a whole number of %d-byte blocks", len(b), blockSize)
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > blockSize || pad > len(b) {
		return nil, fmt.Errorf("invalid PKCS#7 padding length %d", pad)
	}
	for _, c := range b[len(b)-pad:] {
		if int(c) != pad {
			return nil, fmt.Errorf("inconsistent PKCS#7 padding")
		}
	}
	return b[:len(b)-pad], nil
}

// versionTag renders a blob's leading version marker for an error message,
// without echoing ciphertext.
func versionTag(b []byte) string {
	if len(b) > len(chromeCipherPrefix) {
		b = b[:len(chromeCipherPrefix)]
	}
	return string(b)
}

// chromeKeychainSecret reads the Chrome Safe Storage secret from the login
// keychain. Prefer one-time "Allow" in the macOS prompt: persistent access
// grants /usr/bin/security the key for Chrome cookies across sites. The timeout
// bounds a human answering that dialog.
func chromeKeychainSecret() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityBin, // fixed absolute binary, fixed argv, no shell
		"find-generic-password", "-w", "-s", chromeKeychainService, "-a", chromeKeychainAccount)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("reading the %q keychain item — a denied prompt and a missing item both land here: %s", chromeKeychainService, detail)
	}
	return parseKeychainOutput(out)
}

// parseKeychainOutput turns `security find-generic-password -w` output into the
// secret: the trailing newline is dropped, and an empty item is an error.
func parseKeychainOutput(out []byte) (string, error) {
	secret := strings.TrimRight(string(out), "\n")
	if secret == "" {
		return "", fmt.Errorf("the %q keychain item is empty", chromeKeychainService)
	}
	return secret, nil
}

func runSQLite(dbPath, query string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Deliberately not --readonly: the copy may carry a hot rollback journal,
	// and replaying that needs write access to our own temp file.
	// A fixed absolute binary; dbPath is our temp copy and query is a
	// compile-time constant.
	cmd := exec.CommandContext(ctx, sqlite3Bin, dbPath, query) //nolint:gosec // G204, see above
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("sqlite3: %s", detail)
	}
	return string(out), nil
}

// copyFile copies src to dst at 0600. A missing src is reported as
// os.ErrNotExist so callers can treat an absent optional file as normal.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // path is derived from Chrome's own profile layout
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close() //nolint:errcheck // read-only file

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // path is our own temp dir
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck,gosec // best-effort cleanup on the error path
		return fmt.Errorf("copying %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dst, err)
	}
	return nil
}
