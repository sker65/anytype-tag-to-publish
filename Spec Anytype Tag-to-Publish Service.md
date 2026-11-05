# Spec: Anytype Tag-to-Publish Service

Nov 5, 2026 · @Stefan Rinke

## Ziel, Scope, Non-Goals

Ein Go-Service veröffentlicht Anytype-Objekte als Webseite auf eigener Infrastruktur, sobald sie ein Tag bekommen, und schreibt die öffentliche URL ins Objekt zurück. Ziel-Latenz: Tag gesetzt → URL im Objekt sichtbar in **< 5 s (p95)**, ohne Polling.

**In Scope**

- Publish/Unpublish gesteuert ausschließlich über ein Tag (Default `publish`) in beliebigen, normalen Spaces – kein eigener Public-Space nötig.
- Push-basierte Erkennung über die Event-Stream-Subscription von anytype-heart.
- Re-Rendering bei Inhaltsänderungen veröffentlichter Objekte.
- URL-Rückschrieb in eine Property des Objekts (in allen Clients sichtbar, kopierbar).
- Zwei URL-Modi: unratbarer Slug oder Zero-Knowledge (Key im URL-Fragment).
- Ausliefern über eigenen Webserver unter eigener Domain.

**Non-Goals**

- Kein Ersatz für Anytypes Publish-Feature im Client, kein Client-Fork.
- Kein Editieren über das Web, keine Kommentare, keine Suche.
- Kein Multi-User-Betrieb: ein Betreiber, ein Bot-Account.
- Keine Garantie gegen Weiterverbreitung nach dem Depublizieren.

## Architektur & Komponenten

anypub ist ein zusätzlicher Client im eigenen any-sync-Netz: Er hängt über anytype-cli als Bot-Account in deinen Spaces, reagiert auf gepushte Tag-Events und schreibt statische Seiten, die der bestehende nginx unter einem neuen Hostnamen ausliefert.

&#91;embedded content: Architektur · 6 Komponenten, Push statt Polling\]

Die Daten verlassen dein Netz nur als gerenderte Seite über nginx; Anytype-Infrastruktur ist nicht beteiligt.

| Komponente | Rolle | Neu zu bauen |
| --- | --- | --- |
| any-sync-bundle | bestehender Sync-Server | nein |
| anytype-cli | headless anytype-heart mit Bot-Account, HTTP-API v2 lokal (Port 31012) | nein (Setup) |
| anypub | Go-Service: Subscription, Rendering, Rückschrieb, State | **ja** |
| `state.db` | SQLite: Objekt → Slug, Hash, Assets | ja (Teil von anypub) |
| nginx (bestehend) | liefert `public/` statisch aus, nutzt vorhandenes Wildcard-Zertifikat | nein (neuer vhost) |

**Code-Struktur (Vorschlag)**

- `cmd/anypub` – main, Config, Wiring
- `internal/anyapi` – HTTP-Client für die API v2: SSE-Stream, Objekt lesen, Properties schreiben (mit Allowlist), Datei-Download. Einzige Stelle, die mit anytype-cli spricht.
- `internal/watcher` – ein Stream pro Space, Keyed Queue, Debounce, Reconcile
- `internal/app` – Publish, Update, Unpublish, Assets, Rückschrieb, Hooks
- `internal/render` – AnyBlock-JSON → HTML, Link-Rewrite
- `internal/store` – SQLite-State
- `internal/publish` – atomares Schreiben/Löschen unter `public/`
- `internal/service` – Verdrahtung beim Start

## Event-getriebene Erkennung (kein Polling)

Der Service nutzt den SSE-Search-Stream der HTTP-API v2 von anytype-cli (`POST /v2/spaces/{space_id}/search/stream`, Port 31012). Intern legt heart dafür eine Live-Subscription an, denselben Mechanismus wie die Clients. Änderungen an Objekten, die den Filter erfüllen, kommen per Push, sobald sie per Sync ankommen.

**Warum nicht gRPC**: Keys aus `anytype auth apikey create` haben Scope `JsonAPI`. Laut `core/auth.go` in anytype-heart v0.51.6 darf dieser Scope keinen gRPC-Call ausführen. Ein Key mit Scope `Limited` dürfte zwar Events abonnieren, aber keine Properties schreiben. Ein voller Scope ginge nur über den Account-Key des Bots.

**Mechanik**

