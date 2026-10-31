$ErrorActionPreference = "Stop"

Get-NetTCPConnection -LocalPort 50051, 8443, 8080 -ErrorAction SilentlyContinue | Select-Object LocalPort, OwningProcess | ForEach-Object {
    Write-Host "Stopping process on port $($_.LocalPort) (PID: $($_.OwningProcess))..." -ForegroundColor Yellow
    Stop-Process -Id $_.OwningProcess -Force -ErrorAction SilentlyContinue
}

Write-Host ">>> [1/5] Checking Go Environment..." -ForegroundColor Cyan
if (-not (Get-Command "go" -ErrorAction SilentlyContinue)) {
    Write-Error "Go is not installed or not in PATH."
    exit 1
}

# Ensure go.mod exists
if (-not (Test-Path "go.mod") -or ((Get-Item "go.mod").Length -eq 0)) {
    Write-Host "Initializing go.mod..." -ForegroundColor Yellow
    Remove-Item "go.mod" -Force -ErrorAction SilentlyContinue
    go mod init sis2
}

Write-Host ">>> [2/5] Installing Protobuf Go Plugins..." -ForegroundColor Cyan
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Add Go bin to PATH for current session
$goBin = (go env GOPATH) + "\bin"
if ($env:PATH -notlike "*$goBin*") {
    $env:PATH = "$goBin;$env:PATH"
}

Write-Host ">>> [3/5] Verifying protoc Compiler..." -ForegroundColor Cyan
if (-not (Get-Command "protoc" -ErrorAction SilentlyContinue)) {
    $protocDir = "$PSScriptRoot\.tools\protoc"
    if (-not (Test-Path "$protocDir\bin\protoc.exe")) {
        Write-Host "Downloading protoc for Windows..." -ForegroundColor Yellow
        $protocZip = "$env:TEMP\protoc.zip"
        New-Item -ItemType Directory -Force -Path $protocDir | Out-Null
        Invoke-WebRequest -Uri "https://github.com/protocolbuffers/protobuf/releases/download/v28.3/protoc-28.3-win64.zip" -OutFile $protocZip
        Expand-Archive -Path $protocZip -DestinationPath $protocDir -Force
        Remove-Item $protocZip -Force
    }
    $env:PATH = "$protocDir\bin;$env:PATH"
}

Write-Host ">>> [4/5] Compiling Protocol Buffer Stubs into proto/taskv1..." -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path "proto\taskv1" | Out-Null
if (Test-Path "proto\task.proto") {
    Copy-Item "proto\task.proto" "proto\taskv1\task.proto" -Force
}

# Compile with source_relative pointing to proto/taskv1/task.proto
protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/taskv1/task.proto

go mod tidy

Write-Host ">>> [5/5] Launching Server & Running Benchmark..." -ForegroundColor Cyan
$serverProcess = Start-Process -FilePath "go" -ArgumentList "run", "server/main.go" -PassThru -NoNewWindow
Start-Sleep -Seconds 2

try {
    go run client/main.go
} finally {
    Write-Host "Stopping Server (PID: $($serverProcess.Id))..." -ForegroundColor Yellow
    Stop-Process -Id $serverProcess.Id -Force -ErrorAction SilentlyContinue
}