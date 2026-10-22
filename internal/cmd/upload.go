// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	uploadRepo     string
	uploadRepoID   int
	uploadMarkdown bool
)

var uploadCmd = &cobra.Command{
	Use:   "upload <file> [<file>...]",
	Short: "Upload files to GitHub user-attachments and print their URLs",
	Long: `Upload one or more local files to GitHub's user-attachments CDN.

Each upload prints the asset URL. With --markdown it prints the embed GitHub
inserts on drag-and-drop, which for a video is the bare URL. An upload
inherits the repository's visibility. On a private repository, only people
with access to it can open the link.

Examples:
  ghattach upload before.png after.png --repo OWNER/REPO
  ghattach upload demo.mp4 --repo OWNER/REPO --markdown`,
	Args: cobra.MinimumNArgs(1),
	RunE: runUpload,
}

func init() {
	uploadCmd.Flags().StringVarP(&uploadRepo, "repo", "R", "", "target repository as OWNER/REPO (required)")
	uploadCmd.Flags().IntVar(&uploadRepoID, "repo-id", 0, "numeric repository ID (skips the gh lookup)")
	uploadCmd.Flags().BoolVar(&uploadMarkdown, "markdown", false, "print the markdown embed instead of the bare URL")
	cobra.CheckErr(uploadCmd.MarkFlagRequired("repo"))
	rootCmd.AddCommand(uploadCmd)
}

// resolveRepoID resolves the repository's numeric database ID via the gh CLI
// (the one official-API step in the flow). --repo-id skips it for
// environments without gh.
func resolveRepoID(ownerRepo string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A fixed argv with no shell; ownerRepo is the user's own flag value.
	out, err := exec.CommandContext(ctx, "gh", "api", "repos/"+ownerRepo, "--jq", ".id").Output() //nolint:gosec // G204, see above
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return 0, fmt.Errorf("resolving repo id via gh (pass --repo-id to skip): %s: %w", strings.TrimSpace(printable(exitErr.Stderr)), err)
		}
		return 0, fmt.Errorf("resolving repo id via gh (pass --repo-id to skip): %w", err)
	}
	id, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("unexpected gh api output %q: %w", strings.TrimSpace(string(out)), err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("unexpected gh api output %q: not a repository id", strings.TrimSpace(string(out)))
	}
	return id, nil
}

// refreshSessionFromChrome re-derives the session from Chrome and, when it
// differs from the stored one, persists it, so the recovered cookie also serves
// the next invocation. With a pin in the store, the re-read comes from the
// profile the user chose, so the retry uploads as the same GitHub account.
// Without one (every --cookie login, and --from-chrome without
// --chrome-profile), the freshest profile wins, and that may be a different
// account.
func refreshSessionFromChrome(store cookieStore) (string, error) {
	session, err := readChromeSession(store.ChromeProfile)
	if err != nil {
		return "", fmt.Errorf("re-reading the session from Chrome: %w", err)
	}
	if session == store.UserSession {
		return session, nil
	}
	if _, err := storeSession(session, store.ChromeProfile); err != nil {
		return "", fmt.Errorf("storing the refreshed session: %w", err)
	}
	return session, nil
}

// repoRe is the shape of OWNER/REPO: the characters GitHub allows in an
// account and in a repository name, and nothing that would change the page
// URL built from it.
var repoRe = regexp.MustCompile(`^[A-Za-z0-9_-]+/[A-Za-z0-9._-]+$`)

func runUpload(_ *cobra.Command, args []string) error {
	owner, repo, _ := strings.Cut(uploadRepo, "/")
	if !repoRe.MatchString(uploadRepo) || repo == "." || repo == ".." {
		return fmt.Errorf("--repo must be OWNER/REPO, got %q", uploadRepo)
	}
	if uploadRepoID < 0 {
		return fmt.Errorf("--repo-id must be a positive repository id, got %d", uploadRepoID)
	}

	store, err := loadStore()
	if err != nil {
		return fmt.Errorf("loading session: %w", err)
	}
	// Every file is checked before any request, so a bad later argument
	// leaves nothing uploaded. Upload checks each one again just before
	// sending it, since a file can change in between.
	for _, path := range args {
		if _, err := checkUploadFile(path); err != nil {
			return fmt.Errorf("uploading %s: %w", path, err)
		}
	}

	repoID := uploadRepoID
	if repoID == 0 {
		repoID, err = resolveRepoID(uploadRepo)
		if err != nil {
			return fmt.Errorf("resolving repo: %w", err)
		}
	}

	client := NewClient(store.UserSession, githubBaseURL)
	// A rolled session self-heals from the browser's own copy. Re-read Chrome
	// at most once per invocation: a second expiry in the same run means the
	// browser is signed out too, and retrying would only stall on a keychain
	// prompt per file.
	refreshed := false
	for _, path := range args {
		result, err := client.Upload(owner, repo, repoID, path)
		if errors.Is(err, errSessionExpired) && !refreshed {
			refreshed = true
			// Expiry is inferred from a 401 or a redirect to /login. Only Chrome
			// holding a different cookie confirms it, so the same cookie or an
			// unreadable Chrome reports the original error.
			fresh, refreshErr := refreshSessionFromChrome(store)
			switch {
			case refreshErr != nil:
				return fmt.Errorf("uploading %s: %w (could not self-heal: %w)", path, err, refreshErr)
			case fresh == store.UserSession:
				return fmt.Errorf("uploading %s: %w", path, err)
			}
			fmt.Fprintln(os.Stderr, "Re-read the session from Chrome; retrying.")
			client = NewClient(fresh, githubBaseURL)
			result, err = client.Upload(owner, repo, repoID, path)
		}
		if err != nil {
			return fmt.Errorf("uploading %s: %w", path, err)
		}
		line := result.URL
		if uploadMarkdown {
			line = result.Markdown
		}
		// The upload is real and cannot be deleted, so a line that cannot be
		// printed must fail the run rather than vanish.
		if _, err := fmt.Println(line); err != nil {
			return fmt.Errorf("writing the result for %s: %w", path, err)
		}
	}
	return nil
}
