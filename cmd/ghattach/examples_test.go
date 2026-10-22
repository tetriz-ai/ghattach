// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuickStartExampleMatchesReadme keeps examples/embed-one-screenshot and the
// README's Quick start the same program. Every command the Quick start shows
// runs in the example's run.sh, in order, and the example's expected output
// is the Quick start's output. A line one side has and the other cannot is
// listed below with its reason, and must still exist on its own side, so a
// stale entry fails too.
func TestQuickStartExampleMatchesReadme(t *testing.T) {
	t.Parallel()

	readmeCmds, readmeOut := quickStartBlocks(t, readFile(t, "../../README.md"))
	exampleCmds := exampleCommands(t, readFile(t, "../../examples/embed-one-screenshot/run.sh"))
	exampleOut := strings.Split(strings.TrimRight(lastFencedBlock(t, readFile(t, "../../examples/embed-one-screenshot/README.md")), "\n"), "\n")

	tests := []struct {
		name     string
		readme   []string
		example  []string
		onlyInRM map[string]string
		onlyInEx map[string]string
	}{
		{
			name:    "commands",
			readme:  readmeCmds,
			example: exampleCmds,
			onlyInRM: map[string]string{
				"go install github.com/tetriz-ai/ghattach/cmd/ghattach@latest": "the example builds the clone against the stub instead",
				"ghattach login --from-chrome":                                 "it reads the browser's cookie store, which no example does",
			},
			onlyInEx: map[string]string{
				`cp "$repo/testdata/pixel.png" shot.png`: "the Quick start uploads an image of your own",
				"ghattach login --cookie -":              "the example pipes the stub's made-up session instead of reading Chrome",
			},
		},
		{
			name:    "output",
			readme:  readmeOut,
			example: exampleOut,
			onlyInRM: map[string]string{
				"Session stored at /Users/you/.config/ghattach/cookies.json": "--from-chrome runs on macOS, where the home is /Users/you",
			},
			onlyInEx: map[string]string{
				"Session stored at /home/you/.config/ghattach/cookies.json": "the example runner writes its temporary home as /home/you",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, drop(t, tt.readme, tt.onlyInRM, "README Quick start"), drop(t, tt.example, tt.onlyInEx, "examples/embed-one-screenshot"))
		})
	}
}

// examplePipe is how run.sh feeds the stub's made-up session to a command
// that the Quick start runs at a terminal prompt.
const examplePipe = `printf 'fake\n' | `

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // fixed paths inside the repository
	require.NoError(t, err)
	return string(b)
}

type fencedBlock struct {
	lang, body string
}

// fencedBlocks also reads a fence indented under a list item, and strips the
// fence's own indent from each line of its body.
func fencedBlocks(text string) []fencedBlock {
	var blocks []fencedBlock
	var cur *fencedBlock
	var indent string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case strings.HasPrefix(trimmed, "```") && cur == nil:
			indent = line[:len(line)-len(trimmed)]
			cur = &fencedBlock{lang: strings.TrimPrefix(trimmed, "```")}
		case strings.HasPrefix(trimmed, "```"):
			blocks = append(blocks, *cur)
			cur = nil
		case cur != nil:
			cur.body += strings.TrimPrefix(line, indent) + "\n"
		}
	}
	return blocks
}

func lastFencedBlock(t *testing.T, text string) string {
	t.Helper()
	blocks := fencedBlocks(text)
	require.NotEmpty(t, blocks)
	return blocks[len(blocks)-1].body
}

// quickStartBlocks returns the commands and the output lines of the README's
// Quick start section: each line of an sh block is a command, and in a console
// block a "$ " line is a command and any other line is output.
func quickStartBlocks(t *testing.T, readme string) (cmds, out []string) {
	t.Helper()
	_, section, ok := strings.Cut(readme, "\n## Quick start\n")
	require.True(t, ok, "README has a Quick start section")
	section, _, _ = strings.Cut(section, "\n## ")
	for _, b := range fencedBlocks(section) {
		for _, line := range strings.Split(strings.TrimRight(b.body, "\n"), "\n") {
			switch {
			case b.lang == "sh":
				cmds = append(cmds, line)
			case b.lang == "console" && strings.HasPrefix(line, "$ "):
				cmds = append(cmds, strings.TrimPrefix(line, "$ "))
			case b.lang == "console":
				out = append(out, line)
			default:
				t.Fatalf("Quick start has a %q block, which this test does not read", b.lang)
			}
		}
	}
	return cmds, out
}

// exampleCommands returns the lines of run.sh after it sources the setup
// script, without comments, blank lines or the session pipe.
func exampleCommands(t *testing.T, script string) []string {
	t.Helper()
	_, body, ok := strings.Cut(script, "scripts/example-setup.sh\"\n")
	require.True(t, ok, "run.sh sources scripts/example-setup.sh")
	var cmds []string
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmds = append(cmds, strings.TrimPrefix(line, examplePipe))
	}
	return cmds
}

// drop removes the listed lines from lines, failing when a listed line is not
// there.
func drop(t *testing.T, lines []string, listed map[string]string, where string) []string {
	t.Helper()
	for l := range listed {
		assert.Contains(t, lines, l, "%s no longer has a line this test skips", where)
	}
	var kept []string
	for _, l := range lines {
		if _, skip := listed[l]; !skip {
			kept = append(kept, l)
		}
	}
	return kept
}
