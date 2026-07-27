#!/usr/bin/env python3
"""Per-package test coverage floors, enforced.

Run locally with:  make cover-check

Coverage that is only ever *reported* is coverage that rots. A number printed at the
end of a CI log is read by nobody, and the packages that quietly slid from 80% to 40%
are exactly the ones that were being changed most. So the floors live here, one per
package, and dropping below one fails the build.

Two rules, and the second matters more than the first:

  1. A package may not fall below its floor. If a change adds code without tests, the
     floor catches it. Fixing that means writing the test, or lowering the floor in the
     same commit — which is a visible decision in a diff rather than a silent drift.

  2. A package must be *in this file*. A new package with no entry fails, even if it is
     thoroughly tested, because the alternative is that a package can be added with no
     tests and nothing notices. Adding an entry is one line and takes a view on how well
     the thing needs to be tested.

The floors are not aspirations. They are set at, or just under, what the suite actually
achieves today, so this file answers "has it got worse", not "is it good enough". Raise
them when you raise the coverage; that is how the ratchet tightens.
"""

import re
import subprocess
import sys

# Package -> minimum percentage of statements covered.
#
# Where a floor is well below 100 the reason is written down, because a low number with
# no explanation reads as neglect and gets "fixed" by someone writing shallow tests.
FLOORS: dict[str, float] = {
    # The subcommands. backup and bootstrap are the data-safety paths and are well
    # covered; serve.go and seed.go are mostly wiring that the e2e tests exercise for
    # real, which is why the package total is lower than its important halves.
    "almanack/cmd/almanack": 37.0,
    # Passwords and token minting. The uncovered remainder is the crypto/rand failure
    # branches, which need a seam that does not exist.
    "almanack/internal/auth": 93.0,
    "almanack/internal/clock": 100.0,
    # The strict-configuration promise from 0.2.0. The gap is Load's fallback to
    # /etc/almanack/almanack.conf, which cannot be exercised without a system file.
    "almanack/internal/config": 99.0,
    "almanack/internal/domain": 66.0,
    # Scoped edits and series splitting — the hardest logic in the app, and the subject
    # of several open issues. This floor should rise as those land.
    "almanack/internal/events": 69.0,
    "almanack/internal/holidays": 94.0,
    # Handlers. The remainder is largely error plumbing on paths the store tests cover
    # from the other side.
    "almanack/internal/httpapi": 66.0,
    "almanack/internal/i18n": 88.0,
    "almanack/internal/imgproc": 93.0,
    "almanack/internal/mailer": 100.0,
    # The planner, the outbox and boot catch-up.
    "almanack/internal/notify": 74.0,
    # Recurrence is pure functions over dates, so it can be tested exhaustively, and is.
    "almanack/internal/recur": 98.0,
    "almanack/internal/store": 80.0,
    # Web Push, verified against the RFC 8291 test vectors.
    "almanack/internal/webpush": 85.0,
}

# Packages that legitimately have no statements to cover. Each needs a reason: this is
# the escape hatch, and an escape hatch without reasons becomes where everything goes.
NO_STATEMENTS: dict[str, str] = {
    "almanack/internal/deps": "holds no code — it is the dependency policy expressed as a test",
    "almanack/web": "the go:embed directive and nothing else",
}

LINE = re.compile(
    r"^(?:ok|FAIL|\?)\s+(\S+)\s+(?:.*?coverage:\s+(?:(\d+\.\d+)% of statements|\[no statements\])|\[no test files\])"
)


def main() -> int:
    proc = subprocess.run(
        ["go", "test", "-cover", "./..."],
        capture_output=True,
        text=True,
        cwd=None,
    )
    if proc.returncode != 0:
        sys.stdout.write(proc.stdout)
        sys.stderr.write(proc.stderr)
        print("\ntests failed — coverage floors not checked", file=sys.stderr)
        return proc.returncode

    problems: list[str] = []
    seen: set[str] = set()

    for line in proc.stdout.splitlines():
        match = LINE.match(line.strip())
        if not match:
            continue
        pkg, pct = match.group(1), match.group(2)
        seen.add(pkg)

        if pct is None:
            # No test files, or nothing to cover.
            if pkg in NO_STATEMENTS:
                continue
            if pkg in FLOORS:
                problems.append(
                    f"{pkg} has a floor of {FLOORS[pkg]:.0f}% but no tests ran.\n"
                    f"    Either the test files were deleted, or a build tag is hiding them."
                )
            else:
                problems.append(
                    f"{pkg} has no tests and no entry in check_coverage.py.\n"
                    f"    Add a floor for it, or add it to NO_STATEMENTS with a reason."
                )
            continue

        covered = float(pct)
        if pkg not in FLOORS:
            problems.append(
                f"{pkg} is not listed in check_coverage.py (it covers {covered:.1f}%).\n"
                f"    Add a floor so that a later drop is caught. Start at {int(covered)}."
            )
        elif covered < FLOORS[pkg]:
            problems.append(
                f"{pkg} covers {covered:.1f}%, below its floor of {FLOORS[pkg]:.0f}%.\n"
                f"    Write the missing test, or lower the floor in this commit and say why."
            )

    for pkg in FLOORS:
        if pkg not in seen:
            problems.append(
                f"{pkg} has a floor but did not appear in the test output.\n"
                f"    If the package was removed or renamed, remove its floor too."
            )

    if problems:
        print("Coverage floors not met:\n", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
        print(
            f"\n{len(problems)} problem(s). Floors are in .github/scripts/check_coverage.py.",
            file=sys.stderr,
        )
        return 1

    print(f"Coverage floors met for {len(FLOORS)} packages.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
