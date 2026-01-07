# Docker suite for development

**NOTE**: This exists only for local development. If you're interested in using
Docker for a production setup, visit the
[docs](https://listmonk.app/docs/installation/#docker) instead.

## Quick Start

```bash
cd dev
./setup.sh
```

That's it! The script handles everything automatically.

## Access Points

| Service      | URL                     |
|--------------|-------------------------|
| Frontend     | http://localhost:8080   |
| Backend API  | http://localhost:9000   |
| MailHog      | http://localhost:8025   |
| Adminer (DB) | http://localhost:8070   |

## Common Commands

```bash
# View logs
docker compose logs -f

# Stop services
docker compose down

# Complete reset (removes all data)
docker compose down -v && ./setup.sh
```

## Development

- **Backend changes**: Restart the backend with `docker compose restart backend`
- **Frontend changes**: Auto-reloads (hot reload enabled)
