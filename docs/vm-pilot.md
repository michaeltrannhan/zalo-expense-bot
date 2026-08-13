# VM pilot (Oracle / any always-on host)

How to rebuild and restart the three bot processes after a `git pull`.
Postgres stays in Docker; the API and workers are Go binaries.

Startup order: **postgres → migrate → api + receipt-worker + notification-worker**.

## First-time host setup

Needs Docker, Go **1.25+**, and your user in the `docker` group.

```sh
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"
# SSH out and back in, then:
id | grep docker   # must list docker
go version         # go1.25.x
```

If `docker compose` says permission denied, you are still in the old login.
If `make: go: No such file or directory`, install Go 1.25 from https://go.dev/dl
(ARM Oracle A1: `linux-arm64`; AMD micro: `linux-amd64`) into `/usr/local/go`
and put `/usr/local/go/bin` on `PATH`.

Clone (or copy) the repo, then create `.env` from `.env.example`:

```sh
cp .env.example .env
chmod 600 .env
```

Live settings on Oracle (Gemini OCR + real Zalo replies, images in MinIO):

```env
APP_ENV=production
LISTEN_ADDR=127.0.0.1:8080
OBJECTSTORE=s3
S3_BUCKET=zl-expense-receipts
S3_ENDPOINT=http://127.0.0.1:9000
S3_REGION=us-east-1
MINIO_ROOT_USER=minioadmin
MINIO_ROOT_PASSWORD=minioadmin
AWS_ACCESS_KEY_ID=minioadmin
AWS_SECRET_ACCESS_KEY=minioadmin
ORIGINAL_RETENTION_DAYS=1
EXTRACTOR=gemini
GEMINI_API_KEY=...
EXTRACTION_ENABLED=true
OUTBOUND_ENABLED=true
PILOT_ALLOWLIST=<your Zalo sender id>
ZALO_BOT_TOKEN=...
ZALO_WEBHOOK_SECRET=<random, ≥16 chars, not the example placeholder>
```

`make up` / `make vm-rebuild` starts Postgres **and** MinIO, then creates the
bucket. Change `MINIO_ROOT_PASSWORD` (and the matching `AWS_SECRET_ACCESS_KEY`)
on a public VM. Console (local only): http://127.0.0.1:9001

`EXTRACTION_ENABLED` and `OUTBOUND_ENABLED` default to **false**. If either is
false, Gemini will not run and Zalo will not send. `ZALO_WEBHOOK_SECRET` cannot
be `dev-secret-change-me` in production.

The hourly retention sweep deletes MinIO objects whose `delete_after` has
passed (`ORIGINAL_RETENTION_DAYS=1` means yesterday's images go away; chat
history stays).

After editing `.env` on the VM:

```sh
make vm-rebuild
```

```sh
sudo mkdir -p /var/lib/zl-expense
sudo chown "$USER" /var/lib/zl-expense
```

MinIO keeps objects in the `minio-data` Docker volume. `DATA_DIR` is still used
for `/xuatdulieu` exports.

Install systemd units (paths are filled from the current clone):

```sh
sh scripts/install-vm-services.sh
```

The API unit starts with `-poll` so Zalo works without a public HTTPS webhook.
To switch to a webhook behind Caddy, edit `zl-expense-api.service`, drop
`-poll`, `daemon-reload`, and restart. Webhook URL is
`https://<host>/webhook/zalo`. Health: `GET /healthz`.

## Rebuild after every code change

From the repo root, as the same user that owns the clone:

```sh
git pull
sh scripts/vm-rebuild.sh
```

That script:

1. `docker compose up -d postgres`
2. `make migrate` (idempotent)
3. `make build` (rewrites `bin/api`, `bin/receipt-worker`, `bin/notification-worker`)
4. restarts the three systemd units if they are installed

Equivalent by hand:

```sh
docker compose up -d postgres
make migrate
make build
sudo systemctl restart zl-expense-api zl-expense-receipt-worker zl-expense-notification-worker
```

Do **not** `sudo make migrate`. Root would miss your `.env` and your `PATH`.

## Check that everything is up

```sh
docker compose ps
curl -sS http://127.0.0.1:8080/healthz
sudo systemctl status zl-expense-api zl-expense-receipt-worker zl-expense-notification-worker
journalctl -u zl-expense-api -u zl-expense-receipt-worker -u zl-expense-notification-worker -f
```

A healthy API answers `ok` (or similar) on `/healthz`. Worker logs should show
`receipt worker starting` and `notification worker starting` with no config
error about allowlist, webhook secret, or `DATABASE_URL`.

## Stop / start without rebuilding

```sh
sudo systemctl stop zl-expense-api zl-expense-receipt-worker zl-expense-notification-worker
sudo systemctl start zl-expense-api zl-expense-receipt-worker zl-expense-notification-worker
```

Postgres only:

```sh
docker compose stop postgres    # keep the volume
docker compose up -d postgres
```

`docker compose down -v` **wipes the database**. Do not use `-v` on the live VM
unless you intend to reset all users and receipts.

## Laptop (no systemd)

```sh
make run-local API_FLAGS=-poll
```

Ctrl-C stops API + both workers. Postgres stays up until `make down`.

## Storage reminder

- Images: MinIO (`OBJECTSTORE=s3`, `S3_ENDPOINT=http://127.0.0.1:9000`) or
  `$DATA_DIR` when `OBJECTSTORE=local`.
- The hourly sweep deletes image bytes after `ORIGINAL_RETENTION_DAYS` (default 1).
- Source of record: PostgreSQL. Summaries survive after image prune.