1. Authentisierung: API-Key als `Authorization: Bearer <key>`. `GET /v2/auth/whoami` prüft beim Start die Verbindung.
2. Pro konfiguriertem Space: Bot-Teilnehmer über `GET …/members/me` auflösen, Property-Keys über `GET …/properties`.
3. Pro Space einen Stream öffnen, Body `{"filter": "tag IN (\"publish\")", "fields": [tag, last_modified_date, last_modified_by, <public_url>, <published_at>, <publish_error>]}`.
4. Der Stream liefert zuerst `object_added` pro getaggtem Objekt und dann `snapshot_complete`. Danach live `object_added` (Tag gesetzt), `object_updated` (ein angefragtes Feld hat sich geändert, auch `last_modified_date` bei Inhaltsänderungen) und `object_removed` (Tag entfernt, archiviert, gelöscht).
5. Eigene Rückschriebe: heart setzt `last_modified_by` auf den Teilnehmer, der die Änderung gemacht hat. Ein `object_updated` vom Bot wird nicht gerendert.

**Fallback**: Der Stream hat keine Event-IDs. Bricht er ab oder kommt drei Heartbeat-Intervalle lang nichts, verbindet der Service neu (Backoff 1 s bis 30 s). Der neue Snapshot dient als Reconcile (siehe Abläufe). Ein optionaler Safety-Reconcile (Default 1 h) erzwingt einen Reconnect.

## Datenmodell

Steuerung und Ergebnis leben in Anytype-Properties; der Service hält nur einen kleinen lokalen State zur Zuordnung und Idempotenz.

**Anytype-Seite (pro Space)**

| Element | Typ | Zweck |
| --- | --- | --- |
| Tag `publish` (konfigurierbar) | Option der System-Relation `tag` | Trigger: gesetzt = veröffentlichen, entfernt = depublizieren |
| `Public URL` | Relation vom Typ URL | Rückschrieb der veröffentlichten URL; leer = nicht veröffentlicht |
| `Published At` (optional) | Relation vom Typ Datum | Zeitpunkt des letzten erfolgreichen Renders |
| `Publish Error` (optional) | Relation vom Typ Text | Fehlermeldung, falls Rendern scheitert |

Beim Start löst der Service Tag-Name → Option-ID und Relation-Name → Relation-Key pro Space auf. Fehlende Relations legt er an (Flag `--create-relations`, Default an).

**Lokaler State (SQLite, `state.db`)**

| Spalte | Typ | Bedeutung |
| --- | --- | --- |
| `object_id` | text, PK | Anytype-Objekt-ID |
| `space_id` | text | Space |
| `slug` | text, unique | Pfadsegment der URL |
| `mode` | text | `slug` oder `zk` |
| `zk_key` | blob, nullable | AES-Key (nur `zk`, verschlüsselt mit Master-Key) |
| `content_hash` | text | SHA-256 des gerenderten Outputs (ohne Bot-Properties) |
| `asset_ids` | json | hochgeladene Asset-Dateien für Cleanup |
| `published_at` / `updated_at` | timestamp | Zeitstempel |

Der State ist rekonstruierbar: Geht `state.db` verloren, liefert der Reconcile alle getaggten Objekte; vorhandene `Public URL` wird geparst und der Slug übernommen (für `zk` geht der Key verloren → Neupublikation mit neuer URL).

## Abläufe

Alle Events laufen pro `object_id` serialisiert durch einen Worker (Keyed Queue); unterschiedliche Objekte werden parallel verarbeitet (Default 4 Worker).

**Publish (`subscriptionAdd`)**

1. Objekt im State? Ja → wie Update behandeln. Nein → Slug erzeugen (128 bit, base64url), bei `zk` zusätzlich Key.
2. Objekt als Markdown lesen, Assets laden, rendern (siehe Rendering).
3. Output atomar schreiben: in `tmp/` stagen, nach `.v/<version>/` verschieben, dann den Symlink `p/<slug>` per `rename` auf die neue Version umhängen.
4. `Public URL` (+ `Published At`, `Publish Error` leeren) per PATCH-Op `set_properties` setzen.
5. State schreiben. Ziel: Schritte 1–4 < 1 s für Text-Objekte.

**Update (`objectDetailsAmend` mit neuem `lastModifiedDate`)**

1. Debounce pro Objekt: 1,5 s nach dem letzten Event (Tippen erzeugt Event-Serien).
2. Stammt jedes zusammengefasste Event vom Bot (`last_modified_by`), nicht rendern: das ist das Echo des eigenen Rückschriebs. Sonst rendern und Hash berechnen; Hash gleich → nichts schreiben.
3. Sonst Output ersetzen, Assets diffen, verwaiste löschen, `Published At` setzen.

