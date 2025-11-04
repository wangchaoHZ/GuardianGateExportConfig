## Build Instructions

This project is written in **Go**, and supports cross-compilation for different platforms.

### Compile for Linux (ARM64)

Windows PowerShell generate a binary for **Linux ARM64** (e.g., for a Raspberry Pi or embedded device):

```powershell
$env:GOOS="linux"
$env:GOARCH="arm64"
go build -o fix_yaml
