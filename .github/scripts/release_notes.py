#!/usr/bin/env python3
"""Print the CHANGELOG section for one version, for use as GitHub release notes.

    python3 .github/scripts/release_notes.py v0.1.0

The changelog is the release notes. Writing them twice means writing them
differently, and the second copy is the one nobody updates.

Exits non-zero when the version has no section. That is deliberate: the release
workflow runs this before it publishes anything, so tagging a version you forgot to
write down fails loudly instead of shipping an empty release page.

Standard library only, like everything else here.
"""

import re
import sys
from pathlib import Path

CHANGELOG = Path(__file__).resolve().parents[2] / "CHANGELOG.md"


def section(text: str, version: str) -> str | None:
    """Return the body under '## [<version>]', or None if there is no such heading."""
    lines = text.splitlines()
    start = None
    for i, line in enumerate(lines):
        if not line.startswith("## "):
            continue
        if start is not None:
            # The next H2 ends the section we are collecting.
            return "\n".join(lines[start:i]).strip()
        # "## [0.1.0] — 2026-07-27" and "## [0.1.0]" both match version 0.1.0.
        m = re.match(r"##\s+\[([^\]]+)\]", line)
        if m and m.group(1) == version:
            start = i + 1
    if start is None:
        return None
    return "\n".join(lines[start:]).strip()


def main() -> int:
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} <tag>", file=sys.stderr)
        return 2
    tag = sys.argv[1]
    version = tag[1:] if tag.startswith("v") else tag

    text = CHANGELOG.read_text(encoding="utf-8")
    body = section(text, version)
    if body is None:
        print(
            f"CHANGELOG.md has no '## [{version}]' section — add one before tagging {tag}.",
            file=sys.stderr,
        )
        return 1

    # Link definitions at the foot of the file belong to the changelog, not to a
    # release page where they would render as nothing at all.
    body = "\n".join(
        l for l in body.splitlines() if not re.match(r"^\[[^\]]+\]:\s*http", l)
    ).strip()

    if not body:
        print(f"the '## [{version}]' section is empty.", file=sys.stderr)
        return 1

    print(body)
    return 0


if __name__ == "__main__":
    sys.exit(main())
