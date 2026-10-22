// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT
//
// Portions adapted from gh-image, Copyright (c) 2025-2026 drogers0,
// MIT License; see NOTICE and third_party/gh-image/LICENSE.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// githubBaseURL is where uploads go. It is a variable only so that command
// tests can point the CLI at a local stub server.
var githubBaseURL = "https://github.com"

const (
	userSessionCookie = "user_session"
	// maxUploadBytes is GitHub's largest attachment cap, 100 MiB for video;
	// larger files would be rejected server-side after a long transfer, so
	// fail them locally first.
	maxUploadBytes = 100 << 20
)

// errSessionExpired marks a failure caused by a dead user_session, letting
// upload re-derive the cookie from the browser and retry instead of making the
// user log in again by hand.
var errSessionExpired = errors.New("expired session (re-run: ghattach login)")

// Client replays GitHub's browser asset-upload pipeline. baseURL has no
// trailing slash; production is https://github.com, tests point it at an
// httptest server.
type Client struct {
	http    *http.Client
	baseURL string
	session string
	// tokenCache holds the per-repo uploadToken so a multi-file invocation
	// fetches the (large) repo page once, keyed "owner/repo".
	tokenCache map[string]string
}

// NewClient builds a Client authenticated as the given user_session. The
// client never follows redirects: no pipeline endpoint redirects on success,
// so surfacing the 3xx keeps the session cookie off any redirect target and
// lets callers report the real cause (an expired session redirects to /login,
// which would otherwise be misreported as a wrong-status error).
func NewClient(session, baseURL string) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		baseURL:    strings.TrimRight(baseURL, "/"),
		session:    session,
		tokenCache: map[string]string{},
	}
}

// addSessionCookies attaches the session pair to a GitHub-bound request.
// GitHub's CSRF validation requires the __Host-user_session_same_site twin
// alongside user_session, with the same value; the pair is set per-request
// (not via a cookie jar) because jar prefix rules would drop the __Host-
// cookie unless the source URL is https, which test servers are not.
func (c *Client) addSessionCookies(req *http.Request) {
	// These are request cookies: Secure, HttpOnly and SameSite belong to
	// Set-Cookie and are never sent, so gosec's G124 does not apply.
	req.AddCookie(&http.Cookie{Name: userSessionCookie, Value: c.session})               //nolint:gosec // G124, see above
	req.AddCookie(&http.Cookie{Name: "__Host-user_session_same_site", Value: c.session}) //nolint:gosec // G124, see above
}

// isRedirect reports whether status is a redirect that carries a Location.
func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// isAuthFailure reports whether a response proves the session is no longer
// valid — an explicit 401, or a redirect to the login page. The
// policy and finalize steps run after uploadToken has been cached, so a session
// that dies mid-run surfaces there rather than on the repo page; without this
// they would return a plain status error and the caller's self-heal would never
// fire.
func isAuthFailure(resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnauthorized {
		return true
	}
	return isRedirect(resp.StatusCode) && isLoginLocation(resp.Header.Get("Location"))
}

// isLoginLocation reports whether a redirect target is GitHub's login page:
// the path /login, ignoring case and a trailing slash, relative or on
// github.com or www.github.com with no port other than 443. A path that merely
// starts with /login, such as a renamed repo /acme/login-service or an owner
// named loginradius, is a move, not an expired session.
func isLoginLocation(location string) bool {
	u, err := url.Parse(location)
	if err != nil {
		// Unreachable through the real client, which rejects an unparseable
		// Location header before a response gets here; not expiry either way.
		return false
	}
	if u.Host != "" {
		host := strings.ToLower(u.Hostname())
		if host != "github.com" && host != "www.github.com" {
			return false
		}
		if port := u.Port(); port != "" && port != "443" {
			return false
		}
	}
	return strings.EqualFold(strings.TrimSuffix(u.Path, "/"), "/login")
}

// printable makes text quoted from a server or from gh safe to print: every
// control character and invalid byte becomes a space, so an error stays on
// one line and cannot send escape sequences to the terminal.
func printable(b []byte) string {
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, string(b))
}

// Result is a finalized upload: the CDN URL and the markdown reference GitHub
// itself would produce for the asset's media type.
type Result struct {
	URL      string
	Markdown string
}

// policyResponse is the JSON body of POST /upload/policies/assets: the
// presigned S3 form plus the finalize path/token for the created asset.
type policyResponse struct {
	UploadURL string `json:"upload_url"`
	Asset     struct {
		ID          int    `json:"id"`
		ContentType string `json:"content_type"`
	} `json:"asset"`
	Form                         map[string]string `json:"form"`
	AssetUploadURL               string            `json:"asset_upload_url"`
	AssetUploadAuthenticityToken string            `json:"asset_upload_authenticity_token"`
}

