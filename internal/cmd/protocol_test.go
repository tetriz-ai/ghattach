// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeFakeChromeStore writes a throwaway Chrome-shaped cookie store (never a
// real profile) for profile under dir, running sql against it.
func makeFakeChromeStore(t *testing.T, dir, profile, sql string) string {
	t.Helper()
	p := filepath.Join(dir, profile)
	require.NoError(t, os.MkdirAll(p, 0o700))
	store := filepath.Join(p, "Cookies")
	out, err := exec.Command(sqlite3Bin, store, //nolint:gosec // fixed binary, test temp dir
		"CREATE TABLE cookies (host_key TEXT, name TEXT, encrypted_value BLOB, expires_utc INTEGER);"+sql).CombinedOutput()
	require.NoError(t, err, string(out))
	return store
}

// recordedRequest is one request the protocol stub saw.
type recordedRequest struct {
	step    string
	cookies map[string]string
	header  http.Header
	form    map[string]string
}

// protocolStub serves the four upload steps and records every request, in
// order, with its cookies, headers and form fields.
type protocolStub struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	repoPage string
}

func newProtocolStub(t *testing.T) *protocolStub {
	t.Helper()
	p := &protocolStub{repoPage: `{"uploadToken":"tok"}`}
	record := func(step string, r *http.Request) {
		rec := recordedRequest{step: step, cookies: map[string]string{}, header: r.Header.Clone(), form: map[string]string{}}
		for _, c := range r.Cookies() {
			rec.cookies[c.Name] = c.Value
		}
		if err := r.ParseMultipartForm(1 << 20); err == nil { //nolint:gosec // G120: test stub, bounded at 1 MiB
			for k, v := range r.MultipartForm.Value {
				rec.form[k] = v[0]
			}
		}
		p.mu.Lock()
		p.requests = append(p.requests, rec)
		p.mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /acme/widgets", func(w http.ResponseWriter, r *http.Request) {
		record("repo page", r)
		_, _ = io.WriteString(w, p.repoPage)
	})
	mux.HandleFunc("POST /upload/policies/assets", func(w http.ResponseWriter, r *http.Request) {
		record("policy", r)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"upload_url":                      p.srv.URL + "/s3",
			"asset":                           map[string]any{"id": 42, "content_type": "image/png"},
			"form":                            map[string]string{"key": "k"},
			"asset_upload_url":                "/upload/assets/42",
			"asset_upload_authenticity_token": "fin",
		})
	})
	mux.HandleFunc("POST /s3", func(w http.ResponseWriter, r *http.Request) {
		record("s3", r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /upload/assets/42", func(w http.ResponseWriter, r *http.Request) {
		record("finalize", r)
		_, _ = io.WriteString(w, `{"href":"https://github.com/user-attachments/assets/u","name":"a.png"}`)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *protocolStub) steps() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.requests {
		out = append(out, r.step)
	}
	return out
}

func (p *protocolStub) request(step string) recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.requests {
		if r.step == step {
			return r
		}
	}
	return recordedRequest{}
}

// TestUploadRequestOrderAndPolicyRequest pins the four requests of an upload
// in the order docs/reference.md gives, and what the policy request carries: both
// session cookies, the CSRF headers and the form fields.
func TestUploadRequestOrderAndPolicyRequest(t *testing.T) {
	t.Parallel()
	p := newProtocolStub(t)
	_, err := NewClient("sess", p.srv.URL).Upload("acme", "widgets", 7, writeTempPNG(t))
	require.NoError(t, err)
	assert.Equal(t, []string{"repo page", "policy", "s3", "finalize"}, p.steps())

	policy := p.request("policy")
	assert.Equal(t, "sess", policy.cookies["user_session"])
	assert.Equal(t, "sess", policy.cookies["__Host-user_session_same_site"])
	assert.Equal(t, p.srv.URL, policy.header.Get("Origin"))
	assert.Equal(t, p.srv.URL+"/acme/widgets", policy.header.Get("Referer"))
	assert.Equal(t, "XMLHttpRequest", policy.header.Get("X-Requested-With"))
	assert.Equal(t, "application/json", policy.header.Get("Accept"))
	assert.Equal(t, map[string]string{
		"name": "shot.png", "size": "9", "content_type": "image/png",
		"authenticity_token": "tok", "repository_id": "7",
	}, policy.form)
	assert.Empty(t, p.request("s3").cookies, "no GitHub cookie goes to storage")
	assert.Equal(t, "sess", p.request("finalize").cookies["user_session"])
}

// TestUploadStripsCRLFFromTheSentName pins that Upload itself sends the
// sanitised name, not only the helper.
func TestUploadStripsCRLFFromTheSentName(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a\r\nb.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Skipf("this file system refuses CR/LF in names: %v", err)
	}
	p := newProtocolStub(t)
	_, err := NewClient("sess", p.srv.URL).Upload("acme", "widgets", 7, path)
	require.NoError(t, err)
	assert.Equal(t, "ab.png", p.request("policy").form["name"])
}

// TestSAMLCheckIsCaseInsensitiveAndOwnerSpecific pins the SSO interstitial
// match: the owner is matched ignoring case, and another owner's SSO link is
// not this owner's interstitial.
func TestSAMLCheckIsCaseInsensitiveAndOwnerSpecific(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		page    string
		wantErr string
	}{
		{name: "owner in another case", page: `<a href="/orgs/ACME/sso">Continue</a>`, wantErr: "acme enforces SAML SSO"},
		{name: "another owner's link", page: `<a href="/orgs/other/sso">Continue</a>`, wantErr: "uploadToken not found on repo page"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newProtocolStub(t)
			p.repoPage = tt.page
			_, err := NewClient("sess", p.srv.URL).uploadToken("acme", "widgets")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestS3Timeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		size int64
		want time.Duration
	}{
		{size: 0, want: 2 * time.Minute},
		{size: 1, want: 3 * time.Minute},
		{size: 8 << 20, want: 3 * time.Minute},
		{size: 8<<20 + 1, want: 4 * time.Minute},
		{size: 100 << 20, want: 15 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.size), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, s3Timeout(tt.size))
		})
	}
}

