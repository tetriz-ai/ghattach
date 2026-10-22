// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file drive the real cobra commands in-process. They swap
// os.Stdin, os.Stdout, os.Stderr, HOME and package variables, so none of them
// may call t.Parallel.

// cliResult is what one CLI invocation printed and returned.
type cliResult struct {
	stdout string
	stderr string
	err    error
}

// resetFlags returns every flag of c and its subcommands to its default, so
// one invocation's flags cannot leak into the next.
func resetFlags(c *cobra.Command) {
	c.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue) //nolint:errcheck // defaults always parse
		f.Changed = false
	})
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

// runCLI runs ghattach with args, feeding stdin, and captures what it prints.
func runCLI(t *testing.T, stdin string, args ...string) cliResult {
	t.Helper()
	resetFlags(rootCmd)
	rootCmd.Version = "v0.0.0-test"
	rootCmd.SetArgs(args)

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	_, err = io.WriteString(inW, stdin)
	require.NoError(t, err)
	require.NoError(t, inW.Close())
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	errR, errW, err := os.Pipe()
	require.NoError(t, err)

	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = inR, outW, errW
	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&outBuf, outR) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&errBuf, errR) }()

	runErr := rootCmd.Execute()

	os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	require.NoError(t, outW.Close())
	require.NoError(t, errW.Close())
	wg.Wait()
	_ = inR.Close()
	return cliResult{stdout: outBuf.String(), stderr: errBuf.String(), err: runErr}
}

// cliStub is a stand-in for github.com that accepts only the sessions in
// live. A request with any other session gets deadStatus (with deadLocation
// as its Location) on the repo page, the way GitHub treats a dead session.
type cliStub struct {
	srv *httptest.Server

	mu           sync.Mutex
	live         map[string]bool
	deadStatus   int
	deadLocation string
	repoPaths    []string // repo pages requested
	sessions     []string // user_session of each repo-page request
	repoIDs      []string // repository_id of each policy request
	s3Cookies    int      // S3 requests that carried any cookie
	policy401At  int      // from this policy request on (1-based), answer 401; 0 never
	policy401Nth int      // only this policy request (1-based) answers 401; 0 never
	policyCalls  int
	names        map[int]string
	nextID       int
}

func newCLIStub(t *testing.T, liveSessions ...string) *cliStub {
	t.Helper()
	s := &cliStub{
		live:         map[string]bool{},
		deadStatus:   http.StatusFound,
		deadLocation: "/login?return_to=%2Facme%2Fwidgets",
		names:        map[int]string{},
	}
	for _, v := range liveSessions {
		s.live[v] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.repoPaths = append(s.repoPaths, r.URL.Path)
		c, err := r.Cookie(userSessionCookie)
		session := ""
		if err == nil {
			session = c.Value
		}
		s.sessions = append(s.sessions, session)
		if r.URL.Path != "/acme/widgets" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !s.live[session] {
			if s.deadLocation != "" {
				w.Header().Set("Location", s.deadLocation)
			}
			w.WriteHeader(s.deadStatus)
			return
		}
		_, _ = io.WriteString(w, `<html>{"uploadToken":"tok-cli"}</html>`)
	})
	mux.HandleFunc("POST /upload/policies/assets", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: test stub, bounded at 1 MiB
			t.Errorf("parsing policy form: %v", err)
			return
		}
		s.mu.Lock()
		s.policyCalls++
		if (s.policy401At > 0 && s.policyCalls >= s.policy401At) || s.policyCalls == s.policy401Nth {
			s.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.nextID++
		id := s.nextID
		s.names[id] = r.FormValue("name")
		s.repoIDs = append(s.repoIDs, r.FormValue("repository_id"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upload_url":                      s.srv.URL + "/s3",
			"asset":                           map[string]any{"id": id, "content_type": r.FormValue("content_type")},
			"form":                            map[string]string{"key": fmt.Sprintf("assets/%d", id)},
			"asset_upload_url":                fmt.Sprintf("/upload/assets/%d", id),
			"asset_upload_authenticity_token": "fin-tok",
		})
	})
	mux.HandleFunc("POST /s3", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if r.Header.Get("Cookie") != "" {
			s.s3Cookies++
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /upload/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		var id int
		_, _ = fmt.Sscanf(r.PathValue("id"), "%d", &id)
		s.mu.Lock()
		name := s.names[id]
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"href": assetURL(id), "name": name})
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func assetURL(id int) string {
	return fmt.Sprintf("https://github.com/user-attachments/assets/00000000-0000-4000-8000-%012d", id)
}

