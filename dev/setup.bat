@echo off
setlocal enabledelayedexpansion

echo.
echo ╔════════════════════════════════════════════════════════════╗
echo ║           LISTMONK DEV ENVIRONMENT SETUP                   ║
echo ╚════════════════════════════════════════════════════════════╝
echo.

REM Check if Docker is installed
echo Checking prerequisites...
docker --version >nul 2>&1
if errorlevel 1 (
    echo Error: Docker is not installed. Please install Docker Desktop first.
    exit /b 1
)
echo   ✓ Docker is installed

REM Check if Docker Compose is available
docker compose version >nul 2>&1
if errorlevel 1 (
    echo Error: Docker Compose is not available. Please install Docker Compose.
    exit /b 1
)
echo   ✓ Docker Compose is available

REM Get script directory
cd /d "%~dp0"

REM Build Docker images
echo.
echo [1/5] Building Docker images...
docker compose build
if errorlevel 1 (
    echo Error: Failed to build Docker images
    exit /b 1
)
echo   ✓ Docker images built successfully

REM Start database
echo.
echo [2/5] Starting database...
docker compose up -d db
if errorlevel 1 (
    echo Error: Failed to start database
    exit /b 1
)

REM Wait for database to be healthy
echo [3/5] Waiting for database to be healthy...
set RETRIES=30
set DATABASE_READY=0

:wait_db
docker compose exec -T db pg_isready -U listmonk-dev -d listmonk-dev >nul 2>&1
if not errorlevel 1 (
    set DATABASE_READY=1
    goto db_ready
)
echo   Waiting for PostgreSQL... (!RETRIES! attempts left)
set /a RETRIES-=1
if !RETRIES! leq 0 (
    echo Database failed to start. Check logs with: docker compose logs db
    exit /b 1
)
timeout /t 2 /nobreak >nul
goto wait_db

:db_ready
echo   ✓ Database is ready!

REM Initialize database
echo.
echo [4/5] Initializing database schema...

REM Check if database is already initialized
for /f "tokens=*" %%i in ('docker compose exec -T db psql -U listmonk-dev -d listmonk-dev -tAc "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'settings');" 2^>nul') do set TABLE_CHECK=%%i

if "!TABLE_CHECK!"=="t" (
    echo   Database already initialized. Running migrations...
    docker compose run --rm backend sh -c "make dist && ./listmonk --config=dev/config.toml --upgrade --yes" >nul 2>&1
) else (
    echo   Running first-time database setup...
    docker compose run --rm backend sh -c "make dist && ./listmonk --config=dev/config.toml --install --idempotent --yes" >nul 2>&1
)

if errorlevel 1 (
    echo   Warning: Database initialization had issues (might already be initialized)
) else (
    echo   ✓ Database initialization complete!
)

REM Start all services
echo.
echo [5/5] Starting all services...
docker compose up -d
if errorlevel 1 (
    echo Error: Failed to start services
    exit /b 1
)

REM Display completion message
echo.
echo ╔════════════════════════════════════════════════════════════╗
echo ║                    SETUP COMPLETE!                          ║
echo ╚════════════════════════════════════════════════════════════╝
echo.
echo   Frontend:    http://localhost:8080
echo   Backend API: http://localhost:9000
echo   MailHog:     http://localhost:8025
echo   Adminer:     http://localhost:8070
echo.
echo Note: Frontend may take 1-2 minutes to compile on first run.
echo View logs: docker compose logs -f
echo Stop all:  docker compose down
echo Reset all: docker compose down -v ^&^& setup.bat
echo.

endlocal
