# Office Attendance Bot

Kleine, eigenständige Slack-App zur wöchentlichen Büro-Anwesenheitsplanung.

- Zum konfigurierten Zeitpunkt (Standard: **Montag 07:00 Europe/Berlin**) erhält jede Person der Zielgruppe eine private Nachricht mit den Tagen Montag bis Freitag zum Ankreuzen und einem **Speichern**-Button.
- Antworten sind bis zur Abgabefrist möglich (Standard: **Freitag 08:00** der abgefragten Woche) und können beliebig oft geändert werden.
- Im Ergebnis-Channel gibt es **genau eine Übersichtsnachricht pro Kalenderwoche**. Sie wird bei jeder Änderung per `chat.update` aktualisiert.
- Optional gehen Erinnerungen an alle, die noch nicht geantwortet haben.
- Die Slack-Nachrichten sind auf Englisch (Standard), Deutsch, Französisch, Spanisch, Italienisch, Portugiesisch, Niederländisch, Dänisch, Schwedisch, Norwegisch (Bokmål), Polnisch, Tschechisch, Türkisch, Ukrainisch oder Japanisch, einstellbar über `BOT_LANGUAGE`. Zum Spaß gibt es jede Sprache außerdem als Nerd- und Piratenvariante: `nerd_<sprache>` und `pirate_<sprache>`, z. B. `nerd_de` oder `pirate_en`. Die in dieser Doku zitierten Texte sind die deutschen (`BOT_LANGUAGE=de`).
- Unter der Übersicht schickt der Button **📩 Abfrage an mich senden** die DM erneut an alle, die sie nicht bekommen oder nicht mehr gefunden haben. Wer nach dem Wochenstart zur Zielgruppe hinzukommt, kann sich darüber selbst eintragen.
- **Abwesenheiten**: Wer eine ganze Woche weg ist, bekommt weder Abfrage noch Erinnerungen und steht in der Übersicht unter „Abwesend“. Eintragen geht per **🌴 Ich bin diese Woche abwesend** in der DM, per Formular **🌴 Abwesenheit planen** (in DM und Übersicht) oder für Admins per CLI.

Technik: Go, Slack Socket Mode (kein öffentlicher HTTP-Endpunkt), SQLite (WAL), ein einziger Docker-Container. Externe Go-Abhängigkeiten: nur `slack-go/slack` und `modernc.org/sqlite`.

---

## Inhalt

