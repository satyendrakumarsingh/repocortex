# Installation & Setup

RepoCortex ships as a single, self-contained binary with **zero external runtime dependencies** and **zero CGO**. It runs locally on macOS, Linux, and Windows.

---

## 1. Automated Installation (Recommended)

### macOS & Linux
```bash
curl -fsSL https://raw.githubusercontent.com/satyendrakumarsingh/repocortex/main/install.sh | sh
```
*Installs the universal binary to `/usr/local/bin/cortex` (or `~/.local/bin/cortex`), removes macOS quarantine flags, creates the `repocortex` alias, and verifies the SHA256 checksum against official release artifacts.*

### Windows (PowerShell)
```powershell
irm https://raw.githubusercontent.com/satyendrakumarsingh/repocortex/main/install.ps1 | iex
```
*Installs `cortex.exe` and `repocortex.exe` to `%LOCALAPPDATA%\Programs\RepoCortex` and adds it to your user `PATH`.*

---

## 2. Install via Go Toolchain

If you have Go installed (Go 1.23+):
```bash
go install github.com/satyendrakumarsingh/repocortex/cmd/cortex@latest
```
*Compiles the pure-Go binary with embedded Tree-sitter parsers and local SQLite.*

---

## 3. Precompiled Release Binaries

Download precompiled, checksummed binaries from the [GitHub Releases page](https://github.com/satyendrakumarsingh/repocortex/releases):

| Target | Architecture | Archive |
|---|---|---|
| **macOS Universal** | Apple Silicon (M1-M4) & Intel x86_64 | `cortex-mac.zip` |
| **Linux AMD64** | x86_64 servers & containers | `cortex_v1.0.0_linux_amd64.tar.gz` |
| **Linux ARM64** | AWS Graviton, Raspberry Pi, ARM servers | `cortex_v1.0.0_linux_arm64.tar.gz` |
| **Windows AMD64** | Windows 10 / 11 x86_64 | `cortex_v1.0.0_windows_amd64.zip` / `cortex.exe` |

Verify integrity using `SHA256SUMS`:
```bash
shasum -a 256 -c SHA256SUMS
```

---

## 4. Verification

Confirm installation:
```bash
cortex version
```
Expected output:
```text
cortex v1.0.0 (darwin/arm64)
```

---

## Next Steps
- Follow the [60-Second Quickstart](quickstart.md) to index your first repository.
- Wire your coding environment with the [Agent Integration Guide](agent-integration.md).
