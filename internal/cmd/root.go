// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package cmd implements the ghattach commands. upload sends local files to
// GitHub's user-attachments storage and prints each URL or markdown embed.
// login and logout manage the user_session cookie stored in
// ~/.config/ghattach/cookies.json. Parts of client.go are adapted from
// gh-image; see NOTICE.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "ghattach",
	Short: "Upload files to GitHub user-attachments for PR/issue embeds",
	Long: `ghattach uploads local files to GitHub's user-attachments CDN.

It prints the same https://github.com/user-attachments/assets/<uuid> URLs
that dragging a file into a pull request description gives, without
committing anything. Uploads inherit the target repository's visibility.

First-time setup: ghattach login --help

Then:
  ghattach upload ./shot.png --repo OWNER/REPO
  ghattach upload ./shot.png --repo OWNER/REPO --markdown

The session cookie expires from time to time. On macOS, an upload that
finds it expired re-reads Chrome once and retries.`,
	// Every error, a flag-parse error included, prints once via Execute as
	// "Error: <msg>"; the usage text is never printed on an error.
	SilenceErrors: true,
	SilenceUsage:  true,
}

// Execute runs the root command with the given version string.
func Execute(version string) {
	rootCmd.Version = version
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, errorLine(err))
		os.Exit(1)
	}
}

// errorLine is the one line printed for a failure. It can quote a file name
// or a server's answer, so control characters are shown as spaces.
func errorLine(err error) string {
	return "Error: " + printable([]byte(err.Error()))
}
