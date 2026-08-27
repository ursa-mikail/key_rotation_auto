#!/bin/sh
# Builds and starts the full stack: Postgres, the Go rotation backend,
# the TypeScript frontend, and the Terraform runner sidecar.
set -eu
cd "$(dirname "$0")"

docker compose up -d --build

echo
echo "Stack is starting. Once healthy:"
echo "  Frontend:  http://localhost:5173"
echo "  Backend:   http://localhost:8080/api/status"
echo "  Postgres:  localhost:5432 (keyrotate/keyrotate)"
echo
echo "Open the frontend and click 'Generate Genesis Key' to start the rotation clock."
echo "Tail logs with: docker compose logs -f"
