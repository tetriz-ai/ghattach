// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPolicyResponseValidation pins that a policy response missing any field
// the next steps need fails at the policy step, naming the field.
func TestPolicyResponseValidation(t *testing.T) {
	t.Parallel()
	valid := func() map[string]any {
		return map[string]any{
			"upload_url":                      "http://s3.invalid/bucket",
			"asset":                           map[string]any{"id": 42, "content_type": "image/png"},
			"form":                            map[string]string{"key": "k"},
			"asset_upload_url":                "/upload/assets/42",
			"asset_upload_authenticity_token": "fin",
		}
	}
	tests := []struct {
		name    string
		edit    func(m map[string]any)
		raw     string
		status  int
		wantErr string
	}{
		{name: "missing upload_url", edit: func(m map[string]any) { delete(m, "upload_url") }, wantErr: "policy response missing upload_url"},
		{name: "empty form", edit: func(m map[string]any) { m["form"] = map[string]string{} }, wantErr: "policy response missing form fields"},
		{name: "absolute finalize URL", edit: func(m map[string]any) { m["asset_upload_url"] = "https://evil.example/x" }, wantErr: "is not a root-relative path"},
		{name: "missing finalize token", edit: func(m map[string]any) { delete(m, "asset_upload_authenticity_token") }, wantErr: "policy response missing asset_upload_authenticity_token"},
		{name: "missing asset id", edit: func(m map[string]any) { m["asset"] = map[string]any{"content_type": "image/png"} }, wantErr: "policy response missing asset id"},
		{name: "not JSON", raw: "<html>", wantErr: "decoding policy response"},
		{name: "a 200 instead of a 201", status: http.StatusOK, edit: func(map[string]any) {}, wantErr: "policy endpoint returned 200 (want 201)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("POST /upload/policies/assets", func(w http.ResponseWriter, _ *http.Request) {
				status := http.StatusCreated
				if tt.status != 0 {
					status = tt.status
				}
				w.WriteHeader(status)
				if tt.raw != "" {
					_, _ = io.WriteString(w, tt.raw)
					return
				}
				m := valid()
				tt.edit(m)
				_ = json.NewEncoder(w).Encode(m)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			_, err := NewClient("s", srv.URL).requestPolicy("acme", "widgets", "tok", 7, "a.png", 3, "image/png")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestFinalizeResponseValidation pins the finalize step's failure modes.
func TestFinalizeResponseValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "missing href", body: `{"name":"a.png"}`, wantErr: "finalize response missing href"},
		{name: "not JSON", body: `<html>`, wantErr: "decoding finalize response"},
		{name: "a javascript href", body: `{"href":"javascript:alert(1)","name":"x.png"}`, wantErr: `finalize response href "javascript:alert(1)" is not a user-attachments URL`},
		{name: "another host", body: `{"href":"https://evil.example/user-attachments/assets/u","name":"x.png"}`, wantErr: "is not a user-attachments URL"},
		{name: "a href that breaks the markdown", body: `{"href":"https://github.com/user-attachments/assets/u) [x](y","name":"x.png"}`, wantErr: "is not a user-attachments URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("PUT /upload/assets/42", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.body)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			policy := &policyResponse{AssetUploadURL: "/upload/assets/42", AssetUploadAuthenticityToken: "fin"}
			_, err := NewClient("s", srv.URL).finalize("acme", "widgets", "a.png", policy)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestFinalizeSendsSessionAndBrowserHeaders pins what the finalize PUT
// carries: the cookie pair and the headers GitHub's CSRF checks require.
func TestFinalizeSendsSessionAndBrowserHeaders(t *testing.T) {
	t.Parallel()
	var got *http.Request
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /upload/assets/42", func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		_, _ = io.WriteString(w, `{"href":"https://github.com/user-attachments/assets/u","name":"a.png"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	policy := &policyResponse{AssetUploadURL: "/upload/assets/42", AssetUploadAuthenticityToken: "fin"}
	policy.Asset.ContentType = "image/png"
	res, err := NewClient("sess", srv.URL).finalize("acme", "widgets", "a.png", policy)
	require.NoError(t, err)
	assert.Equal(t, "![a.png](https://github.com/user-attachments/assets/u)", res.Markdown)
	require.NotNil(t, got)
	assert.Equal(t, "application/json", got.Header.Get("Accept"))
	assert.Equal(t, srv.URL, got.Header.Get("Origin"))
	assert.Equal(t, srv.URL+"/acme/widgets", got.Header.Get("Referer"))
	assert.Equal(t, "XMLHttpRequest", got.Header.Get("X-Requested-With"))
	c, err := got.Cookie("user_session")
	require.NoError(t, err)
	assert.Equal(t, "sess", c.Value)
}

// TestS3Upload pins the S3 step: no GitHub cookie, a streamed body with a
// known length, and a readable error for a non-2xx answer.
func TestS3Upload(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "204 is success", status: http.StatusNoContent},
		{name: "200 is success", status: http.StatusOK},
		{name: "201 is success", status: http.StatusCreated},
		{name: "403 carries the body", status: http.StatusForbidden, wantErr: "S3 returned 403: <Error>AccessDenied</Error>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cookie string
			var length int64
			var chunked []string
			var body []byte
			mux := http.NewServeMux()
			mux.HandleFunc("POST /s3", func(w http.ResponseWriter, r *http.Request) {
				cookie = r.Header.Get("Cookie")
				length = r.ContentLength
				chunked = r.TransferEncoding
				body, _ = io.ReadAll(r.Body)
				w.WriteHeader(tt.status)
				if tt.status >= 300 {
					_, _ = io.WriteString(w, "<Error>AccessDenied</Error>")
				}
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			policy := &policyResponse{UploadURL: srv.URL + "/s3", Form: map[string]string{"key": "k"}}
			err := NewClient("sess", srv.URL).uploadToS3(policy, writeTempPNG(t), "shot.png", 9)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
			assert.Empty(t, cookie, "the presigned policy is the auth; no GitHub cookie goes to S3")
			// The file is streamed, but with a known length: storage refuses a
			// chunked POST.
			assert.Equal(t, int64(len(body)), length)
			assert.Empty(t, chunked)
		})
	}
}

// TestUploadTokenIsJSONUnescaped pins that the token's JSON escapes are
// undone before it is sent back to GitHub.
func TestUploadTokenIsJSONUnescaped(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"uploadToken":"ab\/cd+ef"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	tok, err := NewClient("s", srv.URL).uploadToken("acme", "widgets")
	require.NoError(t, err)
	assert.Equal(t, "ab/cd+ef", tok)
}

// TestUploadSizeCap pins the local 100 MB cap: one limit for every media
// type, checked before any network call. Sparse files keep the test cheap.
func TestUploadSizeCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sparse := func(name string, size int64) string {
		p := filepath.Join(dir, name)
		f, err := os.Create(p) //nolint:gosec // test temp dir
		require.NoError(t, err)
		require.NoError(t, f.Truncate(size))
		require.NoError(t, f.Close())
		return p
	}
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "an image one byte over", path: sparse("big.png", maxUploadBytes+1), wantErr: "is 104857601 bytes — over GitHub's 100 MiB attachment cap"},
		{name: "a video one byte over", path: sparse("big.mp4", maxUploadBytes+1), wantErr: "is 104857601 bytes — over GitHub's 100 MiB attachment cap"},
		// At the cap exactly, the local guard passes and the next step (the
		// repo page) is what fails here.
		{name: "exactly at the cap passes the local guard", path: sparse("edge.png", maxUploadBytes), wantErr: "get upload token: fetching repo page"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewClient("s", "http://unreachable.invalid").Upload("acme", "widgets", 7, tt.path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestIsAuthFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		status        int
		location      string
		emptyLocation bool
		want          bool
	}{
		{name: "401", status: http.StatusUnauthorized, want: true},
		{name: "302 to /login", status: http.StatusFound, location: "/login?return_to=x", want: true},
		{name: "301 to /login", status: http.StatusMovedPermanently, location: "https://github.com/login", want: true},
		{name: "302 elsewhere", status: http.StatusFound, location: "/acme/renamed", want: false},
		// Only the path /login counts; a repo or owner whose name starts with
		// "login" is a rename, not an expired session.
		{name: "301 to a renamed repo named login-service", status: http.StatusMovedPermanently, location: "https://github.com/acme/login-service", want: false},
		{name: "301 to an owner named loginradius", status: http.StatusMovedPermanently, location: "/loginradius/widgets", want: false},
		{name: "302 to a relative /login with a return_to", status: http.StatusFound, location: "/login?return_to=%2Facme%2Fwidgets", want: true},
		{name: "301 to github.com/login with a return_to", status: http.StatusMovedPermanently, location: "https://github.com/login?return_to=%2Facme%2Fwidgets", want: true},
		{name: "302 to /login on another host", status: http.StatusFound, location: "https://evil.example/login", want: false},
		// Other spellings of github.com/login that the promise covers.
		{name: "http scheme", status: http.StatusFound, location: "http://github.com/login", want: true},
		{name: "upper-case scheme and host", status: http.StatusFound, location: "HTTPS://GITHUB.COM/login", want: true},
		{name: "scheme-relative", status: http.StatusFound, location: "//github.com/login", want: true},
		{name: "percent-encoded path", status: http.StatusFound, location: "/%6Cogin", want: true},
		{name: "with a fragment", status: http.StatusFound, location: "/login#frag", want: true},
		{name: "with an empty query", status: http.StatusFound, location: "https://github.com/login?", want: true},
		// The path is matched ignoring case and a trailing slash, on github.com
		// or www.github.com, with or without the default port.
		{name: "trailing slash", status: http.StatusFound, location: "/login/", want: true},
		{name: "upper-case path", status: http.StatusFound, location: "/LOGIN", want: true},
		{name: "www host", status: http.StatusFound, location: "https://www.github.com/login", want: true},
		{name: "explicit port 443", status: http.StatusFound, location: "https://github.com:443/login", want: true},
		{name: "another port", status: http.StatusFound, location: "https://github.com:8443/login", want: false},
		{name: "a lookalike host", status: http.StatusFound, location: "https://github.com.evil.example/login", want: false},
		{name: "a deeper login path", status: http.StatusFound, location: "/login/oauth", want: false},
		// Unparseable, absent and empty Locations are never expiry.
		{name: "unparseable Location", status: http.StatusFound, location: "%zz", want: false},
		{name: "302 without a Location", status: http.StatusFound, want: false},
		{name: "302 with an empty Location", status: http.StatusFound, emptyLocation: true, want: false},
		// Every redirect code counts, not only 301 and 302.
		{name: "303 to /login", status: http.StatusSeeOther, location: "/login", want: true},
		{name: "307 to /login", status: http.StatusTemporaryRedirect, location: "/login", want: true},
		{name: "308 to /login", status: http.StatusPermanentRedirect, location: "/login", want: true},
		{name: "304 is not a redirect", status: http.StatusNotModified, location: "/login", want: false},
		{name: "403", status: http.StatusForbidden, want: false},
		{name: "404", status: http.StatusNotFound, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{StatusCode: tt.status, Header: http.Header{}}
			if tt.location != "" || tt.emptyLocation {
				resp.Header.Set("Location", tt.location)
			}
			assert.Equal(t, tt.want, isAuthFailure(resp))
		})
	}
}

