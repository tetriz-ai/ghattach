// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Command ghattach uploads local files to GitHub's user-attachments CDN and
// prints the URL or markdown to embed in a pull request or issue body.
package main

import (
	"runtime/debug"

	"github.com/tetriz-ai/ghattach/internal/cmd"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3". When it
// is empty, the version recorded in the binary's build info is used.
var version string

func main() {
	cmd.Execute(resolveVersion(version, debug.ReadBuildInfo))
}

// resolveVersion prefers the version set by the linker, then the main
// module's version from the build info: the tag for a `go install ...@v1.2.3`,
// a pseudo-version or "(devel)" for a build from a checkout.
func resolveVersion(linked string, readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if linked != "" {
		return linked
	}
	if info, ok := readBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