// TestTerminalPasteCap pins the refusal near the 1024-byte terminal line cap:
// 999 bytes are accepted, 1000 and more are refused.
func TestTerminalPasteCap(t *testing.T) {
	tests := []struct {
		size    int
		wantErr bool
	}{
		{size: 999},
		{size: 1000, wantErr: true},
		{size: 1024, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.size), func(t *testing.T) {
			tempHome(t)
			oldTTY, oldRead := stdinIsTerminal, readTerminalLine
			stdinIsTerminal = func() bool { return true }
			readTerminalLine = func() ([]byte, error) { return []byte(strings.Repeat("a", tt.size)), nil }
			t.Cleanup(func() { stdinIsTerminal, readTerminalLine = oldTTY, oldRead })

			res := runCLI(t, "", "login", "--cookie", "-")
			if tt.wantErr {
				require.Error(t, res.err)
				assert.Contains(t, res.err.Error(), "terminal paste near the 1024-byte line cap")
				return
			}
			require.NoError(t, res.err)
			assert.Contains(t, res.stdout, "Session stored at")
		})
	}
}

// TestPolicyStepExpiryRefetchesTheRepoPage pins that a session found dead at
// the policy step, after the upload token was cached, is re-read from Chrome
// and the retry fetches the repo page again with the new session.
func TestPolicyStepExpiryRefetchesTheRepoPage(t *testing.T) {
	tempHome(t)
	_, err := storeSession("sess-a", "")
	require.NoError(t, err)
	s := newCLIStub(t, "sess-a", "sess-b")
	s.policy401Nth = 1
	useStub(t, s)
	useFakeChrome(t, &fakeChrome{session: "sess-b"})
	shot := writeFile(t, t.TempDir(), "shot.png", "png")

	res := runCLI(t, "", "upload", shot, "-R", "acme/widgets", "--repo-id", "7")
	require.NoError(t, res.err)
	assert.Equal(t, []string{"sess-a", "sess-b"}, s.sessions, "the retry fetches the repo page again as sess-b")
}

// TestRollbackJournalIsCopied pins that a store's -journal is copied with it:
// an unreadable journal fails the read instead of being skipped.
func TestRollbackJournalIsCopied(t *testing.T) {
	if _, err := os.Stat(sqlite3Bin); err != nil {
		t.Skipf("%s not present", sqlite3Bin)
	}
	dir := t.TempDir()
	store := makeFakeChromeStore(t, dir, "Default", "")
	require.NoError(t, os.WriteFile(store+"-journal", []byte("x"), 0o000))
	if f, err := os.Open(store + "-journal"); err == nil { //nolint:gosec // test temp file
		_ = f.Close()
		t.Skip("running with permissions that can read a mode-000 file")
	}
	_, _, err := readChromeSessionRow(store, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `copying the "Default" rollback journal`)
	assert.False(t, errors.Is(err, os.ErrNotExist))
}

// TestS3UploadUsesTheSizedDeadline pins the wiring from the file size to the
// deadline the S3 request actually gets.
func TestS3UploadUsesTheSizedDeadline(t *testing.T) {
	p := newProtocolStub(t)
	var got []time.Duration
	old := newS3Client
	newS3Client = func(timeout time.Duration) *http.Client {
		got = append(got, timeout)
		return old(timeout)
	}
	t.Cleanup(func() { newS3Client = old })
	policy := &policyResponse{UploadURL: p.srv.URL + "/s3", Form: map[string]string{"key": "k"}}
	big := filepath.Join(t.TempDir(), "big.mp4")
	f, err := os.Create(big) //nolint:gosec // test temp dir
	require.NoError(t, err)
	require.NoError(t, f.Truncate(100<<20))
	require.NoError(t, f.Close())
	require.NoError(t, NewClient("s", p.srv.URL).uploadToS3(policy, big, "big.mp4", 100<<20))
	assert.Equal(t, []time.Duration{15 * time.Minute}, got)
}

// TestS3UploadFailsWhenTheFileChangedSize pins that a file whose size changed
// since it was checked fails the upload, rather than sending a body that
// disagrees with the size declared to GitHub.
func TestS3UploadFailsWhenTheFileChangedSize(t *testing.T) {
	t.Parallel()
	p := newProtocolStub(t)
	policy := &policyResponse{UploadURL: p.srv.URL + "/s3", Form: map[string]string{"key": "k"}}
	err := NewClient("s", p.srv.URL).uploadToS3(policy, writeTempPNG(t), "shot.png", 1<<20)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "S3 upload request")
}

// TestUploadTokenUnescapeFallsBackToRawBytes pins the best-effort unescape: a
// capture that is not a valid JSON string is sent as the raw bytes.
func TestUploadTokenUnescapeFallsBackToRawBytes(t *testing.T) {
	t.Parallel()
	p := newProtocolStub(t)
	p.repoPage = `{"uploadToken":"ab\x"}`
	tok, err := NewClient("s", p.srv.URL).uploadToken("acme", "widgets")
	require.NoError(t, err)
	assert.Equal(t, `ab\x`, tok)
}
