# Contributing

Thanks for helping! The most common contribution is adding an app.

## Adding an app ([catalog/catalog.json](catalog/catalog.json))
```json
{"key":"vivaldi","name":"Vivaldi","category":"Browsers","winget":"Vivaldi.Vivaldi","brew":"vivaldi"}
```
Rules (enforced by `go test ./catalog` and reviewers):
- Package ids come from the **official** winget / Homebrew / Debian-Ubuntu repositories only. No URLs, no third-party sources, no installer downloads.
- Verify the id yourself: `winget show --id <id> -e`, `brew info <name>`, `apt show <name>`.
- Key: lowercase, unique. Name: the vendor's product name.
- Detection: the app is matched by package id **and** by the display name Windows lists it under. If the name differs from `name` (e.g. "Mozilla Firefox"), add `"match":["mozilla firefox"]`. Check with `winget list` on a machine where it's installed.
- Icons: add the app's Simple Icons slug to `tools/fetch_icons.py` and run it; apps without a brand icon get a letter tile automatically.
- Starter packs live in `catalog/packs.json`; keep them small and unsurprising.

## Code
- `go vet ./... && go test ./... && python tests/test_e2e.py && python tests/test_go_agent.py` must pass.
- Matching rules exist twice (Go: `catalog/catalog.go`, Python: `server/server.py`); change both and their tests.
- Keep dependencies at zero (Go standard library, Python standard library). Fewer moving parts is a feature.
- Anything that executes commands must go through `run()`/backends with argv lists, never a shell string.