// useStub points the CLI at s for the rest of the test.
func useStub(t *testing.T, s *cliStub) {
	t.Helper()
	old := githubBaseURL
	githubBaseURL = s.srv.URL
	t.Cleanup(func() { githubBaseURL = old })
}

// fakeChrome replaces the Chrome reader for the rest of the test. It returns
// session or err, and records each profile it was asked for. A test that does
// not install one gets the reader TestMain installs, which panics.
type fakeChrome struct {
	session  string
	err      error
	profiles []string
}

func useFakeChrome(t *testing.T, f *fakeChrome) {
	t.Helper()
	old := readChromeSession
	readChromeSession = func(profile string) (string, error) {
		f.profiles = append(f.profiles, profile)
		return f.session, f.err
	}
	t.Cleanup(func() { readChromeSession = old })
}

// tempHome gives the test an empty home directory, so the cookie store is
// the test's own.
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func storePath(home string) string {
	return filepath.Join(home, ".config", "ghattach", "cookies.json")
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func readStore(t *testing.T, home string) cookieStore {
	t.Helper()
	data, err := os.ReadFile(storePath(home))
	require.NoError(t, err)
	var s cookieStore
	require.NoError(t, json.Unmarshal(data, &s))
	return s
}

func TestCookiePathIsUnderConfigGhattach(t *testing.T) {
	home := tempHome(t)
	got, err := cookiePath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".config", "ghattach", "cookies.json"), got)
}

