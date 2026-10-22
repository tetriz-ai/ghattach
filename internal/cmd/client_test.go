// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGitHub simulates the four-endpoint pipeline: repo page (uploadToken),
// /upload/policies/assets, the S3 form POST, and the finalize PUT. It records
// what each step received so tests can assert the protocol shape. Handlers
// use t.Errorf+return (never require.*): require calls FailNow, which must
// not run on an httptest server goroutine.
type fakeGitHub struct {
	mux *http.ServeMux
	srv *httptest.Server

	repoPageBody     string
	repoPageHits     int
	policyStatus     int
	finalizeStatus   int
	policyLocation   string // Location sent with a non-201 policy answer, if any
	finalizeLocation string // Location sent with a non-200 finalize answer, if any
	assetUploadURL   string

	policyForm   map[string]string // fields received by /upload/policies/assets
	s3Form       map[string]string // fields received by the S3 endpoint
	s3FileBytes  []byte
	finalizeForm map[string]string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		mux:            http.NewServeMux(),
		policyStatus:   http.StatusCreated,
		finalizeStatus: http.StatusOK,
		assetUploadURL: "/upload/assets/42",
	}
	f.srv = httptest.NewServer(f.mux)
	t.Cleanup(f.srv.Close)
	f.repoPageBody = `<html>... "uploadToken":"tok-123" ...</html>`

	f.mux.HandleFunc("GET /acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		f.repoPageHits++
		_, _ = w.Write([]byte(f.repoPageBody))
	})
	f.mux.HandleFunc("POST /upload/policies/assets", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: test stub, bounded at 1 MiB
			t.Errorf("parsing policy form: %v", err)
			return
		}
		f.policyForm = flattenForm(r.MultipartForm.Value)
		if f.policyStatus != http.StatusCreated {
			if f.policyLocation != "" {
				w.Header().Set("Location", f.policyLocation)
			}
			w.WriteHeader(f.policyStatus)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upload_url": f.srv.URL + "/s3-bucket",
			"asset":      map[string]any{"id": 42, "content_type": "image/png"},
			"form": map[string]string{
				"key":    "user-attachments/assets/uuid-1",
				"policy": "b64policy",
			},
			"asset_upload_url":                f.assetUploadURL,
			"asset_upload_authenticity_token": "finalize-tok",
		})
	})
	f.mux.HandleFunc("POST /s3-bucket", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: test stub, bounded at 1 MiB
			t.Errorf("parsing S3 form: %v", err)
			return
		}
		f.s3Form = flattenForm(r.MultipartForm.Value)
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("reading S3 file part: %v", err)
			return
		}
		defer file.Close() //nolint:errcheck // test read
		buf := make([]byte, 64)
		n, _ := file.Read(buf)
		f.s3FileBytes = buf[:n]
		w.WriteHeader(http.StatusNoContent)
	})
	f.mux.HandleFunc("PUT /upload/assets/42", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: test stub, bounded at 1 MiB
			t.Errorf("parsing finalize form: %v", err)
			return
		}
		f.finalizeForm = flattenForm(r.MultipartForm.Value)
		if f.finalizeStatus != http.StatusOK {
			if f.finalizeLocation != "" {
				w.Header().Set("Location", f.finalizeLocation)
			}
			w.WriteHeader(f.finalizeStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"href": "https://github.com/user-attachments/assets/uuid-1",
			"name": "shot.png",
		})
	})
	return f
}