var uploadTokenRe = regexp.MustCompile(`"uploadToken":"([^"]+)"`)

// uploadToken fetches the repo page and extracts the per-repo uploadToken from
// its embedded JS payload — the authenticity_token the policy endpoint
// requires. Present only for a session with access to the repo; an org that
// enforces SAML SSO serves an interstitial (HTTP 200, no token) until the
// session is SSO-authorized in a browser. Cached per owner/repo.
func (c *Client) uploadToken(owner, repo string) (string, error) {
	cacheKey := owner + "/" + repo
	if tok, ok := c.tokenCache[cacheKey]; ok {
		return tok, nil
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("%s/%s/%s", c.baseURL, owner, repo), nil)
	if err != nil {
		return "", fmt.Errorf("creating repo page request: %w", err)
	}
	c.addSessionCookies(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching repo page: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode != http.StatusOK {
		// GitHub distinguishes the two failures, so this must not conflate them:
		// a *present but invalid* session is redirected to /login, while 404 is
		// no-access-or-absent (what an anonymous request gets, since GitHub hides
		// private repos rather than admitting they exist). Only the former is
		// expiry, so only it carries the sentinel — a 404 must never trigger a
		// re-read that cannot possibly fix it.
		if isAuthFailure(resp) {
			return "", fmt.Errorf("repo page returned %d — %w", resp.StatusCode, errSessionExpired)
		}
		// Any other redirect points at a repo path: renamed or transferred.
		if loc := resp.Header.Get("Location"); isRedirect(resp.StatusCode) && loc != "" {
			return "", fmt.Errorf("repo page returned %d to %s — repo moved or renamed; retry with the new --repo", resp.StatusCode, printable([]byte(loc)))
		}
		return "", fmt.Errorf("repo page returned %d — no access to %s/%s, or it does not exist; check the --repo spelling and your access to it", resp.StatusCode, owner, repo)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading repo page: %w", err)
	}
	m := uploadTokenRe.FindSubmatch(body)
	if m == nil {
		if regexp.MustCompile(`(?i)/orgs/` + regexp.QuoteMeta(owner) + `/sso`).Match(body) {
			return "", fmt.Errorf("%s enforces SAML SSO and this session is not authorized — visit https://github.com/orgs/%s/sso in a browser, then retry", owner, owner)
		}
		return "", fmt.Errorf("uploadToken not found on repo page — does this account have write access to %s/%s?", owner, repo)
	}
	// The capture is the JSON-escaped form; run it through the JSON decoder so
	// an escaped serialization (+, \/) reaches GitHub unescaped.
	var tok string
	// The unescape is best-effort: an unparseable capture falls back to the raw
	// bytes, which match unescaped tokens.
	if err := json.Unmarshal([]byte(`"`+string(m[1])+`"`), &tok); err != nil {
		tok = string(m[1])
	}
	c.tokenCache[cacheKey] = tok
	return tok, nil
}

// checkUploadFile returns the size of path, or why it cannot be uploaded: it
// is missing, not a regular file, empty, or over the size cap.
func checkUploadFile(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() == 0 {
		return 0, fmt.Errorf("%s is empty", path)
	}
	if info.Size() > maxUploadBytes {
		return 0, fmt.Errorf("%s is %d bytes — over GitHub's %d MiB attachment cap", path, info.Size(), maxUploadBytes>>20)
	}
	return info.Size(), nil
}

// Upload runs the full pipeline for one file and returns the asset URL.
// repoID is the repository's numeric database ID; uploads are repo-scoped so
// the asset inherits that repository's visibility.
func (c *Client) Upload(owner, repo string, repoID int, path string) (*Result, error) {
	size, err := checkUploadFile(path)
	if err != nil {
		return nil, err
	}
	contentType := detectContentType(path)
	fileName := sanitizeFileName(filepath.Base(path))

	token, err := c.uploadToken(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("get upload token: %w", err)
	}
	policy, err := c.requestPolicy(owner, repo, token, repoID, fileName, size, contentType)
	if err != nil {
		return nil, fmt.Errorf("request upload policy: %w", err)
	}
	if err := c.uploadToS3(policy, path, fileName, size); err != nil {
		return nil, fmt.Errorf("upload to S3: %w", err)
	}
	result, err := c.finalize(owner, repo, fileName, policy)
	if err != nil {
		return nil, fmt.Errorf("finalize upload: %w", err)
	}
	return result, nil
}

// browserHeaders sets the headers GitHub's CSRF/XHR checks require on the
// policy and finalize requests.
func (c *Client) browserHeaders(req *http.Request, owner, repo, contentType string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", c.baseURL)
	req.Header.Set("Referer", fmt.Sprintf("%s/%s/%s", c.baseURL, owner, repo))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
}

func (c *Client) requestPolicy(owner, repo, token string, repoID int, fileName string, size int64, contentType string) (*policyResponse, error) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	for _, f := range [][2]string{
		{"name", fileName},
		{"size", strconv.FormatInt(size, 10)},
		{"content_type", contentType},
		{"authenticity_token", token},
		{"repository_id", strconv.Itoa(repoID)},
	} {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, fmt.Errorf("writing form field %s: %w", f[0], err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("closing multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.baseURL+"/upload/policies/assets", body)
	if err != nil {
		return nil, fmt.Errorf("creating policy request: %w", err)
	}
	c.browserHeaders(req, owner, repo, w.FormDataContentType())
	c.addSessionCookies(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("policy request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode != http.StatusCreated {
		if isAuthFailure(resp) {
			return nil, fmt.Errorf("policy endpoint returned %d — %w", resp.StatusCode, errSessionExpired)
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 300)) //nolint:errcheck // best-effort error detail
		return nil, fmt.Errorf("policy endpoint returned %d (want 201): %s", resp.StatusCode, printable(respBody))
	}
	var policy policyResponse
	if err := json.NewDecoder(resp.Body).Decode(&policy); err != nil {
		return nil, fmt.Errorf("decoding policy response: %w", err)
	}
	switch {
	case policy.UploadURL == "":
		return nil, fmt.Errorf("policy response missing upload_url")
	case len(policy.Form) == 0:
		return nil, fmt.Errorf("policy response missing form fields")
	case !strings.HasPrefix(policy.AssetUploadURL, "/") || strings.HasPrefix(policy.AssetUploadURL, "//"):
		// Root-relative only: "//host/path" is protocol-relative and, while
		// string concatenation onto the absolute base targets github.com,
		// rejecting it keeps the session-cookie invariant from
		// resting on a URL-joining subtlety.
		return nil, fmt.Errorf("policy response asset_upload_url %q is not a root-relative path", policy.AssetUploadURL)
	case policy.AssetUploadAuthenticityToken == "":
		return nil, fmt.Errorf("policy response missing asset_upload_authenticity_token")
	case policy.Asset.ID == 0:
		return nil, fmt.Errorf("policy response missing asset id")
	}
	// The embed follows the recorded media type; an asset record without one
	// was created from the type sent here.
	if policy.Asset.ContentType == "" {
		policy.Asset.ContentType = contentType
	}
	return &policy, nil
}

// s3FieldOrder lists the presigned-form fields in the order the browser sends
// them; S3 policy uploads can be order-sensitive and the file part must come
// last.
var s3FieldOrder = []string{
	"key", "acl", "policy",
	"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Signature",
	"Content-Type", "Cache-Control", "x-amz-meta-Surrogate-Control",
}

// uploadToS3 POSTs the presigned form + file. The request deliberately sends
// no GitHub cookies — the presigned policy is the auth — and its deadline
// scales with file size so a large video on a modest uplink is not killed by
// a flat timeout (floor 2 min, +1 min per 8 MB ≈ survives ~1 Mbps). The file
// is streamed between the form's head and tail, with the total length set,
// because storage refuses a chunked POST.
func (c *Client) uploadToS3(policy *policyResponse, path, fileName string, size int64) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	written := make(map[string]bool, len(policy.Form))
	for _, k := range s3FieldOrder {
		v, ok := policy.Form[k]
		if !ok {
			continue
		}
		if err := w.WriteField(k, v); err != nil {
			return fmt.Errorf("writing form field %s: %w", k, err)
		}
		written[k] = true
	}
	for k, v := range policy.Form {
		if written[k] {
			continue
		}
		if err := w.WriteField(k, v); err != nil {
			return fmt.Errorf("writing form field %s: %w", k, err)
		}
	}
	if _, err := w.CreateFormFile("file", fileName); err != nil {
		return fmt.Errorf("creating file field: %w", err)
	}
	head := bytes.Clone(buf.Bytes())
	buf.Reset()
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing multipart writer: %w", err)
	}
	tail := buf.Bytes()
	f, err := os.Open(path) //nolint:gosec // CLI tool reads a user-provided file path
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	body := io.MultiReader(bytes.NewReader(head), f, bytes.NewReader(tail))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, policy.UploadURL, body)
	if err != nil {
		return fmt.Errorf("creating S3 request: %w", err)
	}
	// A file that changed size since the stat fails the request instead of
	// sending a body that disagrees with the declared policy size.
	req.ContentLength = int64(len(head)) + size + int64(len(tail))
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", c.baseURL)

	resp, err := newS3Client(s3Timeout(size)).Do(req)
	if err != nil {
		return fmt.Errorf("S3 upload request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 300)) //nolint:errcheck // best-effort error detail
		return fmt.Errorf("S3 returned %d: %s", resp.StatusCode, printable(respBody))
	}
	return nil
}

// newS3Client builds the client for the S3 upload, which carries no cookies. It
// is a variable only so that tests can see the deadline the upload gets.
var newS3Client = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// s3Timeout is the S3 upload deadline for a file of size bytes: 2 minutes,
// plus 1 minute per started 8 MiB.
func s3Timeout(size int64) time.Duration {
	return 2*time.Minute + time.Duration((size+(8<<20)-1)>>23)*time.Minute
}

// assetURLRe is the only shape of URL finalize may return: anything else is
// not a user-attachments asset, and could break or hijack the printed embed.
var assetURLRe = regexp.MustCompile(`^https://github\.com/user-attachments/assets/[A-Za-z0-9-]+$`)

// finalize completes the upload. fileName is the name sent with the policy,
// used for the embed when GitHub's answer carries none.
func (c *Client) finalize(owner, repo, fileName string, policy *policyResponse) (*Result, error) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	if err := w.WriteField("authenticity_token", policy.AssetUploadAuthenticityToken); err != nil {
		return nil, fmt.Errorf("writing form field authenticity_token: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("closing multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, c.baseURL+policy.AssetUploadURL, body)
	if err != nil {
		return nil, fmt.Errorf("creating finalize request: %w", err)
	}
	c.browserHeaders(req, owner, repo, w.FormDataContentType())
	c.addSessionCookies(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("finalize request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode != http.StatusOK {
		if isAuthFailure(resp) {
			return nil, fmt.Errorf("finalize returned %d — %w", resp.StatusCode, errSessionExpired)
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 300)) //nolint:errcheck // best-effort error detail
		return nil, fmt.Errorf("finalize returned %d (want 200): %s", resp.StatusCode, printable(respBody))
	}
	var out struct {
		Href string `json:"href"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding finalize response: %w", err)
	}
	if out.Href == "" {
		return nil, fmt.Errorf("finalize response missing href")
	}
	if !assetURLRe.MatchString(out.Href) {
		return nil, fmt.Errorf("finalize response href %q is not a user-attachments URL", out.Href)
	}
	name := out.Name
	if name == "" {
		name = fileName
	}
	return &Result{
		URL:      out.Href,
		Markdown: renderMarkdown(name, out.Href, policy.Asset.ContentType),
	}, nil
}

// renderMarkdown mirrors what GitHub inserts on drag-and-drop: an image embed
// for images, the bare URL for videos (GitHub renders an inline player only
// for a bare user-attachments URL on its own line), a plain link otherwise.
// The name is escaped so that it cannot end the link text early.
func renderMarkdown(name, href, contentType string) string {
	text := markdownEscaper.Replace(printable([]byte(name)))
	contentType = strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return fmt.Sprintf("![%s](%s)", text, href)
	case strings.HasPrefix(contentType, "video/"):
		return href
	default:
		return fmt.Sprintf("[%s](%s)", text, href)
	}
}

var markdownEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)

// sanitizeFileName removes every control character, which would otherwise
// reach the multipart Content-Disposition header raw (Go escapes only quotes
// and backslashes) and produce an undiagnosable S3 400. Bytes that are not
// valid UTF-8 are kept as they are.
func sanitizeFileName(name string) string {
	b := make([]byte, 0, len(name))
	for len(name) > 0 {
		r, size := utf8.DecodeRuneInString(name)
		if (r != utf8.RuneError || size != 1) && unicode.IsControl(r) {
			name = name[size:]
			continue
		}
		b = append(b, name[:size]...)
		name = name[size:]
	}
	return string(b)
}

// githubContentTypes pins the media types GitHub's policy endpoint expects for
// extensions where Go/OS mime data is absent or wrong. Two failure modes it
// covers: video/archive types missing from Go's builtin table (~16 web types;
// OS mime files fill the rest but minimal Linux images lack them), and .log,
// where GitHub validates content_type against the extension and 422s anything
// but its non-standard text/x-log (the OS answer is text/plain). Checked
// BEFORE the platform lookup so the answer is deterministic everywhere.
var githubContentTypes = map[string]string{
	".mp4":  "video/mp4",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".txt":  "text/plain",
	".zip":  "application/zip",
	".log":  "text/x-log",
}

// detectContentType maps the file extension to the bare media type GitHub's
// policy endpoint validates against (parameters like charset are rejected).
func detectContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ct, ok := githubContentTypes[ext]; ok {
		return ct
	}
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		return "application/octet-stream"
	}
	// Parameter stripping is best-effort: an unparseable OS mime entry ships
	// as is, and GitHub's validation is the backstop.
	if mediaType, _, err := mime.ParseMediaType(ct); err == nil {
		return mediaType
	}
	return ct
}