**Unpublish (`subscriptionRemove`, oder Objekt archiviert/gelöscht)**

1. Verzeichnis `public/p/<slug>/` und Assets löschen.
2. Optional CDN-Purge (Hook).
3. `Public URL` leeren, sofern das Objekt noch existiert.
4. State-Zeile löschen. Erneutes Taggen erzeugt eine **neue** URL (Default; `keep_slug_on_republish: true` schaltet um).

**Reconcile (Start, Reconnect, optional stündlich)**

1. Initial-Records der Subscription = Soll-Menge; State = Ist-Menge.
2. Soll ohne Ist → Publish. Ist ohne Soll → Unpublish. Beide → Update-Check per Hash.
3. Verwaiste Verzeichnisse unter `public/p/` ohne State-Zeile löschen.

**Fehler**: Render- oder Schreibfehler → `Publish Error` setzen, Retry mit Backoff (max. 5), dann aufgeben bis zum nächsten Event. Schreibfehler auf Anytype selbst nur loggen und erneut versuchen.

## Rendering, Assets, URL-Modi

Ein Objekt wird als statische, selbsttragende Seite gerendert; kein Server-Code beim Ausliefern.

**Rendering**

- Inhalt als AnyBlock-JSON lesen (`GET …/objects/{id}?ids=full`); Titel aus `name`, Icon als Emoji. Der Markdown-Export (`?format=md`) ist ungeeignet, weil er Datei- und Objekt-IDs durch Dateinamen ersetzt.
- Blöcke → HTML mit eigenem Renderer (Inline-Markup laut any-block `INLINE_MARKUP.md`, Tabellen, Tasklists, Syntax-Highlighting via chroma), ein `html/template`-Layout, inline CSS, Light/Dark über `prefers-color-scheme`.
- Nur das getaggte Objekt wird veröffentlicht. Ist es eine Collection oder Query, erscheinen Datenbank-Ansichten als Platzhalter; die enthaltenen Objekte werden nicht mitveröffentlicht.
- Links auf andere Anytype-Objekte (`anytype://…`): veröffentlicht → auf deren Public URL umschreiben; nicht veröffentlicht → als reinen Text ausgeben.
- `<meta name="robots" content="noindex">` per Default (konfigurierbar).
- Nicht unterstützte Blöcke (Datenbank-Views, Embeds) als Platzhalter-Hinweis rendern, nicht scheitern.

**Assets**

- Bilder/Dateien über `GET …/files/{file_id}/content` herunterladen, unter `public/p/<slug>/a/<sha256>.<ext>` ablegen, Referenzen umschreiben. Dateiobjekte sind unveränderlich; bei einem Update werden bekannte Assets aus der vorigen Version kopiert statt neu geladen.
- Größenlimit pro Asset (Default 25 MB); darüber → Platzhalter + `Publish Error`-Hinweis.

**URL-Modi** (global per Config, pro Objekt überschreibbar durch ein zweites Tag `publish-zk`)

| Modus | URL | Server sieht Klartext | Aufwand |
| --- | --- | --- | --- |
| `slug` (Default) | `https://pub.example.com/p/<slug>` | ja | gering |
| `zk` | `https://pub.example.com/z/<slug>#<key>` | nein | mittel |

**`zk`-Details**: HTML-Body und Assets mit AES-256-GCM verschlüsseln (Key 256 bit, Nonce pro Datei). Ausgeliefert wird eine generische Viewer-Seite mit kleinem JS (WebCrypto), das den Key aus `location.hash` liest, `content.bin` lädt, entschlüsselt und einsetzt; Assets werden als Blob-URLs nachgeladen. Der Fragment-Teil wird nie an den Server gesendet. Die Public URL in Anytype enthält den Key; wer die URL hat, kann lesen.

## Sicherheit & Berechtigungen

Ohne eigenen Public-Space ist der Bot ein vollwertiges Editor-Mitglied deiner normalen Spaces und damit ein weiteres Gerät mit allen Space-Keys. Das ist der zentrale Trade-off; die Maßnahmen begrenzen den Schaden, nicht den Zugriff.

