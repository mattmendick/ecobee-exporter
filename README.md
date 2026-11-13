# ecobee-exporter

A read-only Prometheus exporter for ecobee thermostats and room sensors.

This fork incorporates [ejsuncy/ecobee-exporter at e5bf92252781](https://github.com/ejsuncy/ecobee-exporter/commit/e5bf922527813284d961dc47405c93588c5f692b),
including its extended-runtime metrics, and builds on Billy Wooten's original
exporter and [dichro/ecobee](https://github.com/dichro/ecobee).

## Rebuild and test an existing deployment

Requirements on the Docker host: Docker with the Compose plugin, Bash, and this
checkout. Go is only needed inside the build container. Nothing is pushed to a registry.

Create a `.env` file alongside `compose.yaml`, containing your existing application
key (this file is ignored by Git and excluded from Docker builds):

```dotenv
ECOBEE_APPKEY=your-existing-key
```

The Compose file reuses the existing external volume
`ecobee-exporter_ecobee-exporter`. No reauthorization is needed. It runs as root
for compatibility with the existing cache ownership; new standalone images otherwise
default to UID/GID 65532. Token writes are atomic with permissions `0600`.

### First migration from docker run

Take a secure backup of the token volume first. Find the old container's name
(Docker may have assigned a random name), build before stopping it, then start Compose:

```sh
docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Ports}}'
docker compose build
docker stop YOUR_OLD_CONTAINER_NAME
docker compose up -d
```

Keep the old container stopped for rollback. Never run both exporters against the
same authorization. Do not delete the external volume or revoke the app to upgrade.

### Subsequent rebuilds

```sh
./rebuild.sh
```

This simply runs `docker compose build`, then `docker compose up -d --force-recreate`.
Build failures leave the current exporter running. There is no automatic API
readiness check or rollback; check the logs and `ecobee_up` after replacement:

```sh
docker compose logs --tail=50 ecobee-exporter
curl -fsS http://localhost:9098/metrics | grep '^ecobee_up '
```

For rollback to the original standalone container, run `docker compose down`,
then `docker start YOUR_OLD_CONTAINER_NAME`. The external token volume is retained,
and its JSON format is compatible with the old exporter. Do not restore stale token
backups over the live cache as part of an ordinary rollback.

## API behavior

The exporter uses the [documented ecobee v1 API](https://www.ecobee.com/home/developer/api/introduction/index.shtml)
with `smartRead` authorization only. It never changes thermostat settings.

- Background polling defaults to **three minutes**, the minimum allowed by
  [ecobee's polling guidance](https://www.ecobee.com/home/developer/api/documentation/v1/operations/get-thermostat-summary.shtml).
- Each cycle checks thermostat summary revisions. Full thermostat data is fetched
  initially and when the revision map changes; unchanged summaries reuse cached data.
  A changed map currently refreshes all registered thermostats in one request.
- Prometheus scrapes only read memory; a 60-second scrape interval adds no API calls.
- Requests are serialized and time-limited. Failed polls back off to as much as
  30 minutes (or the configured interval if longer); HTTP 429 also honors `Retry-After`.
- The last known good data is retained temporarily on failure, then omitted once
  `max-cache-age` elapses without a successful poll.
- Disconnected thermostats expose `ecobee_connected 0` without sensor/temperature
  samples. Missing, unknown, calibrating, and non-finite readings are omitted.
- Short/missing extended-runtime arrays are omitted rather than indexed unsafely.
- Multiple thermostats in one response are supported; paginated responses currently
  fail explicitly rather than silently expose only part of an account.

## Configuration

| Environment | Flag | Default |
| --- | --- | --- |
| `ECOBEE_LISTEN_ADDRESS` | `--listen-address` | `:9098` |
| `ECOBEE_APPKEY` | `--appkey` | Required; no shared default |
| `ECOBEE_CACHEFILE` | `--cachefile` | `/db/auth.cache` |
| `ECOBEE_POLL_INTERVAL` | `--poll-interval` | `3m` (minimum `3m`) |
| `ECOBEE_MAX_CACHE_AGE` | `--max-cache-age` | `15m` (at least the polling interval) |
| `ECOBEE_API_TIMEOUT` | `--api-timeout` | `30s` (per HTTP request) |

Flags override environment variables. `--version` prints build information.
`/healthz` reports HTTP process liveness, not ecobee connectivity.

There is no need to reauthorize when migrating a working cache. If authorization
is actually missing/expired, run an explicit interactive command with your existing
application key and a writable cache directory:

```sh
docker build -t ecobee-exporter:local .
# Set ECOBEE_APPKEY securely in your shell first. Do not commit it.
docker run --rm -it --user 0:0 \
  -e ECOBEE_APPKEY \
  -v ecobee-exporter_ecobee-exporter:/db \
  ecobee-exporter:local --auth
```

Stop the regular exporter before authorizing. The command prints a PIN; authorize
it in ecobee's My Apps portal, then press Enter. Normal server mode never prompts
for input or silently replaces a corrupt cache. ecobee currently does not accept
[new developer registrations](https://www.ecobee.com/en-us/developers/), so keep
your existing application authorization safe.

## Metrics and compatibility

Existing names and labels, including sensor IDs and runtime stage labels, are
preserved. Temperatures remain **Fahrenheit**, not Celsius. One intentional
correction: `ecobee_currenthvacmode` and `ecobee_last_interval_energized_state`
now use value **1**, rather than 0, for the mode named in their label. Review
queries that depended on their former zero value.

New exporter metrics:

| Metric | Meaning |
| --- | --- |
| `ecobee_up` | Last polling cycle succeeded; initially 0 |
| `ecobee_poll_errors_total` | Failed polling cycles since startup |
| `ecobee_last_successful_poll_timestamp_seconds` | Last successful API check, even if revisions were unchanged |
| `ecobee_last_data_fetch_timestamp_seconds` | Last full thermostat fetch, **not** the time the thermostat observed the data |
| `ecobee_connected` | Connection state per thermostat |
| `ecobee_exporter_build_info` | Build version and revision |

`ecobee_fetch_time` now measures the latest background polling cycle.
Health timestamps are absent until the first successful cycle. Cached values can
remain visible while `ecobee_up` is 0; use the health metrics when alerting.
A successful unchanged summary validates the cache but does not imply fresh
device telemetry. ecobee cloud data is not an instantaneous local-device feed.

`ecobee_last_interval_runtime` is a **gauge**, in seconds for the latest reported
five-minute interval. Do not apply `rate()` or treat repeated scrapes as new
runtime intervals. Occupancy means motion detected within the preceding 30 minutes,
not instantaneous motion.

Example Prometheus configuration:

```yaml
scrape_configs:
  - job_name: ecobee-exporter
    scrape_interval: 60s
    static_configs:
      - targets: ['YOUR_DOCKER_HOST:9098']
```

Expose this endpoint only on a trusted network; metrics include thermostat and
room names. Credentials remain secrets even though the rebuild script does not
print them: Docker users can inspect container arguments/environment.

## Development

Requires Go 1.27.4+.

```sh
go test -race ./...
go vet ./...
bash -n rebuild.sh
go build .
```

Tests use synthetic data and local mock HTTP servers, never real credentials.
`./script/cibuild` also builds a local native-architecture image; it does not push.
