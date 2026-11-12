# CI/CD Change-Safety Gates

RepoCortex enforces architectural boundaries, blindspot detection, and refactoring safety directly in GitHub Actions and GitLab CI.

---

## 1. GitHub Actions Workflow (`repocortex-gate.yml`)

Add `.github/workflows/repocortex-gate.yml` to your repository:

```yaml
name: RepoCortex Change-Safety Gate

on:
  pull_request:
    branches: [main]
  push:
    branches: [main]

permissions:
  contents: read

env:
  REPOCORTEX_FAIL_ON: high
  REPOCORTEX_ACCURACY: auto

jobs:
  gate:
    name: Change-Safety Verification
    runs-on: ubuntu-latest
    permissions:
      contents: read
      pull-requests: write   # Posts sticky report comment
      security-events: write # Uploads SARIF to GitHub Code Scanning
    steps:
      - name: Checkout Repository
        uses: actions/checkout@v5
        with:
          fetch-depth: 0

      - name: Setup Go Environment
        uses: actions/setup-go@v6
        with:
          go-version: '1.26.x'
          cache: true

      - name: Build RepoCortex
        run: go build -trimpath -ldflags="-s -w" -o ./bin/repocortex ./cmd/repocortex

      - name: Index and Resolve
        run: |
          ./bin/repocortex init
          ./bin/repocortex index
          ./bin/repocortex resolve

      - name: Evaluate Blast Radius & Risk
        id: impact
        run: |
          mkdir -p repocortex-out
          BASE_REF="${{ github.base_ref }}"
          if [ -n "$BASE_REF" ]; then
            BASE="origin/$BASE_REF"
          else
            BASE="HEAD~1"
          fi
          ./bin/repocortex impact --base "$BASE" --head HEAD --json > repocortex-out/impact.json || true
          ./bin/repocortex blindspots --fail-on "$REPOCORTEX_FAIL_ON" --json > repocortex-out/missed.json || true

      - name: Generate PR Summary Report
        if: always()
        run: |
          if [ -f repocortex-out/impact.json ]; then
            ./bin/repocortex report pr --impact repocortex-out/impact.json \
              --blindspots repocortex-out/missed.json --out repocortex-out/pr-report.md
            cat repocortex-out/pr-report.md >> "$GITHUB_STEP_SUMMARY"
          fi

      - name: Upload SARIF to GitHub Code Scanning
        if: github.event_name == 'push' || github.event.pull_request.head.repo.full_name == github.repository
        uses: github/codeql-action/upload-sarif@v4
        with:
          sarif_file: repocortex-out/repocortex.sarif
          category: repocortex

      - name: Enforce Architecture Invariants
        run: ./bin/repocortex arch --fail-on "$REPOCORTEX_FAIL_ON"
```

---

## 2. SARIF 2.1.0 Integration

RepoCortex outputs standard SARIF 2.1.0 via:
```bash
repocortex report sarif --out repocortex.sarif
```
This renders line-level annotations in GitHub Pull Request "Files Changed" tabs for:
- Dead code / uncalled public symbols
- Architectural layering violations (e.g. `domain` importing `infrastructure`)
- Database access from non-repository packages
- Unmapped HTTP endpoints