- **Eigener Bot-Account** (`anytype auth create`), nie der persönliche Account. Entzug jederzeit über Space-Mitgliedschaft möglich.
- **Rolle Editor** nötig (für den URL-Rückschrieb). Opt-in pro Space: der Bot verarbeitet nur Spaces aus der Config, auch wenn er Mitglied weiterer ist.
- **Netzwerk**: anytype-cli-Ports (31010–31012) nur auf `127.0.0.1` bzw. im Netzwerk-Namespace des Compose-Service; der Bot synct ausschließlich über das eigene any-sync-Netz (`client-config.yml`).
- **Daten im Ruhezustand**: Bot-Datenverzeichnis und `state.db` auf verschlüsseltem Volume; `zk`-Keys zusätzlich mit einem Master-Key aus Env/Vault verschlüsselt.
- **Schreibrechte des Service auf Anytype**: nur die drei eigenen Properties; Code-seitig erzwungen durch eine Allowlist im Adapter (jeder andere Key in `set_properties` wird abgelehnt, bevor ein Request rausgeht). Der API-Key lässt sich zusätzlich serverseitig auf einzelne Spaces beschränken.
- **Webserver** sieht nur `public/`, read-only gemountet; kein Directory-Listing; `Referrer-Policy: no-referrer`, strikte CSP.
- **Audit-Log**: jede Publish/Unpublish-Aktion mit Objekt-ID, Space, Slug (ohne Key), Zeit.
- **Optional**: Public-Space bleibt als Konfigurationsoption möglich (`spaces: [public-space-id]`), dann braucht der Bot keinen Zugriff auf private Spaces.

## Konfiguration, Deployment, Observability

Ein Binary (`anypub`), eine YAML-Config, Deployment als zusätzliche Services neben dem any-sync-bundle.

**Config (`anypub.yaml`, alle Werte per Env überschreibbar)**

```yaml
anytype:
  api_addr: http://127.0.0.1:31012
  api_key_file: /run/secrets/anytype_api_key
  tag_property: tag
  heartbeat: 30s
spaces:            # Opt-in; leer = Fehler
  - id: bafyrei...
    tag: publish
properties:
  public_url: Public URL
  published_at: Published At
  publish_error: Publish Error
  create: true
publish:
  base_url: https://pub.example.com
  out_dir: /srv/anypub/public
  default_mode: slug   # slug | zk
  keep_slug_on_republish: false
  noindex: true
  file_group: www-data   # Gruppe des nginx-Users
  debounce: 1500ms
  workers: 4
  max_asset_bytes: 26214400
  safety_reconcile: 1h  # 0 = aus
  max_retries: 5
state:
  path: /var/lib/anypub/state.db
hooks:
  on_unpublish: ""      # z.B. CDN-Purge-Kommando
observability:
  listen_addr: 127.0.0.1:9464
  log_level: info
```

**Deployment (Compose)**

- `anytype-cli`: Container mit `anytype serve`, Volume für Bot-Daten, `client-config.yml` des eigenen Netzes gemountet. Ports nur intern.
- `anypub`: teilt das Netzwerk-Namespace mit `anytype-cli` (oder internes Netz), schreibt nach Bind-Mount `/srv/anypub/public` auf dem Host. Öffnet keinen öffentlichen Port; Health/Metrics nur auf `127.0.0.1:9464`.
- Kein eigener Webserver: der bestehende nginx (Port 80/443) bekommt einen zusätzlichen vhost für den neuen Hostnamen, z. B. `pub.example.com`, abgedeckt durch das vorhandene Wildcard-Zertifikat `*.example.com`. Kein neues Zertifikat, keine Port-Konflikte.
- Einmal-Setup: DNS-Eintrag für den Hostnamen, Bot-Account anlegen, per Invite den gewünschten Spaces beitreten (Editor), App-Key erzeugen.

**nginx-vhost (Vorlage)**

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name pub.example.com;

    # vorhandenes Wildcard-Zertifikat wiederverwenden
    ssl_certificate     /etc/letsencrypt/live/example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/example.com/privkey.pem;

    root /srv/anypub/public;
    autoindex off;

    add_header Referrer-Policy no-referrer always;
    add_header X-Robots-Tag "noindex, nofollow" always;
    add_header X-Content-Type-Options nosniff always;
    add_header Content-Security-Policy "default-src 'self'; img-src 'self' blob: data:; style-src 'self' 'unsafe-inline'" always;

    location /p/ { try_files $uri $uri/index.html =404; }
    location /z/ { try_files $uri $uri/index.html =404; }
    location /   { return 404; }
}

