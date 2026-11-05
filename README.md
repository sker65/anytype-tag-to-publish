# anypub

anypub veröffentlicht Anytype-Objekte als statische Webseiten auf eigener Infrastruktur. Bekommt ein Objekt das Tag `publish`, rendert anypub es nach `public/p/<slug>/` und schreibt die öffentliche URL in die Property `Public URL` zurück. Wird das Tag entfernt, ist die Seite weg und die URL wieder leer.

anypub läuft als zusätzlicher Client neben `anytype-cli` (headless anytype-heart mit Bot-Account). Es nutzt nur die lokale HTTP-API v2 von anytype-cli und erkennt Änderungen per Push über einen Server-Sent-Events-Stream, ohne Polling.

## Stand

Umgesetzt sind die Meilensteine M1 bis M4 der Spec ([Spec Anytype Tag-to-Publish Service.md](Spec%20Anytype%20Tag-to-Publish%20Service.md)):

- Publish, Update und Unpublish über das Tag, pro Space opt-in
- URL-Rückschrieb in `Public URL`, dazu `Published At` und `Publish Error`
- Rendering aus dem AnyBlock-JSON mit Bildern, Dateien, Tabellen, Code-Highlighting, Light/Dark
- Links und Mentions auf andere veröffentlichte Objekte werden umgeschrieben, auf nicht veröffentlichte Objekte zu reinem Text
- Reconcile bei Start, Reconnect und optional stündlich
- SQLite-State, aus den `Public URL`-Werten wiederherstellbar
- `/healthz`, `/readyz`, `/metrics` (Prometheus), JSON-Logs mit Audit-Einträgen
- Dockerfile, Compose-Datei und nginx-vhost

Nicht umgesetzt:

- Zero-Knowledge-Modus (M5). `default_mode: zk` wird beim Start abgelehnt.
- OpenTelemetry-Traces. Es gibt Prometheus-Metriken, aber keinen OTLP-Export.

Getestet ist anypub automatisch gegen einen Fake-Server und am 5. Nov. 2026 von Hand gegen den kompletten Stack aus `deploy/` (anytype-cli v0.5.2 mit heart v0.51.6, any-sync-bundle 1.7.0). Live geprüft: Publish, Rückschrieb ohne zweiten Render, Untag, Archivieren, Neustart ohne Render und eine noch nicht existierende Tag-Option. Nicht live geprüft sind Änderungen durch einen menschlichen Nutzer, weil dafür ein zweiter Account nötig ist, sowie Bilder und Dateien.

## Abweichungen von der Spec

| Spec | Umsetzung | Grund |
| --- | --- | --- |
| gRPC (`ListenSessionEvents`, `ObjectSearchSubscribe`) mit App-Key | HTTP-API v2: `POST /v2/spaces/{id}/search/stream` (SSE) | Keys aus `anytype auth apikey create` haben Scope `JsonAPI`. Dieser Scope darf laut `core/auth.go` in heart keinen gRPC-Call ausführen. |
| Markdown über den Objekt-Endpoint | AnyBlock-JSON (`GET …/objects/{id}?ids=full`), eigener Renderer | Der Markdown-Export ersetzt Datei- und Objekt-IDs durch Dateinamen. Asset-Download und Link-Rewrite wären damit unmöglich. |
| Hash-Check gegen den Feedback-Loop | Events, deren `last_modified_by` der Bot ist, werden nicht gerendert. Der Hash-Check bleibt als zweite Sicherung. | Erfüllt das Akzeptanzkriterium „kein weiterer Render“ wörtlich. |
| `rename` von `tmp/` nach `public/p/<slug>/` | `p/<slug>` ist ein Symlink auf `.v/<version>`, der Symlink wird per `rename` getauscht | Ein volles Verzeichnis lässt sich nicht atomar ersetzen. |
| Collections | Nur das getaggte Objekt selbst wird veröffentlicht. Datenbank-Ansichten erscheinen als Platzhalter. | Entscheidung vom 5. Nov. 2026: erst einmal nur einzelne Objekte. |

## Ablauf

1. Beim Start löst anypub pro Space den Bot-Teilnehmer (`GET …/members/me`) und die Property-Keys auf. Fehlende Properties legt es an (`properties.create`).
2. Pro Space öffnet es einen SSE-Stream mit dem Filter `tag IN ("publish")`. Der Stream liefert zuerst einen Snapshot aller getaggten Objekte, danach `object_added`, `object_updated` und `object_removed`.
3. Der Snapshot dient als Reconcile: getaggte Objekte ohne aktuelle Seite werden gerendert, veröffentlichte Objekte ohne Tag depubliziert.
4. Events laufen durch eine Queue, die pro Objekt serialisiert. `object_updated` wird 1,5 s entprellt.
5. Bricht der Stream ab oder kommt drei Heartbeat-Intervalle lang nichts, verbindet anypub neu (1 s bis 30 s Backoff). Der neue Snapshot gleicht verlorene Events aus.

