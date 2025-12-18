# Docker suite for development

**NOTE**: This exists only for local development. If you're interested in using
Docker for a production setup, visit the
[docs](https://listmonk.app/docs/installation/#docker) instead.

### Objective

The purpose of this Docker suite for local development is to isolate all the dev
dependencies in a Docker environment. The containers have a host volume mounted
inside for the entire app directory. This helps us to not do a full
`docker build` for every single local change, only restarting the Docker
environment is enough.

## Setting up a dev suite

To spin up a local suite of:

- PostgreSQL
- Mailhog
- Node.js frontend app
- Golang backend app

### Verify your config file

The config file provided at `dev/config.toml` will be used when running the
containerized development stack. Make sure the values set within are suitable
for the feature you're trying to develop.

### Setup DB

Running this will build the appropriate images and initialize the database.

```bash
make init-dev-docker
```

### Start frontend and backend apps

Running this start your local development stack.

```bash
make dev-docker
```

Visit the frontend at `http://localhost:8081` and the backend API at `http://localhost:9000` on your browser.

### Upgrade DB

If you've added database migrations or need to apply pending migrations, run:

```bash
make upgrade-dev-docker
```

This runs the pending migrations inside the backend container (equivalent to `./listmonk --upgrade --yes --config dev/config.toml`). After successful upgrade, restart the backend (`make dev-docker` or `docker compose up -d backend`) if necessary.

### Tear down

This will tear down all the data, including DB.

```bash
make rm-dev-docker
```

### See local changes in action

- Backend: Anytime you do a change to the Go app, it needs to be compiled. Just
  run `make dev-docker` again and that should automatically handle it for you.

- Frontend: Anytime you change the frontend code, you don't need to do anything.
  Since `yarn` is watching for changes and the code is mounted inside the docker container, the yarn dev server will automatically restart.

  Frontend troubleshooting (native rollup module errors)
  -----------------------------------------------------
  If the frontend container exits with an error such as
  `Cannot find module '@rollup/rollup-linux-x64-gnu'` (or similar Rollup native module errors),
  that usually means a host `node_modules` (built on macOS/host) is being mounted into the Linux container,
  causing a platform binary mismatch.

  Quick fix:
  ```bash
  # from project root
  rm -rf frontend/node_modules
  cd dev
  docker compose up --build front
  ```

  Alternatively, install from within the container:
  ```bash
  cd dev
  docker compose run --rm front sh -c "cd frontend && yarn install && yarn dev"
  ```

  Note: The dev compose now uses a named Docker volume `front_node_modules` for
  `/app/frontend/node_modules` to avoid host `node_modules` overwriting container-installed modules.
