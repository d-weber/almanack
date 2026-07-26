#!/usr/bin/env python3
"""Static checks for the browser code, which has no build step to catch these.

Run locally with:  python3 .github/scripts/check_frontend.py

Three things a bundler would normally notice, and nothing else does:

  1. an `import` that does not resolve to a file — a blank screen at runtime;
  2. a JavaScript module missing from the service worker's precache list — which
     breaks offline use quietly, long after the change that caused it;
  3. a translation key used in the UI but absent from a catalog, which renders the
     key itself to the user.
"""

import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[2]
WEB = ROOT / "web"
LOCALES = ROOT / "internal" / "i18n" / "locales"

problems: list[str] = []

modules = sorted(WEB.rglob("*.js"))
if not modules:
    sys.exit("no JavaScript found under web/ — has the layout changed?")

# 1. every import resolves
for path in modules:
    source = path.read_text(encoding="utf-8")
    for match in re.finditer(r"""^\s*(?:import|export)[^'"]*from\s*['"]([^'"]+)['"]""", source, re.M):
        spec = match.group(1)
        if not spec.startswith((".", "/")):
            problems.append(f"{path.relative_to(ROOT)}: bare import {spec!r} — there is no bundler to resolve it")
            continue
        target = (WEB / spec.lstrip("/")) if spec.startswith("/") else (path.parent / spec)
        if not target.resolve().exists():
            problems.append(f"{path.relative_to(ROOT)}: import {spec!r} does not resolve")

# 2. the service worker precaches every module
sw = (WEB / "sw.js").read_text(encoding="utf-8")
shell = re.search(r"const SHELL = \[(.*?)\];", sw, re.S)
if not shell:
    problems.append("web/sw.js: could not find the SHELL precache list")
else:
    listed = {u for u in re.findall(r"'([^']+)'", shell.group(1)) if u.startswith("/js/")}
    actual = {"/js/" + str(p.relative_to(WEB / "js")) for p in (WEB / "js").rglob("*.js")}
    for missing in sorted(actual - listed):
        problems.append(f"web/sw.js: {missing} is not in the precache list (offline use would break)")
    for stale in sorted(listed - actual):
        problems.append(f"web/sw.js: precaches {stale}, which no longer exists")

# 3. every translation key used by the UI exists, and the catalogs agree
catalogs = {p.stem: json.loads(p.read_text(encoding="utf-8")) for p in LOCALES.glob("*.json")}
if "fr" not in catalogs:
    problems.append("internal/i18n/locales: no fr.json")
else:
    used: set[str] = set()
    for path in modules:
        used |= set(re.findall(r"""\bt\(\s*['"]([a-zA-Z][\w.\-]*)['"]""", path.read_text(encoding="utf-8")))
    for key in sorted(used - set(catalogs["fr"])):
        problems.append(f"translation key {key!r} is used by the UI but missing from fr.json")

    reference = set(catalogs["fr"])
    for lang, catalog in catalogs.items():
        for key in sorted(reference - set(catalog)):
            problems.append(f"{lang}.json is missing key {key!r}")
        for key in sorted(set(catalog) - reference):
            problems.append(f"{lang}.json has key {key!r}, which fr.json does not")

print(f"checked {len(modules)} modules, {len(catalogs)} catalogs")
if problems:
    print(f"\n{len(problems)} problem(s):")
    for problem in problems:
        print(f"  - {problem}")
    sys.exit(1)
print("no problems found")