func flattenForm(v map[string][]string) map[string]string {
	out := make(map[string]string, len(v))
	for k, vals := range v {
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	return out
}

func writeTempPNG(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shot.png")
	require.NoError(t, os.WriteFile(path, []byte("png-bytes"), 0o600))
	return path
}

func TestClientUpload(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prep    func(f *fakeGitHub)
		wantErr string
		check   func(t *testing.T, f *fakeGitHub, r *Result)
	}{
		{
			name: "happy path threads token, repo id, file and finalize markdown",
			prep: func(_ *fakeGitHub) {},
			check: func(t *testing.T, f *fakeGitHub, r *Result) {
				assert.Equal(t, "tok-123", f.policyForm["authenticity_token"])
				assert.Equal(t, "7", f.policyForm["repository_id"])
				assert.Equal(t, "shot.png", f.policyForm["name"])
				assert.Equal(t, "image/png", f.policyForm["content_type"])
				assert.Equal(t, "user-attachments/assets/uuid-1", f.s3Form["key"])
				assert.Equal(t, []byte("png-bytes"), f.s3FileBytes)
				assert.Equal(t, "finalize-tok", f.finalizeForm["authenticity_token"])
				assert.Equal(t, "https://github.com/user-attachments/assets/uuid-1", r.URL)
				assert.Equal(t, "![shot.png](https://github.com/user-attachments/assets/uuid-1)", r.Markdown)
			},
		},
		{
			name: "missing uploadToken surfaces access error",
			prep: func(f *fakeGitHub) {
				f.repoPageBody = `<html>no token here</html>`
			},
			wantErr: "uploadToken not found",
		},
		{
			name: "SAML interstitial names the sso fix",
			prep: func(f *fakeGitHub) {
				f.repoPageBody = `<html><a href="/orgs/acme/sso">Continue</a></html>`
			},
			wantErr: "enforces SAML SSO",
		},
		{
			name: "policy rejection surfaces status",
			prep: func(f *fakeGitHub) {
				f.policyStatus = http.StatusUnprocessableEntity
			},
			wantErr: "policy endpoint returned 422",
		},
		{
			name: "finalize rejection surfaces status",
			prep: func(f *fakeGitHub) {
				f.finalizeStatus = http.StatusUnauthorized
			},
			wantErr: "finalize returned 401",
		},
		{
			name: "protocol-relative finalize path is rejected",
			prep: func(f *fakeGitHub) {
				f.assetUploadURL = "//evil.example/upload/assets/42"
			},
			wantErr: "not a root-relative path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeGitHub(t)
			tt.prep(f)
			client := NewClient("sess-1", f.srv.URL)
			result, err := client.Upload("acme", "widgets", 7, writeTempPNG(t))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			tt.check(t, f, result)
		})
	}
}

func TestClientUploadLocalGuards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.png")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "directory rejected", path: dir, wantErr: "not a regular file"},
		{name: "empty file rejected", path: empty, wantErr: "is empty"},
		{name: "missing file rejected", path: filepath.Join(dir, "nope.png"), wantErr: "stat file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Guards fire before any request reaches the server.
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer srv.Close()
			_, err := NewClient("sess-1", srv.URL).Upload("acme", "widgets", 7, tt.path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Zero(t, requests.Load())
		})
	}
}

// TestClientSendsSessionCookiePair pins the CSRF contract: both user_session
// and __Host-user_session_same_site must reach GitHub endpoints (set
// per-request, not via a jar, so plain-http test servers still see them).
func TestClientSendsSessionCookiePair(t *testing.T) {
	t.Parallel()
	var gotCookies []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /acme/widgets", func(w http.ResponseWriter, r *http.Request) {
		for _, c := range r.Cookies() {
			gotCookies = append(gotCookies, c.Name+"="+c.Value)
		}
		_, _ = w.Write([]byte(`"uploadToken":"tok"`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewClient("sess-xyz", srv.URL)
	_, err := client.uploadToken("acme", "widgets")
	require.NoError(t, err)
	assert.Contains(t, gotCookies, "user_session=sess-xyz")
	assert.Contains(t, gotCookies, "__Host-user_session_same_site=sess-xyz")
}

// TestClientDoesNotFollowRedirects pins the no-redirect contract: a 302 from
// an expired session must surface as the response status (mapped to the
// re-login hint), never be chased to another URL with the session cookie.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	var followedTarget bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /acme/widgets", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	mux.HandleFunc("GET /login", func(http.ResponseWriter, *http.Request) {
		followedTarget = true
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := NewClient("stale", srv.URL).uploadToken("acme", "widgets")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "302")
	assert.Contains(t, err.Error(), "ghattach login")
	assert.False(t, followedTarget, "client must not follow the redirect")
	// upload's auto-refresh keys off this sentinel, so the wrap must survive any
	// rewording of the message around it.
	assert.ErrorIs(t, err, errSessionExpired)
}