func TestUploadCommand(t *testing.T) {
	tests := []struct {
		name       string
		args       func(dir string) []string
		loggedIn   bool
		wantStdout func(dir string) string
		wantErr    string
		check      func(t *testing.T, s *cliStub)
	}{
		{
			name: "prints the asset URL",
			args: func(d string) []string {
				return []string{"upload", filepath.Join(d, "shot.png"), "--repo", "acme/widgets", "--repo-id", "7"}
			},
			loggedIn:   true,
			wantStdout: func(string) string { return assetURL(1) + "\n" },
			check: func(t *testing.T, s *cliStub) {
				assert.Equal(t, []string{"7"}, s.repoIDs)
				assert.Equal(t, []string{"sess-live"}, s.sessions)
				assert.Zero(t, s.s3Cookies, "the S3 upload must carry no GitHub cookie")
			},
		},
		{
			name: "-R is the short form of --repo",
			args: func(d string) []string {
				return []string{"upload", filepath.Join(d, "shot.png"), "-R", "acme/widgets", "--repo-id", "7"}
			},
			loggedIn:   true,
			wantStdout: func(string) string { return assetURL(1) + "\n" },
		},
		{
			name: "--markdown prints the embed for each media type",
			args: func(d string) []string {
				return []string{"upload", filepath.Join(d, "shot.png"), filepath.Join(d, "demo.mp4"), filepath.Join(d, "notes.pdf"), "-R", "acme/widgets", "--repo-id", "7", "--markdown"}
			},
			loggedIn: true,
			wantStdout: func(string) string {
				return "![shot.png](" + assetURL(1) + ")\n" + assetURL(2) + "\n[notes.pdf](" + assetURL(3) + ")\n"
			},
			check: func(t *testing.T, s *cliStub) {
				assert.Len(t, s.repoPaths, 1, "one repo-page fetch serves every file")
			},
		},
		{
			// Every file is checked before the first upload, so a bad later
			// argument leaves nothing uploaded.
			name: "a missing later file fails before any upload",
			args: func(d string) []string {
				return []string{"upload", filepath.Join(d, "shot.png"), filepath.Join(d, "missing.png"), filepath.Join(d, "demo.mp4"), "-R", "acme/widgets", "--repo-id", "7"}
			},
			loggedIn: true,
			wantErr:  "missing.png: stat file",
			check: func(t *testing.T, s *cliStub) {
				assert.Empty(t, s.repoPaths, "no request is made")
				assert.Empty(t, s.repoIDs, "nothing is uploaded")
			},
		},
		{
			name: "not logged in",
			args: func(d string) []string {
				return []string{"upload", filepath.Join(d, "shot.png"), "-R", "acme/widgets", "--repo-id", "7"}
			},
			wantErr:  "loading session: not logged in — run: ghattach login --from-chrome (macOS) or: ghattach login --cookie -",
			loggedIn: false,
		},
		{
			name:    "--repo is required",
			args:    func(d string) []string { return []string{"upload", filepath.Join(d, "shot.png")} },
			wantErr: `required flag(s) "repo" not set`,
		},
		{
			name:    "a file argument is required",
			args:    func(string) []string { return []string{"upload", "-R", "acme/widgets"} },
			wantErr: "requires at least 1 arg(s), only received 0",
		},
		{name: "--repo without a slash", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "acme"} }, wantErr: `--repo must be OWNER/REPO, got "acme"`},
		{name: "--repo without an owner", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "/widgets"} }, wantErr: `--repo must be OWNER/REPO, got "/widgets"`},
		{name: "--repo without a repo", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "acme/"} }, wantErr: `--repo must be OWNER/REPO, got "acme/"`},
		{name: "--repo with a third segment", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "acme/widgets/extra"} }, wantErr: `--repo must be OWNER/REPO, got "acme/widgets/extra"`},
		{name: "--repo with a query", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "acme/widgets?x=1"} }, wantErr: `--repo must be OWNER/REPO, got "acme/widgets?x=1"`},
		{name: "--repo with a dot in the owner", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "a.b/widgets"} }, wantErr: `--repo must be OWNER/REPO, got "a.b/widgets"`},
		{name: "--repo with a dot-dot owner", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "../widgets"} }, wantErr: `--repo must be OWNER/REPO, got "../widgets"`},
		{name: "--repo-id -1", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "acme/widgets", "--repo-id", "-1"} }, wantErr: "--repo-id must be a positive repository id, got -1"},
		{
			name: "a missing file fails before any request",
			args: func(d string) []string {
				return []string{"upload", filepath.Join(d, "missing.png"), "-R", "acme/widgets", "--repo-id", "7"}
			},
			loggedIn: true,
			wantErr:  "stat file",
			check: func(t *testing.T, s *cliStub) {
				assert.Empty(t, s.repoPaths)
			},
		},
		{
			name: "a directory fails before any request",
			args: func(d string) []string {
				return []string{"upload", d, "-R", "acme/widgets", "--repo-id", "7"}
			},
			loggedIn: true,
			wantErr:  "is not a regular file",
			check: func(t *testing.T, s *cliStub) {
				assert.Empty(t, s.repoPaths)
			},
		},
		{name: "--repo with a dot-dot repo", args: func(_ string) []string { return []string{"upload", "x.png", "-R", "acme/.."} }, wantErr: `--repo must be OWNER/REPO, got "acme/.."`},
		{
			name: "--repo accepts dots, hyphens and underscores",
			args: func(d string) []string {
				return []string{"upload", filepath.Join(d, "shot.png"), "-R", "my-org/my_repo.go", "--repo-id", "7"}
			},
			loggedIn: true,
			wantErr:  "repo page returned 404 — no access to my-org/my_repo.go",
		},
		{
			name: "a negative --repo-id is refused",
			args: func(d string) []string {
				return []string{"upload", filepath.Join(d, "shot.png"), "-R", "acme/widgets", "--repo-id", "-5"}
			},
			loggedIn: true,
			wantErr:  "--repo-id must be a positive repository id, got -5",
			check: func(t *testing.T, s *cliStub) {
				assert.Empty(t, s.repoIDs)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := tempHome(t)
			dir := t.TempDir()
			writeFile(t, dir, "shot.png", "png-bytes")
			writeFile(t, dir, "demo.mp4", "mp4-bytes")
			writeFile(t, dir, "notes.pdf", "pdf-bytes")
			if tt.loggedIn {
				_, err := storeSession("sess-live", "")
				require.NoError(t, err)
			}
			s := newCLIStub(t, "sess-live")
			useStub(t, s)
			useFakeChrome(t, &fakeChrome{err: errors.New("Chrome must not be read in this test")})

			res := runCLI(t, "", tt.args(dir)...)
			want := ""
			if tt.wantStdout != nil {
				want = tt.wantStdout(dir)
			}
			assert.Equal(t, want, res.stdout)
			if tt.wantErr != "" {
				require.Error(t, res.err)
				assert.Contains(t, res.err.Error(), tt.wantErr)
			} else {
				require.NoError(t, res.err)
			}
			if tt.check != nil {
				tt.check(t, s)
			}
			_ = home
		})
	}
}

