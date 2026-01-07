#!/bin/bash
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo -e "${BLUE}"
echo "╔════════════════════════════════════════════════════════════╗"
echo "║           LISTMONK DEV ENVIRONMENT SETUP                   ║"
echo "╚════════════════════════════════════════════════════════════╝"
echo -e "${NC}"

# Check if Docker is installed
if ! command -v docker &> /dev/null; then
    echo -e "${RED}Error: Docker is not installed. Please install Docker first.${NC}"
    exit 1
fi

# Check if Docker Compose is available
if ! docker compose version &> /dev/null; then
    echo -e "${RED}Error: Docker Compose is not available. Please install Docker Compose.${NC}"
    exit 1
fi

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo -e "${YELLOW}[1/5] Building Docker images...${NC}"
docker compose build

echo -e "${YELLOW}[2/5] Starting database...${NC}"
docker compose up -d db

echo -e "${YELLOW}[3/5] Waiting for database to be healthy...${NC}"
RETRIES=30
until docker compose exec -T db pg_isready -U listmonk-dev -d listmonk-dev > /dev/null 2>&1; do
    RETRIES=$((RETRIES-1))
    if [ $RETRIES -le 0 ]; then
        echo -e "${RED}Database failed to start. Check logs with: docker compose logs db${NC}"
        exit 1
    fi
    echo "  Waiting for PostgreSQL... ($RETRIES attempts left)"
    sleep 2
done
echo -e "${GREEN}  Database is ready!${NC}"

echo -e "${YELLOW}[4/5] Initializing database schema...${NC}"
# Check if database is already initialized
TABLE_EXISTS=$(docker compose exec -T db psql -U listmonk-dev -d listmonk-dev -tAc "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'settings');" 2>/dev/null || echo "f")

if [ "$TABLE_EXISTS" = "t" ]; then
    echo -e "${GREEN}  Database already initialized. Running migrations...${NC}"
    docker compose run --rm backend sh -c "make dist && ./listmonk --config=dev/config.toml --upgrade --yes" || true
else
    echo "  Running first-time database setup..."
    docker compose run --rm backend sh -c "make dist && ./listmonk --config=dev/config.toml --install --idempotent --yes"
fi
echo -e "${GREEN}  Database initialization complete!${NC}"

echo -e "${YELLOW}[5/5] Starting all services...${NC}"
docker compose up -d

echo ""
echo -e "${GREEN}╔════════════════════════════════════════════════════════════╗"
echo "║                    SETUP COMPLETE!                          ║"
echo "╚════════════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "  ${BLUE}Frontend:${NC}    http://localhost:8080"
echo -e "  ${BLUE}Backend API:${NC} http://localhost:9000"
echo -e "  ${BLUE}MailHog:${NC}     http://localhost:8025"
echo -e "  ${BLUE}Adminer:${NC}     http://localhost:8070"
echo ""
echo -e "${YELLOW}Note: Frontend may take 1-2 minutes to compile on first run.${NC}"
echo -e "View logs: ${GREEN}docker compose logs -f${NC}"
echo -e "Stop all:  ${GREEN}docker compose down${NC}"
echo -e "Reset all: ${GREEN}docker compose down -v && ./setup.sh${NC}"