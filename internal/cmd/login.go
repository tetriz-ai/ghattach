// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const toolName = "ghattach"

var (
	loginCookie        string
	loginFromChrome    bool
	loginChromeProfile string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Store the GitHub user_session cookie",
	Long: `Store the GitHub user_session cookie that uploads use.

Uploads use your GitHub browser session. The upload endpoint rejects
personal access tokens and app tokens.

On macOS, read the cookie from a signed-in Chrome:

  ghattach login --from-chrome

macOS asks for access to the "Chrome Safe Storage" keychain item. Choose
"Allow" for this request and approve again when prompted. "Always Allow"
gives /usr/bin/security standing access to the key for Chrome cookies
across sites, which other local processes can use. --chrome-profile picks
one profile when several are signed in, and pins it for later re-reads.

Otherwise, paste the value:

  1. Open github.com in a signed-in browser tab
  2. DevTools (F12, or Cmd+Option+I on macOS) > Application > Cookies >
     https://github.com
  3. Copy the VALUE of the "user_session" cookie
  4. ghattach login --cookie -        (paste at the prompt, then Enter)

Paste only the value at a terminal prompt. To pass a whole Cookie header,
pipe it instead, such as pbpaste on macOS or xclip -o -selection clipboard
on Linux, into ghattach login --cookie -

'--cookie -' keeps the value out of shell history and ps output.
'--cookie <value>' does not.

A user_session cookie is full-account and unscoped. Treat it like a
password. Revoke any session at https://github.com/settings/sessions.`,
	// An argument here is most likely a cookie value typed without --cookie,
	// so the error never echoes it.
	Args: func(_ *cobra.Command, args []string) error {
		if len(args) > 0 {
			return errors.New("login takes no arguments; pass the value with --cookie -")
		}
		return nil
	},
	RunE: runLogin,
}

func init() {
	loginCmd.Flags().StringVar(&loginCookie, "cookie", "", "the user_session value or full Cookie header, or - to read it from standard input")
	loginCmd.Flags().BoolVar(&loginFromChrome, "from-chrome", false, "read user_session from Chrome's cookie store (macOS)")
	loginCmd.Flags().StringVar(&loginChromeProfile, "chrome-profile", "", "Chrome profile directory or file-name pattern to read and pin, such as Default (default: the profile whose session expires last)")
	loginCmd.MarkFlagsMutuallyExclusive("cookie", "from-chrome")
	rootCmd.AddCommand(loginCmd)
	rootCmd.AddCommand(logoutCmd)
}

var logoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Delete the stored session cookie",
	Long: `Delete the stored session cookie from the local cookie store.

This only removes the local copy — the GitHub session itself stays valid
until it expires or is revoked at https://github.com/settings/sessions.`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		path, err := cookiePath()
		if err != nil {
			return fmt.Errorf("locating cookie store: %w", err)
		}
		removeTempStores(path, 0)
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				fmt.Println("No stored session.")
				return nil
			}
			return fmt.Errorf("removing cookie store: %w", err)
		}
		fmt.Printf("Removed %s\n", path)
		fmt.Println("Also revoke the browser session if it is no longer wanted: https://github.com/settings/sessions")
		return nil
	},
}

// staleTempAge is how old a cookies-*.json must be before a write sweeps it.
// A younger one may belong to another ghattach run that is about to rename
// it into place.
const staleTempAge = time.Minute

// removeTempStores best-effort deletes cookies-*.json siblings of the store
// older than minAge — leftovers from a write interrupted between CreateTemp
// and Rename, each holding a full copy of the secret.
func removeTempStores(storePath string, minAge time.Duration) {
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(storePath), "cookies-*.json"))
	if err != nil { // best-effort sweep; Glob errors only on a malformed pattern, which is fixed here
		return
	}
	for _, m := range matches {
		if info, err := os.Stat(m); err != nil || time.Since(info.ModTime()) < minAge {
			continue
		}
		_ = os.Remove(m) //nolint:errcheck // best-effort sweep
	}
}

