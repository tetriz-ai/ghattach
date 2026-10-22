#!/bin/sh
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# The README's quick start: log in with a session value, then upload one
# screenshot and print its markdown embed.
set -eu
# shellcheck source=scripts/example-setup.sh
. "$(dirname "$0")/../../scripts/example-setup.sh"
cp "$repo/testdata/pixel.png" shot.png

printf 'fake\n' | ghattach login --cookie -
ghattach upload shot.png --repo OWNER/REPO --markdown
