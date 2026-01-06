#!/bin/sh
set -e

# Install frontend dependencies if node_modules doesn't exist or is empty
if [ ! -d "/app/frontend/node_modules" ] || [ -z "$(ls -A /app/frontend/node_modules 2>/dev/null)" ]; then
  echo "Installing frontend dependencies..."
  cd /app/frontend && yarn install
fi

# Install email-builder dependencies if node_modules doesn't exist or is empty
if [ ! -d "/app/frontend/email-builder/node_modules" ] || [ -z "$(ls -A /app/frontend/email-builder/node_modules 2>/dev/null)" ]; then
  echo "Installing email-builder dependencies..."
  cd /app/frontend/email-builder && yarn install
fi

# Build email-builder if dist doesn't exist
if [ ! -d "/app/frontend/public/static/email-builder" ] || [ -z "$(ls -A /app/frontend/public/static/email-builder 2>/dev/null)" ]; then
  echo "Building email-builder..."
  export VUE_APP_VERSION="v5.1.0" && cd /app/frontend/email-builder && yarn build
  mkdir -p /app/frontend/public/static/email-builder
  cp -r /app/frontend/email-builder/dist/* /app/frontend/public/static/email-builder
fi

# Start the dev server
echo "Starting frontend dev server..."
export VUE_APP_VERSION="v5.1.0" && cd /app/frontend && yarn dev