// cookiePath fails hard when the home directory cannot be resolved: degrading
// to a cwd-relative path would scatter a password-equivalent credential into
// whatever directory the tool happens to run in.
func cookiePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir for the cookie store: %w", err)
	}
	return filepath.Join(home, ".config", toolName, "cookies.json"), nil
}

type cookieStore struct {
	UserSession string `json:"user_session"`
	// ChromeProfile records an explicitly pinned --chrome-profile, empty when
	// the profile was auto-selected or the cookie was pasted. Only a pin is
	// recorded, never the auto-selected result: re-reading must honour a
	// user's choice of account, but must stay free to re-pick the freshest
	// profile when no choice was expressed.
	ChromeProfile string `json:"chrome_profile,omitempty"`
}

// extractUserSession accepts either a bare user_session value or a Cookie
// header, with or without its "Cookie:" name, and returns the user_session
// value. The input is a header when it carries a cookie separator or a
// name=value pair — a bare value whose only '=' is base64 padding at the end
// passes through verbatim. Fails on an empty or unparseable input, and on a
// value net/http would rewrite on the wire, rather than storing something the
// endpoint will reject.
func extractUserSession(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if name, rest, ok := strings.Cut(raw, ":"); ok && strings.EqualFold(name, "cookie") {
		raw = strings.TrimSpace(rest)
	}
	if raw == "" {
		return "", errors.New("empty cookie")
	}
	value := raw
	if _, v, ok := strings.Cut(raw, "="); strings.Contains(raw, ";") || (ok && strings.Trim(v, "=") != "") {
		var err error
		if value, err = userSessionFromHeader(raw); err != nil {
			return "", err
		}
	}
	if err := validateSessionValue(value); err != nil {
		return "", err
	}
	return value, nil
}

// userSessionFromHeader returns the user_session value from a Cookie header.
func userSessionFromHeader(header string) (string, error) {
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) == userSessionCookie {
			if v = strings.TrimSpace(v); v == "" {
				return "", errors.New("user_session cookie is empty")
			}
			return v, nil
		}
	}
	return "", errors.New("no user_session cookie found in input — copy its value from DevTools > Application > Cookies")
}