// TestUploadResolvesRepoIDWithGh pins the one official-API step: without
// --repo-id, the id comes from `gh api repos/<owner>/<repo> --jq .id`.
func TestUploadResolvesRepoIDWithGh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	tests := []struct {
		name    string
		script  string
		wantErr string
		wantID  string
	}{
		{name: "gh prints the id", script: "echo \"$@\" > \"$GH_ARGS\"\necho 4242\n", wantID: "4242"},
		{
			name:    "gh fails with a message",
			script:  "echo 'HTTP 404: Not Found' >&2\nexit 1\n",
			wantErr: "resolving repo: resolving repo id via gh (pass --repo-id to skip): HTTP 404: Not Found: exit status 1",
		},
		{
			// gh's stderr is joined onto one line.
			name:    "gh fails with a two-line message",
			script:  "printf 'To get started, please run:  gh auth login\\nAlternatively, set GH_TOKEN.\\n' >&2\nexit 4\n",
			wantErr: "resolving repo: resolving repo id via gh (pass --repo-id to skip): To get started, please run:  gh auth login Alternatively, set GH_TOKEN.: exit status 4",
		},
		{
			name:    "gh prints something that is not a number",
			script:  "echo null\n",
			wantErr: `resolving repo: unexpected gh api output "null": strconv.Atoi: parsing "null": invalid syntax`,
		},
		{name: "gh prints 0", script: "echo 0\n", wantErr: `resolving repo: unexpected gh api output "0": not a repository id`},
		{name: "gh prints a negative id", script: "echo -3\n", wantErr: `resolving repo: unexpected gh api output "-3": not a repository id`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempHome(t)
			_, err := storeSession("sess-live", "")
			require.NoError(t, err)
			bin := t.TempDir()
			writeFile(t, bin, "gh", "#!/bin/sh\n"+tt.script)
			require.NoError(t, os.Chmod(filepath.Join(bin, "gh"), 0o700)) //nolint:gosec // G302: the fake gh must be executable
			argsFile := filepath.Join(bin, "args")
			t.Setenv("GH_ARGS", argsFile)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			s := newCLIStub(t, "sess-live")
			useStub(t, s)
			useFakeChrome(t, &fakeChrome{err: errors.New("Chrome must not be read in this test")})
			shot := writeFile(t, t.TempDir(), "shot.png", "png")

			res := runCLI(t, "", "upload", shot, "-R", "acme/widgets")
			if tt.wantErr != "" {
				require.Error(t, res.err)
				assert.Equal(t, tt.wantErr, res.err.Error())
				assert.Empty(t, s.repoPaths, "nothing is requested from GitHub without a repo id")
				return
			}
			require.NoError(t, res.err)
			assert.Equal(t, []string{tt.wantID}, s.repoIDs)
			args, err := os.ReadFile(argsFile) //nolint:gosec // G304: the test's own temp file
			require.NoError(t, err)
			assert.Equal(t, "api repos/acme/widgets --jq .id\n", string(args))
		})
	}
}