1. [Architektur](#architektur)
2. [Slack-App einrichten](#slack-app-einrichten)
3. [Konfiguration](#konfiguration)
4. [Start mit Docker Compose](#start-mit-docker-compose)
5. [Testdurchlauf](#testdurchlauf)
6. [CLI-Befehle](#cli-befehle)
7. [Backup](#backup)
8. [Fehlerdiagnose](#fehlerdiagnose)
9. [Entwicklung](#entwicklung)
10. [Bekannte Einschränkungen](#bekannte-einschränkungen)

---

## Architektur

```text
cmd/officebot/main.go            Verdrahtung, Subcommands, Healthcheck, Graceful Shutdown
internal/config/                 Umgebungsvariablen laden und validieren
internal/isoweek/                ISO-8601-Wochen, sommerzeitsichere Terminberechnung
internal/domain/                 Gemeinsame Typen (Poll, Participant, Response, Days, Absence)
internal/slack/                  API-Interface + slack-go-Adapter + Dry-Run, Block Kit, Socket Mode, Interaktionen
internal/participants/           Zielgruppe ermitteln (Channel- oder Listenmodus, Filter)
internal/poll/                   Geschäftslogik (Wochenstart, Speichern, Frist, Erinnerungen) + Scheduler
internal/summary/                Channel-Übersicht: Rendering + zentraler Update-Worker
internal/storage/                SQLite-Repository + eingebettete Migrationen
internal/worker/                 Outbox-Worker mit Retry/Backoff
```

### Wesentliche Entscheidungen

| Entscheidung                              | Begründung                                                                                                                                                                                                                                                                                                                                                                                            |
|-------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Transaktionale Outbox** (`outbox_jobs`) | Poll, Teilnehmer-Snapshot und ein `send_dm`-Job je Person entstehen in *einer* Transaktion. Ein Neustart setzt dort fort, wo der Versand aufgehört hat. Fehlgeschlagene DMs werden einzeln wiederholt.                                                                                                                                                                                                |
| **Idempotenz über Unique-Constraints**    | `UNIQUE(workspace_id, iso_year, iso_week)` verhindert doppelte Polls. `UNIQUE(job_type, participant_id, dedupe_key)` verhindert doppelte Jobs. Jede Erinnerungsrunde (`reminder_rounds`) läuft genau einmal.                                                                                                                                                                                          |
| **Message-Metadata gegen Duplikate**      | DMs und Übersicht tragen Slack-Metadata. Stürzt der Bot ab, nachdem Slack eine Nachricht angenommen hat, aber bevor der Timestamp gespeichert ist, findet der nächste Versuch die vorhandene Nachricht und postet nicht erneut.                                                                                                                                                                       |
| **Versionierte Übersicht**                | Jede relevante Änderung erhöht `summary_version`. Ein einziger Update-Worker bündelt Änderungen 1,5 s lang, rendert **immer aus dem aktuellen DB-Stand** und setzt danach `summary_synced_version`. Ein älterer Stand kann deshalb nie einen neueren überschreiben. Solange die beiden Versionen auseinanderliegen, synchronisiert der Worker erneut: mit Backoff, alle 30 s und nach jedem Neustart. |
| **Speichern statt Auto-Save**             | Checkbox-Klicks werden nur quittiert und nie gespeichert. Erst **Speichern** liest den Zustand (`state.values`) aus dem Payload. Die `block_id` enthält die Revision, sodass Slack-Clients nach dem Update die gespeicherte Auswahl anzeigen.                                                                                                                                                         |
| **DB vor Slack**                          | Eine Antwort wird zuerst committet. Schlägt danach ein Slack-Aufruf fehl, bleibt die Antwort erhalten, und der Aufruf wird über die Outbox wiederholt.                                                                                                                                                                                                                                                |
| **Mentions ohne Massen-Ping**             | Die Übersicht wird zuerst als Platzhalter ohne Mentions gepostet und sofort per `chat.update` vervollständigt. Updates lösen keine Benachrichtigungen aus, deshalb wird die „Noch offen“-Liste nicht angepingt.                                                                                                                                                                                       |
| **Eine SQLite-Verbindung**                | `MaxOpenConns=1`, WAL, `busy_timeout=5000`, `_txlock=immediate`. Damit gibt es im Prozess kein `SQLITE_BUSY`, und gleichzeitige CLI-Zugriffe warten einfach.                                                                                                                                                                                                                                          |
| **Pure-Go SQLite**                        | Kein cgo, statisches Binary, Distroless-Image mit rund 15 MB.                                                                                                                                                                                                                                                                                                                                         |
| **Kein HTTP-Port**                        | Der Healthcheck liest eine Heartbeat-Datei, die der Daemon alle 15 s schreibt (Prozess, DB, Socket-Mode-Status).                                                                                                                                                                                                                                                                                      |

### Abläufe

1. **Scheduler**: schläft per Timer bis zum nächsten relevanten Zeitpunkt (Wochenstart, Frist, Erinnerung, höchstens 1 h). Jeder Tick ist idempotent: Er schließt fällige Polls, legt den aktuellen Poll an, falls er fehlt (auch nachgeholt nach Ausfall, solange die Frist nicht vorbei ist), plant fällige Erinnerungen ein und löscht alte Daten.
2. **Speichern**: Ack sofort (unter 3 s). Danach asynchron: Workspace prüfen, User aus dem von Slack authentifizierten Payload übernehmen, Poll-ID nur als Lookup verwenden und prüfen, ob Poll offen, Frist nicht vorbei, User im Snapshot und Nachricht = gespeicherte DM ist. Dann atomarer Upsert (Revision steigt nur bei echter Änderung), DM aktualisieren und Übersicht anstoßen.
3. **Frist**: Ab der Frist werden Speichern-Klicks abgelehnt (mit Hinweis an die Person). Die DMs werden ohne Buttons als „geschlossen“ neu gerendert, und die Übersicht zeigt „🔒 Abstimmung geschlossen“.
4. **DM erneut anfordern** (Button unter der Übersicht, nur solange die Abstimmung offen ist): Der Klick muss von der aktuellen Übersicht kommen. Alle Antworten sind ephemer, also nur für die klickende Person sichtbar.
   - **Teilnehmende:** Die alte DM wird durch einen Hinweis ohne Buttons ersetzt („durch eine neue ersetzt“). Eine neue DM mit der gespeicherten Auswahl wird über die Outbox verschickt. Ein Generationszähler je Person in den DM-Metadaten sorgt dafür, dass die Wiederherstellung nach einem Absturz nie die alte DM übernimmt. Pro Person ist höchstens eine neue DM alle 2 Minuten möglich.
   - **Nachzügler:** Wer jetzt zur Zielgruppe gehört (Mitglied des Teilnehmer-Channels oder in `SLACK_USER_IDS`) und die üblichen Filter besteht, wird in die laufende Abstimmung aufgenommen. Alle anderen bekommen einen kurzen Hinweis.
5. **Abwesenheiten** (Tabelle `absences`): ganze Kalendertage von–bis, beide inklusive, in `POLL_TIMEZONE`. Überlappende und direkt angrenzende Einträge einer Person werden zusammengeführt.
   - **Ganze Woche abwesend** (Montag bis Freitag abgedeckt): Die wöchentliche DM wird nicht verschickt (Zustellstatus `away`), es gibt keine Erinnerungen, und die Übersicht führt die Person unter „Abwesend“. Abwesende zählen nicht zur Gesamtzahl: „4 von 5 Mitarbeitern · 1 abwesend“.
   - **Teilweise abwesend**: Die DM kommt wie gewohnt. Erinnerungen an Tagen der Abwesenheit entfallen.
   - **In der DM**: **🌴 Ich bin diese Woche abwesend** trägt Montag bis Freitag der abgefragten Woche ein. Die DM zeigt dann statt der Checkboxen den Hinweis mit **Doch nicht abwesend**. Das gibt genau diese Woche zurück, auch wenn sie in einem längeren Urlaub liegt. Eine vorher gespeicherte Auswahl bleibt erhalten.
   - **Formular** (**🌴 Abwesenheit planen** in DM und Übersicht): öffnet sich nur für die klickende Person. Es nimmt einen Zeitraum auf und listet die eigenen geplanten Abwesenheiten mit **Löschen**. Der letzte Tag darf nicht in der Vergangenheit und höchstens ein Jahr in der Zukunft liegen. In Slack kann jede Person nur ihre eigenen Einträge ändern.
   - **Änderungen während einer offenen Abstimmung** wirken sofort: Übersicht und DM werden aktualisiert. Wer wegen einer Abwesenheit keine DM bekommen hat und sie löscht, bekommt die DM sofort. Wer abwesend ist und **Abfrage an mich senden** klickt (auch als Nachzügler), bekommt die DM mit **Doch nicht abwesend**.
   - **Aufbewahrung**: Abwesenheiten, die länger als `RETENTION_WEEKS` vorbei sind, werden automatisch gelöscht.

---

## Slack-App einrichten

Die vollständige Schritt-für-Schritt-Anleitung steht in **[Slack-App anlegen](slack-app-setup.md)**. Sie behandelt Manifest, Tokens, Installation, Einladen des Bots, das Finden der IDs und einen Funktionstest. Sie ist so geschrieben, dass man sie an einen Workspace-Admin weitergeben kann.

Kurzfassung:

1. App aus dem Manifest anlegen.
2. App-Level-Token mit `connections:write` erzeugen (`SLACK_APP_TOKEN`).
3. App installieren und Bot-Token kopieren (`SLACK_BOT_TOKEN`).
4. Bot per `/invite` in den Ergebnis-Channel einladen, in Modus A auch in den Teilnehmer-Channel.

### Benötigte OAuth-Scopes

| Scope                                 | Wofür                                                      |
|---------------------------------------|------------------------------------------------------------|
| `chat:write`                          | DMs, Erinnerungen und Übersicht posten und aktualisieren   |
| `im:write`                            | DM-Kanal zu einer Person öffnen                            |
| `im:history`                          | Nach Absturz prüfen, ob eine DM schon gesendet wurde       |
| `users:read`                          | Bots, App-User und deaktivierte Accounts ausfiltern        |
| `channels:read` / `groups:read`       | Modus A: Mitglieder des Teilnehmer-Channels lesen          |
| `channels:history` / `groups:history` | Nach Absturz prüfen, ob die Übersicht schon gepostet wurde |

Die `*:history`-Scopes sind nur für den Duplikatschutz nach einem Absturz nötig. Fehlen sie, funktioniert der Bot trotzdem. Er protokolliert dann eine Debug-Meldung und kann in seltenen Fällen eine Nachricht doppelt senden.

Der App-Level-Token braucht zusätzlich `connections:write`. Nach jeder Scope-Änderung muss die App neu installiert werden.

---

## Konfiguration

Alle Einstellungen kommen aus Umgebungsvariablen und werden beim Start validiert. Fehler werden gesammelt ausgegeben, und der Start bricht mit Exit-Code 2 ab. Tokens erscheinen nie in Logs.

Docker Compose lädt zwei Dateien. Werte aus der zweiten überschreiben die aus der ersten:

| Datei        | Im Git                  | Inhalt                                                                                                                        |
|--------------|-------------------------|-------------------------------------------------------------------------------------------------------------------------------|
| `.env`       | ja                      | Gemeinsame, nicht geheime Standardwerte: Zeitplan, Frist, Erinnerungen, Betrieb, Container-UID/GID                            |
| `.env.local` | **nein** (`.gitignore`) | Workspace-spezifische Werte und Geheimnisse: Tokens, Channel-IDs, Teilnehmer, lokale Overrides. Vorlage: `.env.local.example` |

```bash
cp .env.local.example .env.local   # danach Werte eintragen
```

Den vollständigen aktuellen Stand beider Dateien zum Übernehmen zeigt [dotenv.md](dotenv.md).

`.env.local` (aus `.env.local.example`):

```dotenv
# --- Slack-Zugangsdaten (Pflicht außer bei DRY_RUN=true) --------------------
SLACK_BOT_TOKEN=xoxb-...
SLACK_APP_TOKEN=xapp-...

# Channel für die Übersicht (Bot muss Mitglied sein)
SLACK_SUMMARY_CHANNEL_ID=C123456

# Zielgruppe – genau EINE der beiden Varianten setzen:
#   A) alle aktiven, menschlichen Mitglieder eines Channels
SLACK_PARTICIPANTS_CHANNEL_ID=
#   B) explizite Liste von User-IDs
SLACK_USER_IDS=U123,U456,U789
# optional: diese Personen nie befragen
SLACK_EXCLUDE_USER_IDS=

# optionale lokale Overrides von .env, z. B.
# DRY_RUN=true
# POLL_TIME=10:30
# BOT_LANGUAGE=de
```

`.env` (gemeinsame Standardwerte):

```dotenv
# --- Zeitplan ------------------------------------------------------------
POLL_WEEKDAY=MONDAY            # MONDAY = laufende Woche, jeder andere Tag = kommende Woche
POLL_TIME=07:00                # HH:MM
POLL_TIMEZONE=Europe/Berlin    # IANA-Zeitzone, Sommer-/Winterzeit wird berücksichtigt
POLL_DEADLINE_WEEKDAY=FRIDAY   # Abgabefrist, bezogen auf die abgefragte Woche
POLL_DEADLINE_TIME=08:00

# --- Erinnerungen --------------------------------------------------------
REMINDER_ENABLED=true
REMINDER_WEEKDAY=TUESDAY       # auch mehrere: TUESDAY,THURSDAY
REMINDER_TIME=09:00
REMINDER_MAX_COUNT=1           # max. Erinnerungen pro Person und Woche

# --- Betrieb -------------------------------------------------------------
DATABASE_PATH=/data/officebot.db
LOG_LEVEL=info                 # debug, info, warn, error
RETENTION_WEEKS=26             # ältere Abstimmungen werden gelöscht
SUMMARY_MAX_NAMES=40           # Namen pro Liste, danach „… und N weitere“
SUMMARY_PROJECT_LINK=true      # false = kein GitHub-Hinweis unter der Übersicht
BOT_LANGUAGE=en                # Sprache der Slack-Nachrichten (siehe Tabelle)
DRY_RUN=false                  # true = nichts an Slack senden, nur loggen

# --- Docker Compose ------------------------------------------------------
# UID/GID, mit denen der Container läuft (= Besitzer von ./volumes/officebot).
# Ermitteln mit: id -u / id -g
OFFICEBOT_UID=1000
OFFICEBOT_GID=1000
```

`OFFICEBOT_UID`/`OFFICEBOT_GID` werden in `compose.yaml` selbst eingesetzt. Compose liest solche `${…}`-Werte nur aus `.env` (oder der Shell), also bitte in `.env` ändern, nicht in `.env.local`. Niemals Tokens in `.env` eintragen: Die Datei wird committet.

| Variable                                       | Pflicht         | Standard                | Bedeutung                                                                                                                                                               |
|------------------------------------------------|-----------------|-------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SLACK_BOT_TOKEN`                              | ja¹             | –                       | Bot User OAuth Token, `xoxb-…`                                                                                                                                          |
| `SLACK_APP_TOKEN`                              | ja¹             | –                       | App-Level Token (`connections:write`), `xapp-…`                                                                                                                         |
| `SLACK_SUMMARY_CHANNEL_ID`                     | ja              | –                       | Ergebnis-Channel                                                                                                                                                        |
| `SLACK_PARTICIPANTS_CHANNEL_ID`                | eins von beiden | –                       | Modus A: Channel-Mitglieder werden befragt                                                                                                                              |
| `SLACK_USER_IDS`                               | eins von beiden | –                       | Modus B: kommagetrennte User-IDs                                                                                                                                        |
| `SLACK_EXCLUDE_USER_IDS`                       | nein            | –                       | Ausgeschlossene User-IDs                                                                                                                                                |
| `POLL_WEEKDAY` / `POLL_TIME`                   | nein            | `MONDAY` / `07:00`      | Zeitpunkt der Anfrage. `MONDAY` fragt die laufende Woche ab, jeder andere Tag die kommende (siehe unten).                                                               |
| `POLL_TIMEZONE`                                | nein            | `Europe/Berlin`         | Zeitzone für alle Zeitangaben                                                                                                                                           |
| `POLL_DEADLINE_WEEKDAY` / `POLL_DEADLINE_TIME` | nein            | `FRIDAY` / `08:00`      | Abgabefrist in der abgefragten Woche. Sie muss nach dem Anfragezeitpunkt liegen.                                                                                        |
| `REMINDER_ENABLED`                             | nein            | `true`                  | Erinnerungen an/aus                                                                                                                                                     |
| `REMINDER_WEEKDAY` / `REMINDER_TIME`           | nein            | `TUESDAY` / `09:00`     | Erinnerungszeitpunkt(e). Es zählen nur Termine zwischen Wochenstart und Frist.                                                                                          |
| `REMINDER_MAX_COUNT`                           | nein            | `1`                     | Maximale Erinnerungen pro Person und Woche                                                                                                                              |
| `DATABASE_PATH`                                | nein            | `/data/officebot.db`    | SQLite-Datei (Verzeichnis wird mit 0700 angelegt)                                                                                                                       |
| `LOG_LEVEL`                                    | nein            | `info`                  | `debug`, `info`, `warn`, `error`                                                                                                                                        |
| `RETENTION_WEEKS`                              | nein            | `26`                    | Aufbewahrung historischer Abstimmungen                                                                                                                                  |
| `SUMMARY_MAX_NAMES`                            | nein            | `40`                    | Kürzung langer Namenslisten                                                                                                                                             |
| `SUMMARY_PROJECT_LINK`                         | nein            | `true`                  | Kleiner Hinweis unter „Letzte Aktualisierung“, den Bot auf GitHub zu bewerten; `false` blendet ihn aus                                                                  |
| `DRY_RUN`                                      | nein            | `false`                 | Kein Kontakt zu Slack. Alle Nachrichten landen im Log.                                                                                                                  |
| `BOT_LANGUAGE`                                 | nein            | `en`                    | Sprache aller Slack-Nachrichten: `en`, `de`, `fr`, `es`, `it`, `pt`, `nl`, `da`, `sv`, `nb`, `pl`, `cs`, `tr`, `uk`, `ja`; zum Spaß auch `nerd_<sprache>`, `pirate_<sprache>` |
| `HEALTH_FILE`                                  | nein            | `/tmp/officebot.health` | Heartbeat-Datei für den Healthcheck                                                                                                                                     |
| `OFFICEBOT_UID` / `OFFICEBOT_GID`              | nein            | `1000` / `1000`         | Nur Compose: Benutzer, unter dem der Container läuft, und damit Besitzer der Daten in `./volumes/officebot`                                                             |

¹ Nicht nötig bei `DRY_RUN=true`.

**Zeitpunkte für Tests:** Anfrage, Frist und Erinnerungen sind frei einstellbar. Um sofort zu testen, setze `POLL_WEEKDAY` auf heute und `POLL_TIME` ein paar Minuten in die Zukunft. Außer am Montag fragt der Bot dann die kommende Woche ab. Liegt der Zeitpunkt dieser Woche schon zurück, holt der Bot die Abfrage beim Start sofort nach, sofern die Frist noch nicht vorbei ist. Alternativ startet `officebot start-week` die Woche sofort.

**Welche Woche wird abgefragt?** Die Arbeitswoche beginnt am Montag. Eine Abfrage, die am Montag startet, gilt für die laufende Woche. Startet sie an einem anderen Tag (Dienstag bis Sonntag), gilt sie für die kommende Woche, also ab dem nächsten Montag. Die Frist bezieht sich immer auf die abgefragte Woche. Beispiel für eine Abfrage am Freitag für die Folgewoche:

```dotenv
POLL_WEEKDAY=FRIDAY
POLL_TIME=12:00
POLL_DEADLINE_WEEKDAY=MONDAY   # Montag der abgefragten Woche
POLL_DEADLINE_TIME=08:00
REMINDER_WEEKDAY=MONDAY        # Erinnerungen zählen nur zwischen Start und Frist,
REMINDER_TIME=07:00            # der Standard TUESDAY käme hier zu spät
```

---

## Start mit Docker Compose

```bash
# .env.local anlegen (siehe oben) und dann:
docker compose up -d              # fertiges Image von ghcr.io (make pull)
docker compose logs -f officebot
docker compose ps                 # STATUS zeigt (healthy) nach ca. 15–60 s
```

**Fertiges Image oder selbst bauen:** `compose.yaml` verweist auf `ghcr.io/jmatthiesen81/office-attendance-bot:1`, ein Multi-Arch-Image für amd64 und arm64. Für den Betrieb genügen dann `compose.yaml`, `.env`, `.env.local` und das Verzeichnis `volumes/officebot/`, ein Klon des Repos ist nicht nötig. Der Tag `:1` bekommt Fehlerbehebungen und neue Funktionen, aber keine inkompatiblen Änderungen. Für eine feste Version `:1.2.3` eintragen, für den neuesten ungetesteten Stand `:edge` (siehe [Entwicklung](#entwicklung)). Updates: `docker compose up -d`. Wegen `pull_policy: always` lädt Compose dabei jedes Mal das neueste Image zum Tag. Mit `docker compose up -d --build` (`make up`) baut Compose stattdessen aus dem Quellcode. Das passiert auch automatisch, wenn sich das Image nicht laden lässt. `docker compose exec officebot /officebot version` zeigt die laufende Version.

Eigenschaften des Containers:

- Multi-Stage-Build, Laufzeit-Image `distroless/static:nonroot` (ca. 15 MB). Unter Compose läuft er mit `OFFICEBOT_UID:OFFICEBOT_GID` (Standard 1000:1000), nie als root.
- Root-Dateisystem read-only, `/tmp` als tmpfs, alle Capabilities entzogen, `no-new-privileges`.
- **Keine Docker-Volumes:** Alle Daten liegen sichtbar im Projektverzeichnis unter `./volumes/officebot/` (Bind-Mount nach `/data`). Dort liegen `officebot.db` mit `-wal`/`-shm` (Datei 0600). Das Image deklariert bewusst kein `VOLUME`, damit Docker keine anonymen Volumes anlegt. Das Verzeichnis ist mit `.gitkeep` im Repo, sein Inhalt ist per `.gitignore` ausgeschlossen. Empfehlung: `chmod 700 volumes/officebot`.
- `restart: unless-stopped`. `SIGTERM` führt zu einem Graceful Shutdown: Laufende Speichervorgänge und Jobs werden noch beendet (`stop_grace_period: 30s`).
- Healthcheck `officebot healthcheck`. Er meldet „unhealthy“, wenn der Heartbeat älter als 60 s ist, die DB nicht erreichbar ist oder Socket Mode länger als 2 min getrennt ist. Ist Socket Mode länger als 15 min getrennt, beendet sich der Bot selbst, und die Restart-Policy startet ihn neu.

---

## Testdurchlauf

### Lokal ohne Slack (Dry-Run)

```bash
docker build -t officebot .
mkdir -p volumes/officebot-dry
docker run -d --name officebot-dry -u "$(id -u):$(id -g)" -v "$PWD/volumes/officebot-dry:/data" \
  -e DRY_RUN=true -e SLACK_SUMMARY_CHANNEL_ID=CSUM -e SLACK_USER_IDS=U1,U2,U3 officebot

docker logs -f officebot-dry      # zeigt Poll-Start, alle DMs und die Übersicht als Text

E="-e DRY_RUN=true -e SLACK_SUMMARY_CHANNEL_ID=CSUM -e SLACK_USER_IDS=U1,U2,U3"
docker exec $E officebot-dry /officebot simulate-save --user U1 --days mon,wed
docker exec $E officebot-dry /officebot simulate-save --user U2 --days ""   # „kein Bürotag“
docker exec $E officebot-dry /officebot simulate-request --user U3          # Klick auf „Abfrage an mich senden“
docker exec $E officebot-dry /officebot status
docker restart officebot-dry      # erzeugt keine neue Abstimmung, keine neuen DMs
```

### Mit echtem Slack

1. Für den ersten Test `SLACK_USER_IDS` auf die eigene User-ID setzen und einen Test-Channel als `SLACK_SUMMARY_CHANNEL_ID` verwenden.
2. `docker compose up -d --build`. Liegt der konfigurierte Zeitpunkt dieser Woche schon zurück, kommt die DM sofort. Sonst `docker compose exec officebot /officebot start-week` ausführen.
3. In der DM Tage ankreuzen und **Speichern** klicken. Die DM zeigt „✅ Gespeichert“, und die Übersicht im Channel wird nach ca. 1–2 s aktualisiert.
4. Auswahl ändern und erneut speichern: Es kommt keine neue Channel-Nachricht, nur ein Update.
5. Frist testen: `POLL_DEADLINE_WEEKDAY`/`POLL_DEADLINE_TIME` kurz in die Zukunft legen und neu starten. Nach Ablauf zeigt die DM „🔒 geschlossen“.
6. Woche zurücksetzen und erneut testen: `docker compose exec officebot /officebot delete-week --week 2026-W42 --yes`. Das löscht nur die Datenbankeinträge, die alten Slack-Nachrichten bleiben stehen.

---

## CLI-Befehle

Alle Befehle laufen im Container mit derselben Konfiguration: `docker compose exec officebot /officebot <befehl>`. Sie schreiben nur in die Datenbank. Den Versand übernimmt der laufende Bot innerhalb von 30 s. So spricht immer nur ein Prozess mit Slack, und es entstehen keine Doppelnachrichten.

| Befehl                                             | Wirkung                                                                                   |
|----------------------------------------------------|-------------------------------------------------------------------------------------------|
| `run`                                              | Bot starten (Standard)                                                                    |
| `start-week [--week 2026-W43]`                     | Abstimmung jetzt starten. Existiert sie schon, passiert nichts.                           |
| `resync [--week 2026-W43]`                         | Übersicht neu schreiben (ohne `--week`: alle offenen)                                     |
| `status`                                           | Offene Abstimmungen, Antworten, Zustellstatus, Outbox                                     |
| `list-users [--channel C…] [--all]`                | Benutzer mit IDs auflisten und eine fertige Zeile `SLACK_USER_IDS=` ausgeben (nur lesend) |
| `delete-week --week W --yes`                       | Abstimmung einer Woche aus der DB löschen (für Tests)                                     |
| `simulate-save --user U --days mon,wed [--week W]` | Nur bei `DRY_RUN=true`: Antwort simulieren                                                |
| `simulate-request --user U [--week W]`             | Nur bei `DRY_RUN=true`: Klick auf „Abfrage an mich senden“ simulieren                     |
| `away list [--user U]`                             | Kommende Abwesenheiten anzeigen (mit ID)                                                  |
| `away add --user U --from D --to D`                | Abwesenheit eintragen (`YYYY-MM-DD`, beide Tage inklusive)                                |
| `away remove --user U --from D --to D`             | Tage aus Abwesenheiten entfernen (kürzt oder teilt Einträge)                              |
| `away remove --id N`                               | Einen Eintrag löschen                                                                     |
| `backup <datei>`                                   | Konsistente Kopie der DB (`VACUUM INTO`), auch im laufenden Betrieb                       |
| `migrate`                                          | Migrationen anwenden und beenden                                                          |
| `healthcheck`                                      | Healthcheck-Probe                                                                         |
| `version`                                          | Version ausgeben (`dev` bei lokalen Builds ohne Version)                                  |

---

## Backup

Die Datenbank ist die einzige Quelle der Wahrheit. Der Zustand lässt sich vollständig ohne Slack-Nachrichten rekonstruieren, mit `resync` werden alle Übersichten neu geschrieben.

```bash
make backup
# entspricht:
docker compose exec officebot /officebot backup /data/officebot-20261013-1000.db
mv volumes/officebot/officebot-20261013-1000.db backup/
```

`VACUUM INTO` erzeugt auch im laufenden Betrieb (WAL) eine konsistente Kopie. Die Datei erscheint direkt in `./volumes/officebot/`. Ein einfaches Kopieren von `officebot.db` ohne `-wal`/`-shm` ist im laufenden Betrieb **nicht** sicher. Bei gestopptem Container (`docker compose stop`) kann `volumes/officebot/` dagegen einfach komplett kopiert werden.

**Wiederherstellen:** `docker compose stop`, die Sicherung als `volumes/officebot/officebot.db` ablegen, alte `officebot.db-wal`/`-shm` daneben entfernen, `docker compose start` und `resync` ausführen.

---

## Fehlerdiagnose

| Symptom / Log                                            | Ursache und Lösung                                                                                                                            |
|----------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------|
| `invalid configuration: …`                               | Variablen prüfen. Alle Fehler werden auf einmal aufgelistet.                                                                                  |
| `slack auth.test … invalid_auth`                         | `SLACK_BOT_TOKEN` falsch oder App nicht installiert                                                                                           |
| `socket mode: invalid app token`                         | `SLACK_APP_TOKEN` falsch oder ohne `connections:write`                                                                                        |
| `missing_scope`                                          | Scope ergänzen (siehe oben) und **App neu installieren**                                                                                      |
| `not_in_channel` / `channel_not_found` bei der Übersicht | Bot in den Ergebnis-Channel einladen und Channel-ID prüfen                                                                                    |
| `list members of C…` schlägt fehl                        | Modus A: Bot muss Mitglied des Teilnehmer-Channels sein. Bei privatem Channel wird `groups:read` gebraucht.                                   |
| `no participants found`                                  | Zielgruppe leer (nur Bots oder Deaktivierte?). Der Bot versucht es jede Minute erneut.                                                        |
| Klick auf Speichern ohne Wirkung                         | *Interactivity* aktiviert? Socket verbunden (`docker compose ps` → healthy)? Logs mit `LOG_LEVEL=debug` prüfen.                               |
| `poll DM cannot be delivered … unreachable`              | Person deaktiviert oder nicht erreichbar. Sie erscheint in der Übersicht unter „⚠️ nicht zustellbar“.                                         |
| `job failed, retrying`                                   | Vorübergehender Fehler (Netz, 429). Der Bot wiederholt mit exponentiellem Backoff (5 s … 30 min, max. 8 Versuche) und beachtet `Retry-After`. |
| Übersicht wurde gelöscht                                 | Wird beim nächsten Sync automatisch neu gepostet (oder sofort mit `resync`)                                                                   |
| Keine Abstimmung nach Ausfall                            | Die Abfrage wird nur nachgeholt, solange die Frist der Woche nicht vorbei ist. Sonst `start-week --week …` für eine spätere Woche verwenden.  |

Nützlich: `docker compose exec officebot /officebot status` zeigt pro Person Zustellstatus, Erinnerungen und Antwort sowie die Outbox-Statistik.

---

## Entwicklung

Go muss lokal nicht installiert sein. Tests und Linting laufen im offiziellen Image:

```bash
make test     # go test -race ./...
make vet      # gofmt + go vet
```

Die Tests decken ab: ISO-Wochen und Jahreswechsel (inkl. 53-Wochen-Jahre und DST), Abwesenheiten (Zusammenführen, Kürzen und Aufteilen, keine DM und keine Erinnerung, Formular-Validierung, nur eigene Einträge löschbar), Konfigurationsvalidierung, Teilnehmerfilter, Mehrfach- und leere Auswahl, erneute und doppelte Antworten, Manipulationsschutz der Interaktionen, Frist, nachgeholter und idempotenter Wochenstart, parallele Änderungen (Race Detector), Outbox-Retry mit Backoff und `Retry-After`, Wiederherstellung nach verlorener Slack-Antwort, gelöschte Übersicht, Erinnerungen sowie Formatierung inkl. großer Teams.

Übersetzungen: Alle Texte, die der Bot an Slack schickt, liegen in `internal/i18n/locales/<sprache>.json`. Datums- und Zeitformate stehen dort als Go-Layouts (`layout.*`). Für eine neue Sprache die Datei `en.json` kopieren und übersetzen; der Dateiname ist der Wert für `BOT_LANGUAGE`. Für Nerd- und Piratenvariante kommen `nerd_<sprache>.json` und `pirate_<sprache>.json` dazu. Go gibt Monats- und Wochentagsnamen in Layouts immer englisch aus, deshalb nutzen alle nicht englischen Sprachen numerische Datumsformate (Nerd-Varianten: ISO 8601). Tests prüfen, dass jede Sprache alle Schlüssel mit denselben Platzhaltern enthält und keine englischen Namen in ihren Layouts verwendet.

Architekturentscheidungen liegen unter [adr/](adr/), z. B. [ADR 0001: Konfigurierbare Arbeitstage](adr/0001-konfigurierbare-arbeitstage.md) und [ADR 0002: Sprache pro Person](adr/0002-sprache-pro-person.md).

Releases: Der Workflow `.github/workflows/release.yml` führt bei Pull Requests gofmt, `go vet`, die Tests und einen Test-Build aus, ohne etwas zu veröffentlichen. Jeder Push auf einen Versionsbranch wie `1.0` veröffentlicht nach erfolgreichen Tests den aktuellen Stand als `edge` und `sha-<commit>`. Das ist zum Ausprobieren auf einem Testserver gedacht, nicht für den Produktivbetrieb. `officebot version` zeigt dort `edge-<commit>`. Ein Git-Tag `v*` veröffentlicht ein Release:

```bash
git tag v1.0.0 && git push origin v1.0.0   # → Tags 1.0.0, 1.0, 1 und latest
```

Vorabversionen wie `v1.1.0-rc.1` bekommen nur ihren eigenen Tag, `0.x`-Versionen keinen Major-Tag. Das Image enthält SBOM und Build-Provenance. Nach dem ersten Release ist das Paket auf GitHub privat. Es muss einmalig unter *Package settings* → *Change visibility* auf **Public** gestellt werden.

Neue Migrationen: Datei mit der nächsten freien Nummer anlegen, z. B. `internal/storage/migrations/0004_….sql`. Sie wird beim Start einmalig in einer Transaktion angewendet.

---

## Bekannte Einschränkungen

- **Eine Instanz**: Scheduler, Outbox und Update-Worker gehen davon aus, dass genau ein Container läuft (keine verteilten Locks).
- **Datenschutz bei Abwesenheiten**: Der Bot speichert Urlaubs- und Abwesenheitszeiträume mit Slack-User-ID in der Datenbank. Wer eine ganze Woche abwesend ist, steht für alle sichtbar im Übersichts-Channel unter „Abwesend“. Einen Grund fragt der Bot nie ab. Bitte das Team vorab informieren und gegebenenfalls intern (Betriebsrat, Datenschutz) abstimmen.
- **Abwesenheiten bei geschlossenen Wochen**: Spätere Änderungen an Abwesenheiten aktualisieren nur offene Abstimmungen. Geschlossene Übersichten bleiben, wie sie sind.
- **Ein Workspace**: Interaktionen aus anderen Workspaces werden verworfen. Enterprise-Grid-Installationen auf Org-Ebene werden nicht unterstützt.
- **Nicht gespeicherte Häkchen**: Kreuzt jemand Tage an, ohne zu speichern, zeigt Slack diese Häkchen lokal weiter an. Verbindlich ist nur die Zeile „✅ Gespeichert: …“ unter den Checkboxen.
- **Teilnehmer-Snapshot**: Wer nach dem Wochenstart dem Teilnehmer-Channel beitritt, wird nicht automatisch befragt. Über den Button unter der Übersicht kann sich die Person selbst in die laufende Woche eintragen, sonst wird sie ab der nächsten Woche befragt. Wer austritt, bleibt in der laufenden Woche gelistet.
- **Erinnerungen**: Stürzt der Bot genau zwischen dem Senden einer Erinnerung und dem Speichern des Zählers ab, kann eine Erinnerung doppelt ankommen. Bei Poll-DMs und Übersicht verhindert der Metadata-Abgleich solche Duplikate.
- **Verlorene DMs** werden nicht automatisch neu gesendet, aber jede Person kann über den Button unter der Übersicht eine neue anfordern. Gelöschte Übersichten postet der Bot automatisch neu.
- **Lange Listen** werden je Abschnitt nach `SUMMARY_MAX_NAMES` Namen gekürzt („… und N weitere“). Die Zahlen pro Tag bleiben vollständig.
- „Letzte Aktualisierung“ ändert sich nur, wenn sich der Inhalt ändert.
