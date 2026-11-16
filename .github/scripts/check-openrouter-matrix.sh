#!/usr/bin/env bash
set -euo pipefail

# Verify that every model id referenced by the OpenRouter routing matrix
# still exists in the live OpenRouter catalog. The matrix ships with the
# docs (docs/openrouter-routing.md) and the CI script; keeping both in sync
# is what makes the "delisted model" acceptance criterion hold: a model that
# OpenRouter has removed will make this check fail and block the release.
#
# Failure model:
#   - Network / fetch failure  -> warn and skip (do not block the release on
#     an upstream outage; the docs carry a data_as_of date so readers know
#     how fresh the snapshot is).
#   - Catalog fetched, but a matrix model id is absent -> hard fail.

matrix_file="test/openrouter_matrix.json"
catalog_file="$(mktemp)"
trap 'rm -f "$catalog_file"' EXIT

python3 - "$matrix_file" "$catalog_file" <<'PY'
import json, sys, urllib.request, urllib.error

matrix_path, catalog_path = sys.argv[1], sys.argv[2]
catalog_url = "https://openrouter.ai/api/v1/models"

with open(matrix_path) as fh:
    matrix = json.load(fh)

ids = []
for sc in matrix["scenarios"].values():
    for key in ("primary", "backup", "fallback"):
        if sc.get(key):
            ids.append(sc[key])
ids = sorted(set(ids))

try:
    with urllib.request.urlopen(catalog_url, timeout=30) as resp:
        live = json.load(resp)
except Exception as exc:  # network or parse problem: do not block on it
    print(f"::warning::Could not fetch OpenRouter catalog ({exc}); skipping live matrix check.")
    sys.exit(0)

live_ids = {m.get("id") for m in live.get("data", [])}
missing = [i for i in ids if i not in live_ids]

with open(catalog_path, "w") as fh:
    json.dump({"checked_ids": ids, "missing": missing}, fh)

if missing:
    print("OpenRouter matrix references delisted or unknown model ids:")
    for m in missing:
        print(f"  - {m}")
    print("Update docs/openrouter-routing.md and test/openrouter_matrix.json, "
          "or point the scenario at a currently-listed model.")
    sys.exit(1)

print(f"OpenRouter matrix check passed: all {len(ids)} referenced model ids are live "
      f"(catalog has {len(live_ids)} models).")
PY
