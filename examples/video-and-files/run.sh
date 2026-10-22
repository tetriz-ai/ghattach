#!/bin/sh
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# Uploads an image, a screen recording and a log file in one run. --markdown
# prints a different embed for each media type, one line per file, in order.
set -eu
# shellcheck source=scripts/example-setup.sh
. "$(dirname "$0")/../../scripts/example-setup.sh"
cp "$repo/testdata/pixel.png" shot.png
printf 'a stand-in for a screen recording\n' > demo.mp4
printf 'ok 1 - build\nok 2 - test\n' > build.log

printf 'fake\n' | ghattach login --cookie - > /dev/null
ghattach upload shot.png demo.mp4 build.log --repo OWNER/REPO --markdown