// TestUploadTokenCachedPerRepo pins the multi-file behavior: one repo-page
// fetch serves every upload in the invocation.
func TestUploadTokenCachedPerRepo(t *testing.T) {
	t.Parallel()
	f := newFakeGitHub(t)
	client := NewClient("s", f.srv.URL)
	for range 3 {
		_, err := client.uploadToken("acme", "widgets")
		require.NoError(t, err)
	}
	assert.Equal(t, 1, f.repoPageHits)
}

// TestRepoPageStatusClassification pins the distinction measured against real
// GitHub: a present-but-invalid session is redirected to /login, whereas 404 is
// no-access-or-absent (what an anonymous request gets, since GitHub hides
// private repos). Conflating them would make `upload` re-read Chrome for a
// repo typo and report a permissions problem as expiry.
func TestRepoPageStatusClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		handler         func(w http.ResponseWriter, r *http.Request)
		wantIsExpired   bool
		wantErrContains string
	}{
		{
			name: "302 to login is expiry",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/login?return_to=x", http.StatusFound)
			},
			wantIsExpired: true,
		},
		{
			name:            "404 is no-access, never expiry",
			handler:         func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantIsExpired:   false,
			wantErrContains: "no access",
		},
		{
			name:            "403 is no-access, never expiry",
			handler:         func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
			wantIsExpired:   false,
			wantErrContains: "no access",
		},
		{
			name: "401 is expiry",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			wantIsExpired: true,
		},
		{
			name:            "an empty uploadToken is not a token",
			handler:         func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"uploadToken":""}`) },
			wantErrContains: "uploadToken not found",
		},
		{
			name: "a 307 to login is expiry",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
			},
			wantIsExpired: true,
		},
		{
			name: "a 307 elsewhere means the repo moved",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/acme/renamed", http.StatusTemporaryRedirect)
			},
			wantErrContains: "moved or renamed",
		},
		{
			name: "redirect elsewhere means the repo moved",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/acme/renamed", http.StatusMovedPermanently)
			},
			wantIsExpired:   false,
			wantErrContains: "moved or renamed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("GET /acme/widgets", tt.handler)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			_, err := NewClient("s", srv.URL).uploadToken("acme", "widgets")
			require.Error(t, err)
			assert.Equal(t, tt.wantIsExpired, errors.Is(err, errSessionExpired))
			if tt.wantErrContains != "" {
				assert.Contains(t, err.Error(), tt.wantErrContains)
			}
		})
	}
}

// TestAuthFailureAfterTokenCache covers the steps that run *after* uploadToken
// is cached: a session dying mid-run surfaces at policy or finalize, and unless
// those map to errSessionExpired the caller's self-heal silently never fires.
// A non-auth failure at the same step must NOT be mistaken for expiry.
func TestAuthFailureAfterTokenCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		prep            func(f *fakeGitHub)
		wantIsExpired   bool
		wantErrContains string
	}{
		{
			name:          "policy 401",
			prep:          func(f *fakeGitHub) { f.policyStatus = http.StatusUnauthorized },
			wantIsExpired: true,
		},
		{
			name:          "finalize 401",
			prep:          func(f *fakeGitHub) { f.finalizeStatus = http.StatusUnauthorized },
			wantIsExpired: true,
		},
		{
			name: "policy 302 to /login is expiry",
			prep: func(f *fakeGitHub) {
				f.policyStatus, f.policyLocation = http.StatusFound, "/login?return_to=x"
			},
			wantIsExpired: true,
		},
		{
			name: "finalize 302 to /login is expiry",
			prep: func(f *fakeGitHub) {
				f.finalizeStatus, f.finalizeLocation = http.StatusFound, "/login?return_to=x"
			},
			wantIsExpired: true,
		},
		{
			name: "policy 302 to a repo named login-service is not expiry",
			prep: func(f *fakeGitHub) {
				f.policyStatus, f.policyLocation = http.StatusFound, "/acme/login-service"
			},
			wantIsExpired:   false,
			wantErrContains: "policy endpoint returned 302 (want 201)",
		},
		{
			name: "finalize 302 to a repo named login-service is not expiry",
			prep: func(f *fakeGitHub) {
				f.finalizeStatus, f.finalizeLocation = http.StatusFound, "/acme/login-service"
			},
			wantIsExpired:   false,
			wantErrContains: "finalize returned 302 (want 200)",
		},
		{
			// A server error is not an auth problem: re-reading the cookie would
			// not fix it, so it must stay a plain failure.
			name:            "policy 500 is not expiry",
			prep:            func(f *fakeGitHub) { f.policyStatus = http.StatusInternalServerError },
			wantIsExpired:   false,
			wantErrContains: "want 201",
		},
		{
			name:            "finalize 422 is not expiry",
			prep:            func(f *fakeGitHub) { f.finalizeStatus = http.StatusUnprocessableEntity },
			wantIsExpired:   false,
			wantErrContains: "want 200",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeGitHub(t)
			tt.prep(f)
			_, err := NewClient("s", f.srv.URL).Upload("acme", "widgets", 7, writeTempPNG(t))
			require.Error(t, err)
			assert.Equal(t, tt.wantIsExpired, errors.Is(err, errSessionExpired))
			if tt.wantErrContains != "" {
				assert.Contains(t, err.Error(), tt.wantErrContains)
			}
		})
	}
}

// TestSessionStoreRoundTrip pins the store contract the refresh path depends on:
// an explicit --chrome-profile pin survives a write/read cycle, and a later
// write without one clears it rather than leaving a stale profile that would
// re-read the wrong account.
func TestSessionStoreRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // cookiePath() resolves under the home dir; keep the real store untouched

	path, err := storeSession("sess-1", "Work")
	require.NoError(t, err)
	assert.FileExists(t, path)

	store, err := loadStore()
	require.NoError(t, err)
	assert.Equal(t, "sess-1", store.UserSession)
	assert.Equal(t, "Work", store.ChromeProfile)

	_, err = storeSession("sess-2", "")
	require.NoError(t, err)
	store, err = loadStore()
	require.NoError(t, err)
	assert.Equal(t, "sess-2", store.UserSession)
	assert.Empty(t, store.ChromeProfile, "an unpinned write must clear a stale pin")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the store holds a password-equivalent secret")
}

func TestExtractUserSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "bare value", in: "abc123", want: "abc123"},
		{name: "bare value with base64 padding equals", in: "abc123==", want: "abc123=="},
		{name: "full cookie header", in: "logged_in=yes; user_session=abc123; _gh_sess=zzz", want: "abc123"},
		{name: "prefixed single pair", in: "user_session=abc123", want: "abc123"},
		{name: "header without user_session", in: "logged_in=yes; _gh_sess=zzz", wantErr: true},
		{name: "empty", in: "  ", wantErr: true},
		{name: "empty user_session value", in: "user_session=; other=1", wantErr: true},
		{name: "a header that starts with its own name", in: "Cookie: user_session=abc; x=y", want: "abc"},
		{name: "a lower-case header name", in: "cookie: logged_in=yes; user_session=abc", want: "abc"},
		{name: "only the exact cookie name", in: "my_user_session=x; user_session=y", want: "y"},
		{name: "a lookalike name alone", in: "my_user_session=x", wantErr: true},
		{name: "a single pair for another cookie", in: "__Host-user_session_same_site=abc", wantErr: true},
		{name: "spaces around the value", in: "user_session= abc ; x=y", want: "abc"},
		// A value net/http would rewrite on the wire is refused, not stored.
		{name: "a backslash", in: `abc\def`, wantErr: true},
		{name: "a quote", in: `ab"c`, wantErr: true},
		{name: "a space", in: "abc def", wantErr: true},
		{name: "a comma", in: "abc,def", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := extractUserSession(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRenderMarkdown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		file        string
		contentType string
		want        string
	}{
		{name: "image embeds", contentType: "image/png", want: "![n](u)"},
		{name: "video emits bare url", contentType: "video/mp4", want: "u"},
		{name: "other links", contentType: "application/pdf", want: "[n](u)"},
		{name: "the media type ignores case", contentType: "IMAGE/png", want: "![n](u)"},
		{name: "brackets in the name are escaped", file: `a]b[c\d.png`, contentType: "image/png", want: `![a\]b\[c\\d.png](u)`},
		{name: "control bytes in the name become spaces", file: "a\x1b[31mb.png", contentType: "image/png", want: `![a \[31mb.png](u)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			file := tt.file
			if file == "" {
				file = "n"
			}
			assert.Equal(t, tt.want, renderMarkdown(file, "u", tt.contentType))
		})
	}
}

func TestDetectContentType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "png", path: "a/b.png", want: "image/png"},
		// .html is in Go's builtin table WITH a charset parameter, so this
		// exercises the parameter-stripping branch on every platform.
		{name: "parameters stripped", path: "a/b.html", want: "text/html"},
		// .mp4 is absent from Go's builtin table and OS mime files may be
		// missing (minimal Linux) — the override keeps video markdown working.
		{name: "mp4 resolves without OS mime files", path: "a/b.mp4", want: "video/mp4"},
		// GitHub 422s .log unless it is declared text/x-log; the override must
		// beat the OS mime answer (text/plain on macOS).
		{name: "log uses GitHub's text/x-log over the OS answer", path: "a/b.log", want: "text/x-log"},
		{name: "unknown falls back", path: "a/b.zzz9", want: "application/octet-stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := detectContentType(tt.path)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, ";", "media type must carry no parameters")
		})
	}
}

func TestSanitizeFileName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ab.png", sanitizeFileName("a\r\nb.png"))
	assert.Equal(t, "sn☃w man.png", sanitizeFileName("sn☃w man.png"))
	assert.Equal(t, "abc.png", sanitizeFileName("a\tb\x00c\x7f.png"), "every control byte is removed")
	assert.Equal(t, "ab.png", sanitizeFileName("a\u0085b.png"), "a C1 control character is removed too")
	assert.Equal(t, "bad\xffname.png", sanitizeFileName("bad\xffname.png"), "invalid UTF-8 is kept byte for byte")
}

// TestS3FieldOrdering pins that the known presigned fields precede any unknown
// extras and the file part is last — S3 policy uploads are order-sensitive.
func TestS3FieldOrdering(t *testing.T) {
	t.Parallel()
	var order []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /s3", func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart reader: %v", err)
			return
		}
		for {
			part, err := mr.NextPart()
			if err != nil { // io.EOF terminates the part walk; a torn body just yields fewer parts and fails the ordering assertions
				break
			}
			order = append(order, part.FormName())
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	policy := &policyResponse{
		UploadURL: srv.URL + "/s3",
		Form: map[string]string{
			"policy":       "p",
			"key":          "k",
			"x-new-future": "extra",
		},
	}
	client := NewClient("s", srv.URL)
	require.NoError(t, client.uploadToS3(policy, writeTempPNG(t), "shot.png", 9))
	require.NotEmpty(t, order)
	assert.Equal(t, "file", order[len(order)-1], "file part must be last")
	assert.Less(t, indexOf(order, "key"), indexOf(order, "policy"), "known fields keep canonical order")
	assert.Less(t, indexOf(order, "policy"), indexOf(order, "x-new-future"), "unknown fields follow known ones")
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return len(s)
}

// TestPolicyResponseDecodesAllFields guards the policyResponse JSON tags
// against the real field names: a typo'd tag compiles fine but silently
// zeroes the field, failing Upload in a confusing place — so decode the
// fake's payload and check every field.
func TestPolicyResponseDecodesAllFields(t *testing.T) {
	t.Parallel()
	f := newFakeGitHub(t)
	client := NewClient("s", f.srv.URL)
	token, err := client.uploadToken("acme", "widgets")
	require.NoError(t, err)
	policy, err := client.requestPolicy("acme", "widgets", token, 7, "shot.png", 9, "image/png")
	require.NoError(t, err)
	assert.Equal(t, f.srv.URL+"/s3-bucket", policy.UploadURL)
	assert.Equal(t, 42, policy.Asset.ID)
	assert.Equal(t, "image/png", policy.Asset.ContentType)
	assert.Equal(t, "/upload/assets/42", policy.AssetUploadURL)
	assert.Equal(t, "finalize-tok", policy.AssetUploadAuthenticityToken)
	assert.NotContains(t, policy.Form, "repository_id", "repository_id is a policy-request field, never an S3 form field")
}