// TestUploadSelfHeal covers the one Chrome re-read an upload may make when
// the stored session is dead.
func TestUploadSelfHeal(t *testing.T) {
	tests := []struct {
		name         string
		stored       string
		pin          string
		chrome       fakeChrome
		deadStatus   int
		deadLocation string
		noLocation   bool
		policy401At  int
		files        int
		wantStdout   int // URLs printed
		wantErr      string
		wantStderr   string
		wantProfiles []string
		wantStored   string
	}{
		{
			name:         "a dead session is replaced by Chrome's and the upload retried",
			stored:       "sess-dead",
			chrome:       fakeChrome{session: "sess-live"},
			files:        2,
			wantStdout:   2,
			wantStderr:   "Re-read the session from Chrome; retrying.\n",
			wantProfiles: []string{""},
			wantStored:   "sess-live",
		},
		{
			name:         "the re-read honours the stored --chrome-profile pin",
			stored:       "sess-dead",
			pin:          "Work",
			chrome:       fakeChrome{session: "sess-live"},
			files:        1,
			wantStdout:   1,
			wantStderr:   "Re-read the session from Chrome; retrying.\n",
			wantProfiles: []string{"Work"},
			wantStored:   "sess-live",
		},
		{
			name:         "Chrome holding the same cookie reports the original error",
			stored:       "sess-dead",
			chrome:       fakeChrome{session: "sess-dead"},
			files:        1,
			wantErr:      "uploading %s: get upload token: repo page returned 302 — expired session (re-run: ghattach login)",
			wantProfiles: []string{""},
			wantStored:   "sess-dead",
		},
		{
			name:         "an unreadable Chrome rides under the original error",
			stored:       "sess-dead",
			chrome:       fakeChrome{err: errors.New("no Chrome here")},
			files:        1,
			wantErr:      "expired session (re-run: ghattach login) (could not self-heal: re-reading the session from Chrome: no Chrome here)",
			wantProfiles: []string{""},
			wantStored:   "sess-dead",
		},
		{
			// Chrome also holds a dead cookie: the retry fails and Chrome is not
			// read a second time.
			name:         "Chrome is read at most once per run",
			stored:       "sess-dead",
			chrome:       fakeChrome{session: "sess-also-dead"},
			files:        2,
			wantErr:      "expired session",
			wantStderr:   "Re-read the session from Chrome; retrying.\n",
			wantProfiles: []string{""},
			wantStored:   "sess-also-dead",
		},
		{
			// The first file recovers through Chrome; the session then dies
			// again mid-run (the policy step answers 401). Chrome is not read
			// a second time, and the second file's error stands.
			name:         "a second expiry after a successful re-read is not retried",
			stored:       "sess-dead",
			chrome:       fakeChrome{session: "sess-live"},
			policy401At:  2,
			files:        2,
			wantStdout:   1,
			wantErr:      "policy endpoint returned 401 — expired session",
			wantStderr:   "Re-read the session from Chrome; retrying.\n",
			wantProfiles: []string{""},
			wantStored:   "sess-live",
		},
		{
			// A renamed repo whose new name starts with "login" is a move, not an
			// expired session: the stored session is kept and Chrome is not read.
			name:         "a 301 to a repo named login-service is a rename, not expiry",
			stored:       "sess-dead",
			chrome:       fakeChrome{session: "sess-live"},
			deadStatus:   http.StatusMovedPermanently,
			deadLocation: "https://github.com/acme/login-service",
			files:        1,
			wantErr:      "repo page returned 301 to https://github.com/acme/login-service — repo moved or renamed",
			wantStored:   "sess-dead",
		},
		{
			// A redirect with no Location is not a login redirect: no re-read.
			name:       "a 302 with no Location never triggers a Chrome re-read",
			stored:     "sess-dead",
			chrome:     fakeChrome{session: "sess-live"},
			deadStatus: http.StatusFound,
			noLocation: true,
			files:      1,
			wantErr:    "repo page returned 302 — no access to acme/widgets, or it does not exist",
			wantStored: "sess-dead",
		},
		{
			// A 404 means no access or no such repo, never expiry.
			name:       "a 404 never triggers a Chrome re-read",
			stored:     "sess-dead",
			deadStatus: http.StatusNotFound,
			files:      1,
			wantErr:    "repo page returned 404 — no access to acme/widgets, or it does not exist",
			wantStored: "sess-dead",
		},
		{
			name:         "a 307 to /login counts as expiry",
			stored:       "sess-dead",
			deadStatus:   http.StatusTemporaryRedirect,
			deadLocation: "/login",
			chrome:       fakeChrome{session: "sess-live"},
			files:        1,
			wantStdout:   1,
			wantStderr:   "Re-read the session from Chrome; retrying.\n",
			wantProfiles: []string{""},
			wantStored:   "sess-live",
		},
		{
			name:         "a 401 counts as expiry",
			stored:       "sess-dead",
			deadStatus:   http.StatusUnauthorized,
			chrome:       fakeChrome{session: "sess-live"},
			files:        1,
			wantStdout:   1,
			wantStderr:   "Re-read the session from Chrome; retrying.\n",
			wantProfiles: []string{""},
			wantStored:   "sess-live",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := tempHome(t)
			_, err := storeSession(tt.stored, tt.pin)
			require.NoError(t, err)
			s := newCLIStub(t, "sess-live")
			if tt.deadStatus != 0 {
				s.deadStatus = tt.deadStatus
			}
			if tt.deadLocation != "" {
				s.deadLocation = tt.deadLocation
			}
			if tt.noLocation {
				s.deadLocation = ""
			}
			s.policy401At = tt.policy401At
			useStub(t, s)
			chrome := tt.chrome
			useFakeChrome(t, &chrome)
			dir := t.TempDir()
			args := []string{"upload"}
			for i := range tt.files {
				args = append(args, writeFile(t, dir, fmt.Sprintf("shot%d.png", i), "png"))
			}
			args = append(args, "-R", "acme/widgets", "--repo-id", "7")

			res := runCLI(t, "", args...)
			if tt.wantErr != "" {
				require.Error(t, res.err)
				want := tt.wantErr
				if strings.Contains(want, "%s") {
					want = fmt.Sprintf(want, args[1])
				}
				assert.Contains(t, res.err.Error(), want)
			} else {
				require.NoError(t, res.err)
			}
			assert.Equal(t, tt.wantStdout, strings.Count(res.stdout, "\n"), "stdout: %q", res.stdout)
			assert.Equal(t, tt.wantStderr, res.stderr)
			assert.Equal(t, tt.wantProfiles, chrome.profiles)
			assert.Equal(t, tt.wantStored, readStore(t, home).UserSession)
			assert.Equal(t, tt.pin, readStore(t, home).ChromeProfile, "the pin survives a refresh")
		})
	}
}