func loadStore() (cookieStore, error) {
	path, err := cookiePath()
	if err != nil {
		return cookieStore{}, fmt.Errorf("locating cookie store: %w", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed path under the user's own config dir
	if err != nil {
		if os.IsNotExist(err) {
			return cookieStore{}, fmt.Errorf("not logged in — run: ghattach login --from-chrome (macOS) or: ghattach login --cookie -")
		}
		return cookieStore{}, fmt.Errorf("reading cookie store: %w", err)
	}
	var store cookieStore
	if err := json.Unmarshal(data, &store); err != nil {
		return cookieStore{}, fmt.Errorf("unmarshaling cookie store: %w", err)
	}
	if store.UserSession == "" {
		return cookieStore{}, fmt.Errorf("cookie store has no session — run: ghattach login")
	}
	if err := validateSessionValue(store.UserSession); err != nil {
		return cookieStore{}, fmt.Errorf("cookie store holds an unusable session — run: ghattach login: %w", err)
	}
	return store, nil
}

// readCookieInput resolves the --cookie flag: "-" reads from stdin so the
// secret stays out of shell history and ps output — no-echo when stdin is a
// terminal (the paste never renders on screen), a plain line read when piped.
// stdinIsTerminal and readTerminalLine are the terminal checks readCookieInput
// makes. They are variables only so that tests can drive the terminal branch.
var (
	stdinIsTerminal  = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	readTerminalLine = func() ([]byte, error) { return term.ReadPassword(int(os.Stdin.Fd())) }
)

func readCookieInput() (string, error) {
	if loginCookie != "-" {
		return loginCookie, nil
	}
	fmt.Fprint(os.Stderr, "Paste the user_session VALUE, then Enter: ")
	if stdinIsTerminal() {
		raw, err := readTerminalLine()
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("reading cookie from terminal: %w", err)
		}
		// ReadPassword reads in canonical mode, and the kernel's line cap
		// (MAX_CANON, 1024 bytes on macOS) drops input past it, the Enter
		// included, so a longer paste never returns. A paste that fits but is
		// near the cap is refused rather than stored, since it is likely cut
		// short and would surface later as a bogus "expired session". The
		// bare value (~48 chars) always fits; a full header goes through the
		// piped path, which has no cap.
		if len(raw) >= 1000 {
			return "", fmt.Errorf("terminal paste near the 1024-byte line cap — likely truncated; paste only the user_session value, or pipe the header: pbpaste | ghattach login --cookie -")
		}
		return string(raw), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading cookie from stdin: %w", err)
	}
	return line, nil
}

func runLogin(cmd *cobra.Command, _ []string) error {
	// A pin belongs only to a session read from Chrome.
	if cmd.Flags().Changed("chrome-profile") && !loginFromChrome {
		return errors.New("--chrome-profile needs --from-chrome")
	}
	var session string
	switch {
	case loginFromChrome:
		var err error
		session, err = readChromeSession(loginChromeProfile)
		if err != nil {
			return fmt.Errorf("reading the session from Chrome: %w", err)
		}
	case !cmd.Flags().Changed("cookie"):
		fmt.Println("On macOS, read the cookie straight out of a signed-in Chrome:")
		fmt.Println("\n  ghattach login --from-chrome")
		fmt.Println("\nOr copy the user_session value from a logged-in github.com tab")
		fmt.Println("(DevTools > Application > Cookies > https://github.com), then run:")
		fmt.Println("\n  ghattach login --cookie -")
		return nil
	default:
		raw, err := readCookieInput()
		if err != nil {
			return fmt.Errorf("reading cookie input: %w", err)
		}
		session, err = extractUserSession(raw)
		if err != nil {
			return fmt.Errorf("parsing cookie input: %w", err)
		}
	}
	// A pasted cookie has no Chrome origin to remember, so the empty pin
	// clears any stale one rather than carrying it onto this session.
	path, err := storeSession(session, loginChromeProfile)
	if err != nil {
		return fmt.Errorf("storing the session: %w", err)
	}
	fmt.Printf("Session stored at %s\n", path)
	fmt.Println("Verify with: ghattach upload <file> --repo OWNER/REPO")
	return nil
}

// storeSession writes the cookie store atomically and returns its path, so a
// concurrent reader sees either the old store or the new one, never a partial
// write. chromeProfile is the pin to remember (empty for none) and always
// replaces any previously stored value.
func storeSession(session, chromeProfile string) (string, error) {
	path, err := cookiePath()
	if err != nil {
		return "", fmt.Errorf("locating cookie store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("creating cookie store dir: %w", err)
	}
	// MkdirAll leaves an existing directory's mode alone.
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // G302: a directory needs its search bit; 0700 is owner-only
		return "", fmt.Errorf("restricting cookie store dir: %w", err)
	}
	data, err := json.MarshalIndent(cookieStore{UserSession: session, ChromeProfile: chromeProfile}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling cookie store: %w", err)
	}
	// Sweep any cookies-*.json left by an earlier crashed write — a stale
	// temp copy of the secret must not outlive the store it was meant for.
	removeTempStores(path, staleTempAge)
	// Temp-file + rename so a concurrent reader sees the old or new store,
	// never a partial one. CreateTemp always creates a fresh 0600 file and
	// Rename replaces the destination inode wholesale; the explicit Chmod
	// below is deliberate redundancy, not a correctness requirement.
	tmp, err := os.CreateTemp(filepath.Dir(path), "cookies-*.json")
	if err != nil {
		return "", fmt.Errorf("creating temp cookie store: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()        //nolint:errcheck // best-effort cleanup
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("writing cookie store: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()        //nolint:errcheck // best-effort cleanup
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("chmod cookie store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("closing cookie store: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) //nolint:errcheck // best-effort cleanup
		return "", fmt.Errorf("installing cookie store: %w", err)
	}
	return path, nil
}
