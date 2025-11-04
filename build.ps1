param(
    [string]$ArgsOutputName
)

# 出错时立即停止
$ErrorActionPreference = "Stop"

# 确定输出文件名（如果传入参数则使用之，否则使用默认）
if ($ArgsOutputName) {
    $OutputName = $ArgsOutputName
} else {
    $OutputName = "DefaultAppName"
}

# 打印开始信息
Write-Host ""
Write-Host "Start Building Go Project" -ForegroundColor Cyan

# 设置目标平台（这里固定为 linux/arm64）
$env:GOOS = "linux"
$env:GOARCH = "arm64"

# 显示目标信息（深蓝色）
Write-Host "Target OS: $env:GOOS" -ForegroundColor DarkBlue
Write-Host "Target Arch: $env:GOARCH" -ForegroundColor DarkBlue
Write-Host "Output File: $OutputName" -ForegroundColor DarkBlue
Write-Host ""

# 如果已存在同名二进制，则删除它
if (Test-Path $OutputName) {
    Remove-Item $OutputName -Force
    Write-Host "Removed Old Binary: $OutputName" -ForegroundColor DarkGray
}

# 执行构建命令
Write-Host "Building Project..." -ForegroundColor Cyan
go build -o $OutputName

# 检查构建结果并输出对应信息
if ($LASTEXITCODE -eq 0) {
    Write-Host ""
    Write-Host "Build Succeeded" -ForegroundColor Green
    Write-Host "Output File: $OutputName" -ForegroundColor Green
} else {
    Write-Host ""
    Write-Host "Build Failed. Please Check The Error Logs Above." -ForegroundColor Red
    exit 1
}

Write-Host ""
Write-Host "Build Completed" -ForegroundColor Cyan