## Einrichtung

Der einfachste Weg ist der komplette Stack mit Sync-Netz, anytype-cli und anypub in `deploy/docker-compose.yml`, beschrieben in [deploy/README.md](deploy/README.md).

Ohne Docker: Voraussetzung ist ein laufendes anytype-cli mit Bot-Account im eigenen any-sync-Netz (siehe `SELF-HOSTED.md` im anytype-cli-Repository).

1. Bot-Account anlegen: `anytype auth create anypub-bot --network-config network.yml`
2. Den gewünschten Spaces per Invite beitreten, mit Rolle Editor: `anytype space join "<invite-link>"`
3. API-Key erzeugen und in eine Datei schreiben: `anytype auth apikey create anypub`
4. `anypub.example.yaml` kopieren, Space-IDs und `publish.base_url` eintragen.
5. DNS-Eintrag für den Hostnamen anlegen. `deploy/nginx/pub.example.com.conf` und `deploy/nginx/anypub-headers.conf` anpassen und installieren.
6. Starten, lokal mit `go run ./cmd/anypub -config anypub.yaml` oder per `deploy/docker-compose.yml`.

Prüfen: `curl 127.0.0.1:9464/readyz` antwortet `ready`, sobald alle Streams ihren Snapshot geliefert haben. Danach ein Objekt taggen; `Public URL` sollte nach wenigen Sekunden gesetzt sein.

Rechte im Ausgabeverzeichnis: Verzeichnisse `0750`, Dateien `0640`. nginx braucht Leserechte über die Gruppe, entweder über `publish.file_group` oder über die primäre Gruppe des Prozesses (in der Compose-Datei gid 33, `www-data`). Das Ausgabeverzeichnis auf dem Host muss für den anypub-Nutzer schreibbar sein.

## Layout von `out_dir`

```
p/<slug>        Symlink auf ../.v/<version>, von nginx ausgeliefert
.v/<version>/   index.html und a/<sha256>.<ext>, nicht direkt ausgeliefert
tmp/            Staging auf demselben Dateisystem
```

## Entwicklung

```
go test -race ./...
```

`internal/service/e2e_test.go` startet den kompletten Dienst gegen den Fake-Server in `internal/service/fake_test.go` und prüft Publish, Update, Unpublish, den Feedback-Loop, Reconnect, Neustart und den Verlust von `state.db`.

| Paket | Aufgabe |
| --- | --- |
| `internal/anyapi` | einziger Zugriff auf anytype-cli: HTTP-Client, SSE-Stream, Allowlist für Schreibzugriffe |
| `internal/watcher` | ein Stream pro Space, Reconnect, Snapshot-Reconcile, Queue mit Debounce und Retry |
| `internal/app` | Publish, Update, Unpublish, Assets, Rückschrieb |
| `internal/render` | AnyBlock-JSON zu HTML, Inline-Markup-Parser |
| `internal/publish` | Schreiben und Löschen unter `out_dir` |
| `internal/store` | SQLite-State |
| `internal/service` | Verdrahtung beim Start |

## Verhalten der echten API

Gegen anytype-cli v0.5.2 geprüft:

- Der Key der Tag-Property ist `tag`.
- `last_modified_by` kommt als `participant-<identity>`. Der Rückschrieb von anypub wird daran erkannt und nicht erneut gerendert.
- Archivieren entfernt das Objekt aus dem Stream (`object_removed`), anypub depubliziert. Das Leeren der Properties lehnt die API bei archivierten Objekten mit 403 ab. anypub loggt das und macht weiter. Weil die `Public URL` dadurch erhalten bleibt, sollte ein wiederhergestelltes Objekt seinen alten Slug zurückbekommen. Das ist nicht live geprüft.
- `set_properties` nimmt Datumswerte als RFC-3339-String an.
- Die API lehnt einen Filter ab, der eine noch nicht existierende Option nennt (400), und legt Optionen nur beim Schreiben eines Objekts an. anypub wertet das als „nichts getaggt“, meldet sich bereit und prüft alle `anytype.heartbeat` erneut.
- Space-IDs gehen in voller Form (`bafy….xyz`, aus `anytype space list`) oder in der Kurzform aus `GET /v2/spaces`. Pro Space sollte immer dieselbe Form konfiguriert bleiben, weil der State sie speichert.
- Einmal hing direkt nach dem Login der erste Stream-Aufruf ohne Antwort. Der Watchdog bricht jetzt auch das Warten auf die Response-Header nach drei Heartbeats ab.

Offen:

- Wenn ein Objekt neu veröffentlicht wird, rendert anypub Seiten, die bereits darauf verlinken, erst bei deren nächster Änderung neu.
