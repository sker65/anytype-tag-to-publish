# anypub

anypub publishes Anytype objects as static web pages on your own infrastructure. When an object gets the tag `publish`, anypub renders it to `public/p/<slug>/` and writes the public URL back into the property `Public URL`. When the tag is removed, the page is deleted and the URL is cleared.

anypub runs as an additional client next to `anytype-cli` (headless anytype-heart with a bot account). It uses only the local HTTP API v2 of anytype-cli. It detects changes by push through a Server-Sent Events stream, without polling.

## Status

Milestones M1 to M4 of the spec are implemented ([Spec Anytype Tag-to-Publish Service.md](Spec%20Anytype%20Tag-to-Publish%20Service.md), in German):

- Publish, update and unpublish through the tag, opt-in per space
- URL write-back to `Public URL`, plus `Published At` and `Publish Error`
- Rendering from the AnyBlock JSON with images, files, tables, code highlighting, light and dark mode
- Links and mentions to other published objects are rewritten; links to unpublished objects become plain text
- Reconcile on start, on reconnect and optionally every hour
- SQLite state, recoverable from the `Public URL` values
- `/healthz`, `/readyz`, `/metrics` (Prometheus), JSON logs with audit entries
- Dockerfile, Compose file and nginx vhost

Not implemented:

- Zero-knowledge mode (M5). `default_mode: zk` is rejected at startup.
- OpenTelemetry traces. There are Prometheus metrics, but no OTLP export.

anypub is tested automatically against a fake server. On 5 Nov 2026 it was also tested by hand against the complete stack from `deploy/` (anytype-cli v0.5.2 with heart v0.51.6, any-sync-bundle 1.7.0). Checked live: publish, write-back without a second render, untag, archive, restart without render, and a tag option that does not exist yet. Not checked live: edits by a human user, because that needs a second account, and images and files.

## Deviations from the spec

| Spec | Implementation | Reason |
| --- | --- | --- |
| gRPC (`ListenSessionEvents`, `ObjectSearchSubscribe`) with an app key | HTTP API v2: `POST /v2/spaces/{id}/search/stream` (SSE) | Keys from `anytype auth apikey create` have scope `JsonAPI`. According to `core/auth.go` in heart, this scope may not call any gRPC method. |
| Markdown from the object endpoint | AnyBlock JSON (`GET …/objects/{id}?ids=full`), own renderer | The Markdown export replaces file and object ids with file names. Asset download and link rewriting would be impossible. |
| Hash check against the feedback loop | Events whose `last_modified_by` is the bot are not rendered. The hash check stays as a second safeguard. | Meets the acceptance criterion "no further render" literally. |
| `rename` from `tmp/` to `public/p/<slug>/` | `p/<slug>` is a symlink to `.v/<version>`; the symlink is swapped with `rename` | A non-empty directory cannot be replaced atomically. |
| Collections | Only the tagged object itself is published. Database views appear as placeholders. | Decision of 5 Nov 2026: single objects only for now. |

## How it works

1. At startup anypub resolves, per space, the bot participant (`GET …/members/me`) and the property keys. It creates missing properties (`properties.create`).
2. Per space it opens an SSE stream with the filter `tag IN ("publish")`. The stream first sends a snapshot of all tagged objects, then `object_added`, `object_updated` and `object_removed`.
3. The snapshot serves as the reconcile: tagged objects without a current page are rendered, and published objects without the tag are unpublished.
4. Events go through a queue that serializes per object. `object_updated` is debounced by 1.5 s.
5. If the stream breaks, or nothing arrives for three heartbeat intervals, anypub reconnects (backoff 1 s to 30 s). The new snapshot makes up for lost events.

## Setup

The simplest way is the complete stack with sync network, anytype-cli and anypub in `deploy/docker-compose.yml`, described in [deploy/README.md](deploy/README.md).

Without Docker, you need a running anytype-cli with a bot account in your own any-sync network (see `SELF-HOSTED.md` in the anytype-cli repository).

1. Create the bot account: `anytype auth create anypub-bot --network-config network.yml`
2. Join the spaces by invite, with the Editor role: `anytype space join "<invite-link>"`
3. Create an API key and write it to a file: `anytype auth apikey create anypub --all-spaces --read-write`
4. Copy `anypub.example.yaml` and fill in the space ids and `publish.base_url`.
5. Create a DNS record for the host name. Adapt and install `deploy/nginx/pub.example.com.conf` and `deploy/nginx/anypub-headers.conf`.
6. Start it, locally with `go run ./cmd/anypub -config anypub.yaml` or with `deploy/docker-compose.yml`.

Check: `curl 127.0.0.1:9464/readyz` answers `ready` once all streams have delivered their snapshot. Then tag an object; `Public URL` should be set within a few seconds.

Permissions in the output directory: directories `0750`, files `0640`. nginx needs read access through the group, either through `publish.file_group` or through the primary group of the process (gid 33, `www-data`, in the Compose file). The output directory on the host must be writable for the anypub user.

## Layout of `out_dir`

```
p/<slug>        symlink to ../.v/<version>, served by nginx
.v/<version>/   index.html and a/<sha256>.<ext>, not served directly
tmp/            staging on the same file system
```

## Development

```
go test -race ./...
```

`internal/service/e2e_test.go` starts the complete service against the fake server in `internal/service/fake_test.go`. It checks publish, update, unpublish, the feedback loop, reconnect, restart, the loss of `state.db` and a missing tag option.

| Package | Purpose |
| --- | --- |
| `internal/anyapi` | the only access to anytype-cli: HTTP client, SSE stream, allowlist for writes |
| `internal/watcher` | one stream per space, reconnect, snapshot reconcile, queue with debounce and retry |
| `internal/app` | publish, update, unpublish, assets, write-back |
| `internal/render` | AnyBlock JSON to HTML, inline markup parser |
| `internal/publish` | writing and deleting under `out_dir` |
| `internal/store` | SQLite state |
| `internal/service` | wiring at startup |

The workflow `.github/workflows/image.yml` runs the tests and pushes the image `ghcr.io/sker65/anypub`, see [deploy/README.md](deploy/README.md#anypub-image).

## Behaviour of the real API

Checked against anytype-cli v0.5.2:

- The key of the tag property is `tag`.
- `last_modified_by` comes as `participant-<identity>`. anypub uses it to recognize its own write-back and does not render again.
- Archiving removes the object from the stream (`object_removed`), and anypub unpublishes. The API refuses to clear the properties of an archived object with 403. anypub logs this and continues. Because the `Public URL` stays, a restored object should get its old slug back. This is not checked live.
- `set_properties` accepts dates as RFC 3339 strings.
- The API refuses a filter that names an option that does not exist yet (400), and it creates options only when an object is written. anypub treats this as "nothing tagged", reports ready and checks again every `anytype.heartbeat`.
- Space ids work in full form (`bafy….xyz`, from `anytype space list`) or in the short form from `GET /v2/spaces`. Keep the same form configured for a space, because the state stores it.
- Once, directly after login, the first stream request hung without an answer. The watchdog now also aborts the wait for the response headers after three heartbeats.

Open:

- When an object is newly published, anypub re-renders pages that already link to it only on their next change.
