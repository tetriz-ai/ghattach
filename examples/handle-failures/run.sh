#!/bin/sh
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# Uploads two images where GitHub refuses the second, an 11 MiB image, since
# its limit for images is lower than for video. Every file is checked locally
# before the first upload, but a refusal only the server can make stops the
# run after the first upload, and that embed is a real upload, so the script
# keeps it.
set -eu
# shellcheck source=scripts/example-setup.sh
. "$(dirname "$0")/../../scripts/example-setup.sh"
cp "$repo/testdata/pixel.png" before.png
dd if=/dev/zero of=after.png bs=1048576 count=11 2> /dev/null

printf 'fake\n' | ghattach login --cookie - > /dev/null
if ghattach upload before.png after.png --repo OWNER/REPO --markdown > embeds.md 2> error.txt; then
	echo "Every file uploaded."
else
	echo "ghattach exited with status $?."
	cat error.txt
	echo "Uploaded before the failure:"
	cat embeds.md
fi
