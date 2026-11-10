# CI/CD Change-Safety Gates

RepoCortex enforces architectural boundaries, blindspot detection, and refactoring safety directly in GitHub Actions and GitLab CI.

---

## 1. GitHub Actions Workflow (`cortex-gate.yml`)

Add `.github/workflows/cortex-gate.yml` to your repository:

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
  CORTEX_FAIL_ON: high
  CORTEX_ACCURACY: auto

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
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Setup Go Environment
        uses: actions/setup-go@v5
        with:
          go-version: '1.25.x'
          cache: true

      - name: Build RepoCortex
        run: go build -trimpath -ldflags="-s -w" -o ./bin/cortex ./cmd/cortex

      - name: Index and Resolve
        run: |
          ./bin/cortex init
          ./bin/cortex index
          ./bin/cortex resolve

      - name: Evaluate Blast Radius & Risk
        id: impact
        run: |
          mkdir -p cortex-out
          BASE_REF="${{ github.base_ref }}"
          if [ -n "$BASE_REF" ]; then
            BASE="origin/$BASE_REF"
          else
            BASE="HEAD~1"
          fi
          ./bin/cortex impact --base "$BASE" --head HEAD --json > cortex-out/impact.json || true
          ./bin/cortex missed --fail-on "$CORTEX_FAIL_ON" --json > cortex-out/missed.json || true

      - name: Generate PR Summary Report
        if: always()
        run: |
          if [ -f cortex-out/impact.json ]; then
            ./bin/cortex report pr --impact cortex-out/impact.json \
              --missed cortex-out/missed.json --out cortex-out/pr-report.md
            cat cortex-out/pr-report.md >> "$GITHUB_STEP_SUMMARY"
          fi

      - name: Upload SARIF to GitHub Code Scanning
        if: github.event_name == 'push' || github.event.pull_request.head.repo.full_name == github.repository
        uses: github/codeql-action/upload-sarif@v3
        with:
          sarif_file: cortex-out/cortex.sarif
          category: cortex

      - name: Enforce Architecture Invariants
        run: ./bin/cortex arch --fail-on "$CORTEX_FAIL_ON"
```

---

## 2. SARIF 2.1.0 Integration

RepoCortex outputs standard SARIF 2.1.0 via:
```bash
cortex report sarif --out cortex.sarif
```
This renders line-level annotations in GitHub Pull Request "Files Changed" tabs for:
- Dead code / uncalled public symbols
- Architectural layering violations (e.g. `domain` importing `infrastructure`)
- Database access from non-repository packages
- Unmapped HTTP endpoints
