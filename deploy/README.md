# Stack mit Docker Compose

`docker-compose.yml` startet drei Container:

| Service | Image | Aufgabe |
| --- | --- | --- |
| `any-sync` | `ghcr.io/grishy/any-sync-bundle` | Sync-Netz (Coordinator, Consensus, Tree, File) in einem Container, mit eingebettetem MongoDB und Redis. Startet nur mit `--profile sync`. |
| `anytype-cli` | `ghcr.io/anyproto/anytype-cli` | Headless-Client mit Bot-Account, stellt die HTTP-API auf `127.0.0.1:31012` bereit |
| `anypub` | `ghcr.io/sker65/anypub`, gebaut von `.github/workflows/image.yml` | veröffentlicht getaggte Objekte nach `PUBLIC_DIR` |

`any-sync-bundle` ist ein Community-Projekt, das die offiziellen any-sync-Knoten in ein Binary packt. Das offizielle Setup von Anytype ist [any-sync-dockercompose](https://github.com/anyproto/any-sync-dockercompose) mit rund zehn Containern.

## Bestehendes Sync-Netz

Läuft any-sync bereits, darf der Service `any-sync` nicht starten: Er würde ein zweites, neues Netz mit eigenen Schlüsseln anlegen. Deshalb gehört er zum Profil `sync` und startet ohne `--profile sync` nicht.

anytype-cli braucht dann nur die `client-config.yml` des bestehenden Netzes. Sie in ein eigenes Verzeichnis kopieren und `SYNC_CONFIG_DIR` in `.env` darauf setzen:

```sh
mkdir -p sync && cp /pfad/zum/any-sync/data/client-config.yml sync/
echo SYNC_CONFIG_DIR=./sync >> .env
```

Das ganze Datenverzeichnis des Bundles einzubinden geht auch, gibt anytype-cli aber Lesezugriff auf `bundle-config.yml` mit den privaten Schlüsseln des Netzes. Die Kopie muss erneuert werden, wenn sich die externen Adressen des Netzes ändern.

anytype-cli erreicht den Server dann über die Adressen in `client-config.yml`, also über den öffentlichen Namen. Läuft der Sync-Server auf demselben Host und ist der Name von dort nicht erreichbar, hilft ein Eintrag unter `anytype-cli` in `docker-compose.yml`: `extra_hosts: ["sync.example.com:host-gateway"]`.

Vor einem Update des Bundles auf die Version aus `docker-compose.yml` dessen Datenverzeichnis nach einem sauberen Stopp sichern. Laut Bundle-README bleibt das Konfigurationsformat innerhalb von 1.x lesbar.

nginx mit TLS läuft auf dem Host und liefert `PUBLIC_DIR` aus, siehe `nginx/`.

## Erster Start

Alle Befehle laufen in diesem Verzeichnis.

1. Konfiguration anlegen:

   ```sh
   cp .env.example .env              # SYNC_HOST, PUB_BASE_URL, PUBLIC_DIR eintragen
   cp ../anypub.example.yaml anypub.yaml
   touch anytype_api_key
   ```

   `SYNC_HOST` ist der Name, unter dem die Anytype-Apps den Server erreichen. any-sync liest den Wert nur beim ersten Start und schreibt ihn dann nach `data/any-sync/bundle-config.yml`.

2. Client starten, mit eigenem Sync-Netz zusätzlich das Profil `sync`:

   ```sh
   docker compose up -d anytype-cli                       # bestehendes Netz
   docker compose --profile sync up -d any-sync anytype-cli  # neues Netz
   ```

   Ein neues Netz erzeugt dabei `data/any-sync/client-config.yml`. Firewall: TCP 33010 und UDP 33020 müssen offen sein.

3. Bot-Account im eigenen Netz anlegen:

   ```sh
   docker compose exec anytype-cli anytype auth create anypub-bot --network-config /sync/client-config.yml
   ```

   Der Befehl gibt den Account-Key aus. Ihn sicher aufbewahren, er ist der einzige Zugang zum Bot. Im Container liegt er unter `data/anytype-config/`, beim Neustart meldet sich anytype-cli damit wieder an.

4. API-Key für anypub erzeugen und in die Secret-Datei schreiben:

   ```sh
   docker compose exec anytype-cli anytype auth apikey create anypub --all-spaces --read-write
   ```

   Den Wert hinter `Key:` ohne Zeilenumbruch nach `anytype_api_key` schreiben. anypub läuft als uid 10001, gid 33 und muss die Datei lesen können, zum Beispiel mit `chgrp 33 anytype_api_key && chmod 640 anytype_api_key`.

5. Nur bei einem neuen Netz: die Anytype-Desktop-App damit verbinden, also `data/any-sync/client-config.yml` beim Login unter „Self-hosted“ importieren. Ein Vault im eigenen Netz ist getrennt von einem Vault im Anytype-Netz. Bestehende Inhalte müssen exportiert und importiert werden.

6. In der App den Space freigeben, den Einladungslink mit der Rolle Editor erzeugen und den Bot beitreten lassen:

   ```sh
   docker compose exec anytype-cli anytype space join "<invite-link>"
   docker compose exec anytype-cli anytype space list
   ```

   Ohne Editor-Rolle kann anypub die `Public URL` nicht zurückschreiben.

7. Die Space-ID aus `space list` als `ANYPUB_SPACES` in `.env` eintragen und anypub starten. Das Image kommt von ghcr.io; `--build` baut es stattdessen aus diesem Checkout.

   ```sh
   docker compose up -d anypub
   docker compose logs -f anypub
   ```

   Im Log stehen `anypub started` und danach `stream live` pro Space. Hat im Space noch nie ein Objekt das Tag `publish` getragen, meldet anypub stattdessen `tag option does not exist yet`. Es prüft dann alle 30 s erneut. Sobald das erste Objekt getaggt ist, startet der Stream.

Prüfen: `docker compose exec anytype-cli wget -qO- 127.0.0.1:9464/readyz` gibt `ready` aus.

## Daten und Backup

| Pfad | Inhalt | Sichern |
| --- | --- | --- |
| `data/any-sync/` | Sync-Daten und `bundle-config.yml` mit den Schlüsseln des Netzes | ja, nach `docker compose stop any-sync` |
| `data/anytype/`, `data/anytype-config/` | lokale Kopie des Bots und sein Account-Key | ja |
| Volume `anypub-state` | `state.db`, lässt sich aus den `Public URL`-Werten wiederherstellen | optional |
| `PUBLIC_DIR` | erzeugte Seiten, werden beim Start neu abgeglichen | nein |

`.env`, `anypub.yaml`, `anytype_api_key` und `data/` enthalten Geheimnisse und gehören nicht in ein Repository.

## Image von anypub

`.github/workflows/image.yml` testet und baut das Image für linux/amd64 und linux/arm64:

| Auslöser | Tags in `ghcr.io/sker65/anypub` |
| --- | --- |
| Push auf `main` | `main`, `sha-<commit>` |
| Tag `v1.2.3` | `1.2.3`, `1.2`, `latest`, `sha-<commit>` |
| Pull Request | nur Build, kein Push |

Ein neues Paket auf ghcr.io ist zunächst privat. Damit der Server es ohne Login ziehen kann, unter github.com/users/sker65/packages/container/anypub/settings die Sichtbarkeit auf „Public“ stellen. Sonst auf dem Server `docker login ghcr.io` mit einem Token mit `read:packages` ausführen.

`ANYPUB_VERSION` in `.env` wählt den Tag, Standard ist `main`.

## Updates

Die Image-Versionen von any-sync und anytype-cli sind fest eingetragen. Für ein Update die Tags in `docker-compose.yml` ändern und `docker compose pull && docker compose up -d` ausführen. Vor einem Update von any-sync `data/any-sync/` sichern.
