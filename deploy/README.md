# Docker Compose stack

`docker-compose.yml` starts three containers:

| Service | Image | Purpose |
| --- | --- | --- |
| `any-sync` | `ghcr.io/grishy/any-sync-bundle` | sync network (coordinator, consensus, tree, file) in one container, with embedded MongoDB and Redis. Starts only with `--profile sync`. |
| `anytype-cli` | `ghcr.io/anyproto/anytype-cli` | headless client with the bot account, serves the HTTP API on `127.0.0.1:31012` |
| `anypub` | `ghcr.io/sker65/anypub`, built by `.github/workflows/image.yml` | publishes tagged objects to `PUBLIC_DIR` |

`any-sync-bundle` is a community project that packs the official any-sync nodes into one binary. The official Anytype setup is [any-sync-dockercompose](https://github.com/anyproto/any-sync-dockercompose) with about ten containers.

nginx with TLS runs on the host and serves `PUBLIC_DIR`, see `nginx/`.

## Existing sync network

If any-sync is already running, the service `any-sync` must not start: it would create a second, new network with its own keys. That is why it belongs to the profile `sync` and does not start without `--profile sync`.

anytype-cli then needs only the `client-config.yml` of the existing network. Copy it to a directory of its own and point `SYNC_CONFIG_DIR` in `.env` at it:

```sh
mkdir -p sync && cp /path/to/any-sync/data/client-config.yml sync/
echo SYNC_CONFIG_DIR=./sync >> .env
```

Mounting the whole data directory of the bundle also works, but gives anytype-cli read access to `bundle-config.yml`, which holds the private keys of the network. Renew the copy when the external addresses of the network change.

anytype-cli reaches the server through the addresses in `client-config.yml`, that is, through the public name. If the sync server runs on the same host and the name is not reachable from there, add this under `anytype-cli` in `docker-compose.yml`: `extra_hosts: ["sync.example.com:host-gateway"]`.

Before updating the bundle to the version in `docker-compose.yml`, stop it cleanly and back up its data directory. According to the bundle README, the configuration format stays readable within 1.x.

## First start

Run all commands in this directory.

1. Create the configuration:

   ```sh
   cp .env.example .env              # fill in SYNC_HOST, PUB_BASE_URL, PUBLIC_DIR
   cp ../anypub.example.yaml anypub.yaml
   touch anytype_api_key
   ```

   `SYNC_HOST` is the name under which the Anytype apps reach the server. any-sync reads it only on its first start and then writes it to `data/any-sync/bundle-config.yml`.

2. Start the client; for a new sync network, add the profile `sync`:

   ```sh
   docker compose up -d anytype-cli                          # existing network
   docker compose --profile sync up -d any-sync anytype-cli  # new network
   ```

   A new network writes `data/any-sync/client-config.yml`. Firewall: TCP 33010 and UDP 33020 must be open.

3. Create the bot account in your network:

   ```sh
   docker compose exec anytype-cli anytype auth create anypub-bot --network-config /sync/client-config.yml
   ```

   The command prints the account key. Keep it safe, it is the only way into the bot account. anytype-cli stores it under `data/anytype-config/` and logs in with it again after a restart.

4. Create the API key for anypub and write it to the secret file:

   ```sh
   docker compose exec anytype-cli anytype auth apikey create anypub --all-spaces --read-write
   ```

   Write the value after `Key:` to `anytype_api_key`, without a line break. anypub runs as uid 10001, gid 33 and must be able to read the file, for example after `chgrp 33 anytype_api_key && chmod 640 anytype_api_key`.

5. For a new network only: connect the Anytype desktop app to it by importing `data/any-sync/client-config.yml` under "Self-hosted" at login. A vault in your own network is separate from a vault in the Anytype network. Existing content has to be exported and imported.

6. In the app, share the space, create an invite link with the Editor role and let the bot join:

   ```sh
   docker compose exec anytype-cli anytype space join "<invite-link>"
   docker compose exec anytype-cli anytype space list
   ```

   Without the Editor role, anypub cannot write back the `Public URL`.

7. Put the space id from `space list` into `ANYPUB_SPACES` in `.env` and start anypub. The image comes from ghcr.io; `--build` builds it from this checkout instead.

   ```sh
   docker compose up -d anypub
   docker compose logs -f anypub
   ```

   The log shows `anypub started` and then `stream live` per space. If no object in the space has ever carried the tag `publish`, anypub logs `tag option does not exist yet` instead. It then checks again every 30 s. The stream starts as soon as the first object is tagged.

Check: `docker compose exec anytype-cli wget -qO- 127.0.0.1:9464/readyz` prints `ready`.

## Data and backup

| Path | Content | Back up |
| --- | --- | --- |
| `data/any-sync/` | sync data and `bundle-config.yml` with the keys of the network | yes, after `docker compose stop any-sync` |
| `data/anytype/`, `data/anytype-config/` | the bot's local copy and its account key | yes |
| volume `anypub-state` | `state.db`, recoverable from the `Public URL` values | optional |
| `PUBLIC_DIR` | generated pages, reconciled again at startup | no |

`.env`, `anypub.yaml`, `anytype_api_key`, `sync/` and `data/` contain secrets and do not belong in a repository. `deploy/.gitignore` excludes them.

## anypub image

`.github/workflows/image.yml` tests and builds the image for linux/amd64 and linux/arm64:

| Trigger | Tags in `ghcr.io/sker65/anypub` |
| --- | --- |
| push to `main` | `main`, `sha-<commit>` |
| tag `v1.2.3` | `1.2.3`, `1.2`, `latest`, `sha-<commit>` |
| pull request | build only, no push |

A new package on ghcr.io starts out private. So that the server can pull it without logging in, set its visibility to "Public" at github.com/users/sker65/packages/container/anypub/settings. Otherwise run `docker login ghcr.io` on the server with a token that has `read:packages`.

`ANYPUB_VERSION` in `.env` selects the tag; the default is `main`.

## Updates

The image versions of any-sync and anytype-cli are pinned. To update, change the tags in `docker-compose.yml` and run `docker compose pull && docker compose up -d`. Back up `data/any-sync/` before updating any-sync.