func TestLoginCommand(t *testing.T) {
	instructions := "On macOS, read the cookie straight out of a signed-in Chrome:\n\n  ghattach login --from-chrome\n\n" +
		"Or copy the user_session value from a logged-in github.com tab\n" +
		"(DevTools > Application > Cookies > https://github.com), then run:\n\n  ghattach login --cookie -\n"
	tests := []struct {
		name         string
		args         []string
		stdin        string
		chrome       fakeChrome
		priorPin     string
		wantStdout   string // "%s" is replaced by the store path
		wantStderr   string
		wantErr      string
		wantSession  string
		wantPin      string
		wantNoStore  bool
		wantProfiles []string
	}{
		{name: "no flag prints the two ways to log in", args: []string{"login"}, wantStdout: instructions, wantNoStore: true},
		{
			// As --cookie "$UNSET_SECRET" in a script: an error, and any stored
			// session stays as it was.
			name: "an empty --cookie fails and keeps the stored session", args: []string{"login", "--cookie", ""},
			priorPin: "Work", wantErr: "parsing cookie input: empty cookie", wantNoStore: true,
		},
		{
			name: "--chrome-profile without --from-chrome fails", args: []string{"login", "--chrome-profile", "Work"},
			wantErr: "--chrome-profile needs --from-chrome", wantNoStore: true,
		},
		{
			name: "--cookie - accepts a header that starts with Cookie:", args: []string{"login", "--cookie", "-"},
			stdin:       "Cookie: logged_in=yes; user_session=abc\n",
			wantStdout:  "Session stored at %s\nVerify with: ghattach upload <file> --repo OWNER/REPO\n",
			wantStderr:  "Paste the user_session VALUE, then Enter: ",
			wantSession: "abc",
		},
		{
			name: "a value net/http cannot send is refused", args: []string{"login", "--cookie", "-"},
			stdin:      "\"fake\"\n",
			wantStderr: "Paste the user_session VALUE, then Enter: ",
			wantErr:    `parsing cookie input: session value contains '"', which is invalid in a cookie`, wantNoStore: true,
		},
		{
			name: "--cookie with a bare value", args: []string{"login", "--cookie", "abc123"},
			wantStdout:  "Session stored at %s\nVerify with: ghattach upload <file> --repo OWNER/REPO\n",
			wantSession: "abc123",
		},
		{
			name: "--cookie - reads a full Cookie header from stdin", args: []string{"login", "--cookie", "-"},
			stdin:       "logged_in=yes; user_session=abc123; _gh_sess=zzz\n",
			wantStdout:  "Session stored at %s\nVerify with: ghattach upload <file> --repo OWNER/REPO\n",
			wantStderr:  "Paste the user_session VALUE, then Enter: ",
			wantSession: "abc123",
		},
		{
			// pbpaste sends no trailing newline.
			name: "--cookie - reads a header with no trailing newline", args: []string{"login", "--cookie", "-"},
			stdin:       "logged_in=yes; user_session=abc",
			wantStdout:  "Session stored at %s\nVerify with: ghattach upload <file> --repo OWNER/REPO\n",
			wantStderr:  "Paste the user_session VALUE, then Enter: ",
			wantSession: "abc",
		},
		{
			name: "--cookie - with empty stdin fails", args: []string{"login", "--cookie", "-"},
			wantStderr: "Paste the user_session VALUE, then Enter: ",
			wantErr:    "reading cookie input: reading cookie from stdin: EOF", wantNoStore: true,
		},
		{
			name: "a header without user_session fails", args: []string{"login", "--cookie", "logged_in=yes; _gh_sess=zzz"},
			wantErr: "parsing cookie input: no user_session cookie found in input", wantNoStore: true,
		},
		{
			name: "a pasted cookie clears an earlier pin", args: []string{"login", "--cookie", "abc123"},
			priorPin:    "Old",
			wantStdout:  "Session stored at %s\nVerify with: ghattach upload <file> --repo OWNER/REPO\n",
			wantSession: "abc123", wantPin: "",
		},
		{
			name: "--chrome-profile with --cookie fails", args: []string{"login", "--cookie", "abc123", "--chrome-profile", "Work"},
			wantErr: "--chrome-profile needs --from-chrome", wantNoStore: true,
		},
		{
			name: "--from-chrome stores Chrome's session", args: []string{"login", "--from-chrome"},
			chrome:      fakeChrome{session: "sess-chrome"},
			wantStdout:  "Session stored at %s\nVerify with: ghattach upload <file> --repo OWNER/REPO\n",
			wantSession: "sess-chrome", wantProfiles: []string{""},
		},
		{
			name: "--from-chrome with --chrome-profile stores the pin", args: []string{"login", "--from-chrome", "--chrome-profile", "Work"},
			chrome:      fakeChrome{session: "sess-chrome"},
			wantStdout:  "Session stored at %s\nVerify with: ghattach upload <file> --repo OWNER/REPO\n",
			wantSession: "sess-chrome", wantPin: "Work", wantProfiles: []string{"Work"},
		},
		{
			name: "--from-chrome reports Chrome's error", args: []string{"login", "--from-chrome"},
			chrome:  fakeChrome{err: errors.New("no Chrome here")},
			wantErr: "reading the session from Chrome: no Chrome here", wantNoStore: true, wantProfiles: []string{""},
		},
		{
			name: "--cookie and --from-chrome are mutually exclusive", args: []string{"login", "--cookie", "x", "--from-chrome"},
			wantErr: "if any flags in the group [cookie from-chrome] are set none of the others can be", wantNoStore: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := tempHome(t)
			if tt.priorPin != "" {
				_, err := storeSession("old", tt.priorPin)
				require.NoError(t, err)
			}
			chrome := tt.chrome
			useFakeChrome(t, &chrome)

			res := runCLI(t, tt.stdin, tt.args...)
			if tt.wantErr != "" {
				require.Error(t, res.err)
				assert.Contains(t, res.err.Error(), tt.wantErr)
			} else {
				require.NoError(t, res.err)
			}
			assert.Equal(t, strings.ReplaceAll(tt.wantStdout, "%s", storePath(home)), res.stdout)
			assert.Equal(t, tt.wantStderr, res.stderr)
			assert.Equal(t, tt.wantProfiles, chrome.profiles)
			if tt.wantNoStore {
				if tt.priorPin == "" {
					assert.NoFileExists(t, storePath(home))
				} else {
					assert.Equal(t, cookieStore{UserSession: "old", ChromeProfile: tt.priorPin}, readStore(t, home))
				}
				return
			}
			got := readStore(t, home)
			assert.Equal(t, tt.wantSession, got.UserSession)
			assert.Equal(t, tt.wantPin, got.ChromeProfile)
			info, err := os.Stat(storePath(home))
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			dirInfo, err := os.Stat(filepath.Dir(storePath(home)))
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
		})
	}
}