// TestExpiredSessionHint pins the hint's wording, which names no login
// method, since the session may have come from either.
func TestExpiredSessionHint(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "expired session (re-run: ghattach login)", errSessionExpired.Error())
}

func TestDetectContentTypeTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want string
	}{
		{path: "a.mov", want: "video/quicktime"},
		{path: "a.webm", want: "video/webm"},
		{path: "a.txt", want: "text/plain"},
		{path: "a.zip", want: "application/zip"},
		{path: "A.PNG", want: "image/png"},
		{path: "a.JPG", want: "image/jpeg"},
		{path: "a.gif", want: "image/gif"},
		{path: "a.pdf", want: "application/pdf"},
		{path: "Makefile", want: "application/octet-stream"},
		// The fixed table is matched ignoring case too.
		{path: "A.MP4", want: "video/mp4"},
		{path: "a.LOG", want: "text/x-log"},
		{path: "a.WebM", want: "video/webm"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, detectContentType(tt.path))
		})
	}
}

func TestChromeEpochTime(t *testing.T) {
	t.Parallel()
	assert.True(t, chromeEpochTime(0).IsZero(), "0 means no stored expiry")
	// 1601-01-01 plus 11644473600 s is the Unix epoch.
	assert.Equal(t, time.Unix(0, 0).UTC(), chromeEpochTime(11644473600*1_000_000))
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), chromeEpochTime((time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()+11644473600)*1_000_000))
}

func TestValidateSessionValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		wantErr bool
	}{
		{in: "abc-_~.XYZ09", wantErr: false},
		{in: "", wantErr: true},
		{in: "a b", wantErr: true},
		{in: "a;b", wantErr: true},
		{in: "a,b", wantErr: true},
		{in: `a"b`, wantErr: true},
		{in: "a\x7fb", wantErr: true},
		{in: "é", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			err := validateSessionValue(tt.in)
			assert.Equal(t, tt.wantErr, err != nil, "err: %v", err)
		})
	}
}

func TestVersionTag(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "v1", versionTag([]byte("v1")))
	assert.Equal(t, "v20", versionTag([]byte("v20xxxxxxxx")))
}

// TestChromeGuardOffMacOS pins the platform guard. It only runs where the
// guard fires: on macOS the same call would read the real keychain.
func TestChromeGuardOffMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("on macOS this would read the real Chrome keychain item")
	}
	_, err := chromeUserSession("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading cookies from Chrome is supported on macOS only")
	assert.Contains(t, err.Error(), "use: ghattach login --cookie -")
}

func TestReadChromeSessionRowsNoStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := readChromeSessionRows(dir, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Chrome cookie store under "+dir+" — is Chrome installed and signed in?")
	_, err = readChromeSessionRows(dir, "Work")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no cookie store for Chrome profile "Work" under `+dir)
}

// TestReadChromeSessionRowsFromFakeStores builds throwaway Chrome-shaped
// SQLite stores (never a real profile) and pins how profiles combine.
func TestReadChromeSessionRowsFromFakeStores(t *testing.T) {
	if _, err := os.Stat(sqlite3Bin); err != nil {
		t.Skipf("%s not present", sqlite3Bin)
	}
	makeStore := func(t *testing.T, dir, profile, sql string) {
		t.Helper()
		p := filepath.Join(dir, profile)
		require.NoError(t, os.MkdirAll(p, 0o700))
		out, err := exec.Command(sqlite3Bin, filepath.Join(p, "Cookies"), //nolint:gosec // fixed binary, test temp dir
			"CREATE TABLE cookies (host_key TEXT, name TEXT, encrypted_value BLOB, expires_utc INTEGER);"+sql).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	row := "INSERT INTO cookies VALUES ('github.com','user_session',X'763130AB',13400000000000000);"
	tests := []struct {
		name         string
		setup        func(t *testing.T, dir string)
		profile      string
		wantProfiles []string
		wantErr      string
	}{
		{
			name: "one row per signed-in profile",
			setup: func(t *testing.T, dir string) {
				makeStore(t, dir, "Default", row)
				makeStore(t, dir, "Profile 1", row)
			},
			wantProfiles: []string{"Default", "Profile 1"},
		},
		{
			name: "a signed-out profile yields no row and no error",
			setup: func(t *testing.T, dir string) {
				makeStore(t, dir, "Default", row)
				makeStore(t, dir, "Profile 1", "")
			},
			wantProfiles: []string{"Default"},
		},
		{
			name: "only other hosts' cookies count as signed out",
			setup: func(t *testing.T, dir string) {
				makeStore(t, dir, "Default", "INSERT INTO cookies VALUES ('other.example','user_session',X'76313041',0);")
			},
			wantProfiles: nil,
		},
		{
			name: "lookalike hosts are not github.com",
			setup: func(t *testing.T, dir string) {
				makeStore(t, dir, "Default", "INSERT INTO cookies VALUES ('.github.com','user_session',X'763130AB',13400000000000000);"+
					"INSERT INTO cookies VALUES ('gist.github.com','user_session',X'763130AB',13400000000000000);")
			},
			wantProfiles: nil,
		},
		{
			name: "a broken store does not mask a working one",
			setup: func(t *testing.T, dir string) {
				makeStore(t, dir, "Default", row)
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "Broken"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "Broken", "Cookies"), []byte("not sqlite"), 0o600))
			},
			wantProfiles: []string{"Default"},
		},
		{
			name: "a broken store beside signed-out ones surfaces its error",
			setup: func(t *testing.T, dir string) {
				makeStore(t, dir, "Default", "")
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "Broken"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "Broken", "Cookies"), []byte("not sqlite"), 0o600))
			},
			wantErr: `querying the "Broken" cookie store`,
		},
		{
			name: "a named profile reads only that store",
			setup: func(t *testing.T, dir string) {
				makeStore(t, dir, "Default", row)
				makeStore(t, dir, "Work", row)
			},
			profile:      "Work",
			wantProfiles: []string{"Work"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)
			rows, err := readChromeSessionRows(dir, tt.profile)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			var got []string
			for _, r := range rows {
				got = append(got, r.profile)
				assert.Equal(t, []byte{'v', '1', '0', 0xAB}, r.encrypted)
				assert.Equal(t, 2025, r.expires.Year())
			}
			assert.Equal(t, tt.wantProfiles, got)
		})
	}
}

// TestFinalizeEmptyNameUsesTheLocalName pins that when GitHub returns no
// name, the embed uses the local file name rather than an empty link text.
func TestFinalizeEmptyNameUsesTheLocalName(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /upload/assets/42", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"href":"https://github.com/user-attachments/assets/u"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	policy := &policyResponse{AssetUploadURL: "/upload/assets/42", AssetUploadAuthenticityToken: "fin"}
	policy.Asset.ContentType = "image/png"
	res, err := NewClient("s", srv.URL).finalize("acme", "widgets", "shot.png", policy)
	require.NoError(t, err)
	assert.Equal(t, "![shot.png](https://github.com/user-attachments/assets/u)", res.Markdown)
}

// TestServerTextInErrorsHasNoControlBytes pins that a body quoted into an
// error loses its control bytes, so the error stays on one line and cannot
// send escape sequences to the terminal.
func TestServerTextInErrorsHasNoControlBytes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/policies/assets", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, "{\"errors\":\n[\"line two\"]}\n\x1b[31mred\xff")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, err := NewClient("s", srv.URL).requestPolicy("acme", "widgets", "tok", 7, "a.png", 3, "image/png")
	require.Error(t, err)
	assert.Equal(t, "policy endpoint returned 422 (want 201): {\"errors\": [\"line two\"]}  [31mred ", err.Error())
}

// TestPolicyFallsBackToTheSentMediaType pins that an asset record with no
// media type takes the one ghattach sent, so a .png still embeds as an image.
func TestPolicyFallsBackToTheSentMediaType(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/policies/assets", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"upload_url":"http://s3.invalid","asset":{"id":42},"form":{"key":"k"},`+
			`"asset_upload_url":"/upload/assets/42","asset_upload_authenticity_token":"fin"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	policy, err := NewClient("s", srv.URL).requestPolicy("acme", "widgets", "tok", 7, "a.png", 3, "image/png")
	require.NoError(t, err)
	assert.Equal(t, "image/png", policy.Asset.ContentType)
}

// TestS3UploadStreamsTheFile pins that the storage upload reads the file as
// it sends it rather than copying it into memory first. It must not run in
// parallel, since it measures the process's allocations.
func TestS3UploadStreamsTheFile(t *testing.T) {
	const size = 64 << 20
	p := filepath.Join(t.TempDir(), "big.mp4")
	f, err := os.Create(p) //nolint:gosec // test temp dir
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	var got int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	policy := &policyResponse{UploadURL: srv.URL, Form: map[string]string{"key": "k"}}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	require.NoError(t, NewClient("s", srv.URL).uploadToS3(policy, p, "big.mp4", size))
	runtime.ReadMemStats(&after)
	assert.Greater(t, got, int64(size), "the whole file and the form reach storage")
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(size/4), "the file is not buffered in memory")
}

// TestStoreSessionConcurrentWriters pins that runs storing a session at the
// same moment all succeed: a write never deletes another writer's temp file.
func TestStoreSessionConcurrentWriters(t *testing.T) {
	tempHome(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := storeSession(fmt.Sprintf("sess-%d", i), "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

// TestStoreSessionSweepsOnlyStaleTempCopies pins that a write deletes a temp
// copy left by a crashed write, but not one another writer has just created.
func TestStoreSessionSweepsOnlyStaleTempCopies(t *testing.T) {
	home := tempHome(t)
	dir := filepath.Dir(storePath(home))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	stale := filepath.Join(dir, "cookies-1.json")
	fresh := filepath.Join(dir, "cookies-2.json")
	require.NoError(t, os.WriteFile(stale, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(fresh, []byte("in flight"), 0o600))
	old := time.Now().Add(-2 * time.Minute)
	require.NoError(t, os.Chtimes(stale, old, old))

	_, err := storeSession("mine", "")
	require.NoError(t, err)
	assert.NoFileExists(t, stale)
	assert.FileExists(t, fresh)
	assert.Equal(t, "mine", readStore(t, home).UserSession)
}

// TestStoreSessionTightensAnExistingDirectory pins that the store directory
// ends up 0700 even when it existed with a wider mode.
func TestStoreSessionTightensAnExistingDirectory(t *testing.T) {
	home := tempHome(t)
	dir := filepath.Dir(storePath(home))
	require.NoError(t, os.MkdirAll(dir, 0o755)) //nolint:gosec // G301: the test needs a group-readable dir
	require.NoError(t, os.Chmod(dir, 0o755))    //nolint:gosec // G302: the test needs a group-readable dir
	_, err := storeSession("sess", "")
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// TestChromeUserDataDirIsChromeStable pins the one browser directory read:
// Google Chrome's own, not Beta, Canary or another Chromium browser.
func TestChromeUserDataDirIsChromeStable(t *testing.T) {
	home := tempHome(t)
	got, err := chromeUserDataDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "Library", "Application Support", "Google", "Chrome"), got)
}