server {
    listen 80;
    server_name pub.example.com;
    return 301 https://$host$request_uri;
}
```

**Anforderungen an anypub dafür**

- `out_dir` muss für den nginx-User lesbar sein: Verzeichnisse `0750`, Dateien `0640`, Gruppe konfigurierbar (`publish.file_group`, z. B. `www-data`).
- Atomares Ersetzen per `rename` innerhalb desselben Dateisystems: `tmp/` liegt unter `out_dir`, wird aber von nginx nicht ausgeliefert (`location /` → 404).
- Kein Caching-Problem beim Depublizieren: Seiten mit `Cache-Control: no-cache` ausliefern (per nginx `add_header` oder Meta), Assets mit Content-Hash im Namen dürfen lange gecacht werden.

**Observability**

- OpenTelemetry: Traces pro Event (Span-Kette receive → render → write → setDetails), OTLP-Export konfigurierbar.
- Metriken: `anypub_publish_latency_seconds` (Event-Eingang bis URL geschrieben), `anypub_events_total{type}`, `anypub_stream_reconnects_total`, `anypub_published_objects`.
- `/healthz` (Prozess) und `/readyz` (Stream verbunden, Subscriptions aktiv) auf internem Port.
- Strukturierte Logs (`slog`, JSON), nie Keys oder Inhalte loggen.

## Akzeptanzkriterien, Meilensteine, Risiken

Fertig ist der Service, wenn alle Kriterien in einer Integrationsumgebung (bundle + anytype-cli + anypub in Compose) automatisiert grün sind.

**Akzeptanzkriterien**

- [ ] Tag am Desktop gesetzt → `Public URL` im Objekt in < 5 s (p95), ohne Polling-Code im Hot Path.
- [ ] Tag entfernt → Seite liefert 404 und `Public URL` ist leer in < 5 s.
- [ ] Textänderung an veröffentlichtem Objekt → Seite aktualisiert in < 5 s nach letzter Eingabe.
- [ ] URL-Rückschrieb löst keinen weiteren Render aus (Hash-Check, per Metrik prüfbar).
- [ ] Service-Neustart und Stream-Abbruch: kein verlorenes Publish/Unpublish (Reconcile).
- [ ] Verlust von `state.db`: Slug-URLs bleiben stabil.
- [ ] `zk`-Modus: Server-Dateien enthalten keinen Klartext (Test: grep nach Titel/Inhalt).
- [ ] Schreibversuch auf nicht erlaubte Properties wird im Adapter abgelehnt (Unit-Test).
- [ ] Objekte in nicht konfigurierten Spaces werden ignoriert, auch wenn getaggt.

**Meilensteine**

1. **M1 Spike (Risiko zuerst)**: SSE-Search-Stream mit API-Key gegen anytype-cli, Tag-Filter; Added/Updated/Removed-Events loggen. Klärt Property-Keys und Event-Format am echten System.
2. **M2 Publish/Unpublish**: Slug-Modus, Markdown-Rendering ohne Assets, URL-Rückschrieb, State, Reconcile.
3. **M3 Updates & Assets**: Debounce, Hash-Check, Bilder/Dateien, Objekt-Link-Rewrite.
4. **M4 Betrieb**: Compose, nginx-vhost, OTel, Health-Endpoints, Hooks.
5. **M5 Zero-Knowledge**: `zk`-Modus inklusive Viewer-JS.

**Risiken & offene Punkte**

- Die HTTP-API v2 ist neu (heart v0.51.x) und kann sich noch ändern → anytype-cli-Version pinnen, Zugriff in `internal/anyapi` kapseln.
- Gegen ein echtes anytype-cli zu verifizieren: Key der Tag-Property (`tag`), Format von `last_modified_by` im Stream, `object_removed` bei Archivierung, Datumsformat beim Schreiben.
- Wird ein Objekt neu veröffentlicht, aktualisieren sich Seiten, die darauf verlinken, erst bei ihrer nächsten Änderung.
- Latenz hängt am Sync: Ein mobiles Gerät muss die Änderung erst zum eigenen Node pushen; das liegt außerhalb des Service.
- Nicht unterstützte Blocktypen (Datenbank-Ansichten, Diagramm-Embeds) werden als Platzhalter gerendert.
- Bot hat Vollzugriff auf konfigurierte Spaces (siehe Sicherheit) – bewusste Entscheidung, Public-Space bleibt Option.