func TestLogoutCommand(t *testing.T) {
	t.Run("removes the store and leftover temp copies", func(t *testing.T) {
		home := tempHome(t)
		_, err := storeSession("sess", "")
		require.NoError(t, err)
		leftover := filepath.Join(filepath.Dir(storePath(home)), "cookies-123.json")
		require.NoError(t, os.WriteFile(leftover, []byte(`{"user_session":"old"}`), 0o600))

		res := runCLI(t, "", "logout")
		require.NoError(t, res.err)
		assert.Equal(t, "Removed "+storePath(home)+"\nAlso revoke the browser session if it is no longer wanted: https://github.com/settings/sessions\n", res.stdout)
		assert.NoFileExists(t, storePath(home))
		assert.NoFileExists(t, leftover)
	})
	t.Run("says so when nothing is stored", func(t *testing.T) {
		tempHome(t)
		res := runCLI(t, "", "logout")
		require.NoError(t, res.err)
		assert.Equal(t, "No stored session.\n", res.stdout)
	})
}

func TestLoadStoreErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    *string
		wantErr string
	}{
		{name: "missing", wantErr: "not logged in — run: ghattach login --from-chrome (macOS) or: ghattach login --cookie -"},
		{name: "not JSON", body: ptr("{"), wantErr: "unmarshaling cookie store"},
		{name: "no session", body: ptr(`{"chrome_profile":"Work"}`), wantErr: "cookie store has no session — run: ghattach login"},
		{name: "a session a cookie cannot carry", body: ptr(`{"user_session":"sess-live\n"}`), wantErr: "cookie store holds an unusable session — run: ghattach login: session value contains '\\n'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := tempHome(t)
			if tt.body != nil {
				require.NoError(t, os.MkdirAll(filepath.Dir(storePath(home)), 0o700))
				require.NoError(t, os.WriteFile(storePath(home), []byte(*tt.body), 0o600))
			}
			_, err := loadStore()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func ptr(s string) *string { return &s }

// TestStoreSessionSweepsTempCopies pins that a write removes cookies-*.json
// left by an interrupted earlier write, each a full copy of the secret.
func TestStoreSessionSweepsTempCopies(t *testing.T) {
	home := tempHome(t)
	dir := filepath.Dir(storePath(home))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	leftover := filepath.Join(dir, "cookies-999.json")
	require.NoError(t, os.WriteFile(leftover, []byte("old"), 0o600))
	stale := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(leftover, stale, stale))

	_, err := storeSession("new", "")
	require.NoError(t, err)
	assert.NoFileExists(t, leftover)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "cookies.json", entries[0].Name())
}

