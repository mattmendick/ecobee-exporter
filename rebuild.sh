#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")"

# Leave the running exporter alone if the build fails.
docker compose build
docker compose up -d --force-recreate
