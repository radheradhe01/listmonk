# Windows PowerShell setup script for listmonk dev environment
$ErrorActionPreference = "Stop"

# Colors for output
function Write-ColorOutput {
    param(
        [string]$Message,
        [string]$Color = "White"
    )
    Write-Host $Message -ForegroundColor $Color
}

Write-ColorOutput "╔════════════════════════════════════════════════════════════╗" "Cyan"
Write-ColorOutput "║           LISTMONK DEV ENVIRONMENT SETUP                   ║" "Cyan"
Write-ColorOutput "╚════════════════════════════════════════════════════════════╝" "Cyan"
Write-Host ""

# Check if Docker is installed
Write-ColorOutput "Checking prerequisites..." "Yellow"
$dockerVersion = docker --version 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-ColorOutput "Error: Docker is not installed. Please install Docker Desktop first." "Red"
    exit 1
}
Write-ColorOutput "  ✓ Docker is installed" "Green"

# Check if Docker Compose is available
$composeVersion = docker compose version 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-ColorOutput "Error: Docker Compose is not available. Please install Docker Compose." "Red"
    exit 1
}
Write-ColorOutput "  ✓ Docker Compose is available" "Green"

# Get script directory
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $ScriptDir

# Build Docker images
Write-Host ""
Write-ColorOutput "[1/5] Building Docker images..." "Yellow"
docker compose build
if ($LASTEXITCODE -ne 0) {
    Write-ColorOutput "Error: Failed to build Docker images" "Red"
    exit 1
}
Write-ColorOutput "  ✓ Docker images built successfully" "Green"

# Start database
Write-Host ""
Write-ColorOutput "[2/5] Starting database..." "Yellow"
docker compose up -d db
if ($LASTEXITCODE -ne 0) {
    Write-ColorOutput "Error: Failed to start database" "Red"
    exit 1
}

# Wait for database to be healthy
Write-ColorOutput "[3/5] Waiting for database to be healthy..." "Yellow"
$Retries = 30
$DatabaseReady = $false

while ($Retries -gt 0) {
    $result = docker compose exec -T db pg_isready -U listmonk-dev -d listmonk-dev 2>&1
    if ($LASTEXITCODE -eq 0) {
        $DatabaseReady = $true
        break
    }
    Write-Host "  Waiting for PostgreSQL... ($Retries attempts left)"
    Start-Sleep -Seconds 2
    $Retries--
}

if (-not $DatabaseReady) {
    Write-ColorOutput "Database failed to start. Check logs with: docker compose logs db" "Red"
    exit 1
}
Write-ColorOutput "  ✓ Database is ready!" "Green"

# Initialize database
Write-Host ""
Write-ColorOutput "[4/5] Initializing database schema..." "Yellow"

# Check if database is already initialized
$TableCheck = docker compose exec -T db psql -U listmonk-dev -d listmonk-dev -tAc "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'settings');" 2>&1
$TableExists = $TableCheck -match 't'

if ($TableExists) {
    Write-ColorOutput "  Database already initialized. Running migrations..." "Green"
    docker compose run --rm backend sh -c 'make dist && ./listmonk --config=dev/config.toml --upgrade --yes' 2>&1 | Out-Null
} else {
    Write-Host "  Running first-time database setup..."
    docker compose run --rm backend sh -c 'make dist && ./listmonk --config=dev/config.toml --install --idempotent --yes' 2>&1 | Out-Null
}

if ($LASTEXITCODE -ne 0) {
    Write-ColorOutput "  Warning: Database initialization had issues (might already be initialized)" "Yellow"
} else {
    Write-ColorOutput "  ✓ Database initialization complete!" "Green"
}

# Start all services
Write-Host ""
Write-ColorOutput "[5/5] Starting all services..." "Yellow"
docker compose up -d
if ($LASTEXITCODE -ne 0) {
    Write-ColorOutput "Error: Failed to start services" "Red"
    exit 1
}

# Display completion message
Write-Host ""
Write-ColorOutput "╔════════════════════════════════════════════════════════════╗" "Green"
Write-ColorOutput "║                    SETUP COMPLETE!                          ║" "Green"
Write-ColorOutput "╚════════════════════════════════════════════════════════════╝" "Green"
Write-Host ""
Write-ColorOutput "  Frontend:    " -NoNewline; Write-ColorOutput "http://localhost:8080" "Cyan"
Write-ColorOutput "  Backend API: " -NoNewline; Write-ColorOutput "http://localhost:9000" "Cyan"
Write-ColorOutput "  MailHog:     " -NoNewline; Write-ColorOutput "http://localhost:8025" "Cyan"
Write-ColorOutput "  Adminer:     " -NoNewline; Write-ColorOutput "http://localhost:8070" "Cyan"
Write-Host ""
Write-ColorOutput "Note: Frontend may take 1-2 minutes to compile on first run." "Yellow"
Write-ColorOutput "View logs: " -NoNewline; Write-ColorOutput "docker compose logs -f" "Green"
Write-ColorOutput "Stop all:  " -NoNewline; Write-ColorOutput "docker compose down" "Green"
Write-ColorOutput "Reset all: " -NoNewline; Write-ColorOutput "docker compose down -v; .\setup.ps1" "Green"
Write-Host ""