// TestUploadFailsWhenStdoutCannotBeWritten pins that a URL that cannot be
// printed fails the run: the upload is real, and exit 0 would lose it.
func TestUploadFailsWhenStdoutCannotBeWritten(t *testing.T) {
	tempHome(t)
	_, err := storeSession("sess-live", "")
	require.NoError(t, err)
	useStub(t, newCLIStub(t, "sess-live"))
	useFakeChrome(t, &fakeChrome{err: errors.New("Chrome must not be read in this test")})
	shot := writeFile(t, t.TempDir(), "shot.png", "png")
	readOnly, err := os.Open(shot) //nolint:gosec // G304: the test's own temp file
	require.NoError(t, err)
	defer readOnly.Close() //nolint:errcheck // test file

	resetFlags(rootCmd)
	rootCmd.SetArgs([]string{"upload", shot, "-R", "acme/widgets", "--repo-id", "7"})
	oldOut := os.Stdout
	os.Stdout = readOnly
	err = rootCmd.Execute()
	os.Stdout = oldOut
	require.Error(t, err)
	assert.Contains(t, err.Error(), "writing the result for "+shot)
}

// TestUploadChecksEveryFileBeforeAnyRequest pins that every file argument is
// checked before the repo id lookup and before the first upload: a missing
// later file fails the run with nothing uploaded and gh never called.
func TestUploadChecksEveryFileBeforeAnyRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake gh is a shell script")
	}
	tempHome(t)
	_, err := storeSession("sess-live", "")
	require.NoError(t, err)
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	writeFile(t, bin, "gh", "#!/bin/sh\necho gh >> \"$GH_CALLS\"\necho 7\n")
	require.NoError(t, os.Chmod(filepath.Join(bin, "gh"), 0o700)) //nolint:gosec // G302: the fake gh must be executable
	t.Setenv("GH_CALLS", calls)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := newCLIStub(t, "sess-live")
	useStub(t, s)
	useFakeChrome(t, &fakeChrome{err: errors.New("Chrome must not be read in this test")})
	dir := t.TempDir()
	good := writeFile(t, dir, "good.png", "png")
	missing := filepath.Join(dir, "missing.png")

	res := runCLI(t, "", "upload", good, missing, "-R", "acme/widgets")
	require.Error(t, res.err)
	assert.Contains(t, res.err.Error(), "uploading "+missing+": stat file")
	assert.Empty(t, res.stdout, "good.png is not uploaded")
	assert.Empty(t, s.repoPaths)
	assert.NoFileExists(t, calls, "the id lookup never runs")
}
