#!/bin/sh
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# The upload step of a CI job with no signed-in gh: the session comes from a
# stored secret, --repo-id replaces the gh lookup, and the embeds go into a
# comment body for the job's next step to post.
set -eu
# shellcheck source=scripts/example-setup.sh
. "$(dirname "$0")/../../scripts/example-setup.sh"
cp "$repo/testdata/pixel.png" before.png
cp "$repo/testdata/pixel.png" after.png

# In a real job this is a secret, such as ${{ secrets.GHATTACH_SESSION }}. Here
# it is the stub's made-up session.
GITHUB_SESSION=fake

printf '%s\n' "$GITHUB_SESSION" | ghattach login --cookie - > /dev/null
{
	echo "Screenshots from this build:"
	echo
	ghattach upload before.png after.png --repo OWNER/REPO --repo-id 123456789 --markdown
} > comment.md
cat comment.md
