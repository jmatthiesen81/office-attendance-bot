# ADR 0003: Weboberfläche für Verwaltung und Auswertungen

- **Status:** Angenommen, umgesetzt am 2026-11-05
- **Datum:** 2026-11-05

## Kontext

Der Bot wird heute vollständig über Slack und die CLI bedient. Verwaltungsaufgaben wie Abwesenheiten (`away`), Sprachen (`lang`), Korrekturen nach der Frist (`attendance`), `refresh`, `resync` und `status` gibt es nur auf der Konsole des Servers. Wer keinen Zugang zum Server hat, kann sie nicht erledigen.

Außerdem soll der Bot die Frage beantworten können, **ob und in welchem Umfang Shared Desks reichen**. Dafür braucht es Zahlen, die auch vor dem Team bestehen:

- wie viele Personen pro Tag im Office sind, mit Spitzen und Verteilung,
- wie viele Office-Tage jede Person pro Monat hat,
- wie lange Kolleginnen und Kollegen jeweils nicht im Office waren.

Ausgangslage im Code:

- Der Bot läuft im **Socket Mode** (`internal/slack/socket.go`). Er hat keinen HTTP-Port und keinen öffentlichen Endpunkt.
- Alle CLI-Aktionen rufen Service-Methoden auf (`cmd/officebot/main.go`, `internal/poll`). Eine Weboberfläche kann dieselben Methoden nutzen.
- Die Rohdaten liegen in SQLite: `polls`, `participants`, `responses` (fünf Tagesspalten pro Person und Woche) und `absences`.
- `RETENTION_WEEKS` (Standard 26) löscht alte Wochen samt Antworten (`Service.Cleanup`). Mit den heutigen Tabellen reicht eine Auswertung also höchstens ein halbes Jahr zurück.
- Die Antworten sind zunächst **geplante** Anwesenheit, abgegeben bis zur Frist. Admins gleichen sie schon heute über `attendance` (siehe Commit `9200ec5`) mit der tatsächlichen Anwesenheit ab, auch für vergangene Tage. Bisher hält der Bot aber nicht fest, welche Tage so geprüft wurden. Geprüfte und ungeprüfte Zahlen lassen sich deshalb nicht unterscheiden.

## Entscheidung

Der Bot bekommt eine optionale Weboberfläche im selben Prozess. Die Anmeldung läuft über **„Sign in with Slack“ (OpenID Connect)**. Wer sich anmelden darf, wird zuerst über die CLI und danach auch in der Weboberfläche festgelegt. Für belastbare Auswertungen schreibt der Bot eine eigene Tabelle mit dem Status jeder Person an jedem Arbeitstag. Diese Tabelle hat eine eigene, längere Aufbewahrungsfrist.

### 1. Weboberfläche

- Neues Paket `internal/web`, gebaut nur mit der Standardbibliothek: `net/http`, `html/template` und `embed` für Templates und CSS. Es gibt keine neuen Abhängigkeiten und kein CDN.
- Der Server startet nur, wenn `WEB_LISTEN_ADDR` gesetzt ist. Ohne die Variable verhält sich der Bot genau wie heute.
- Er läuft als weitere Goroutine im `run`-Loop und fährt mit dem Rest des Bots sauber herunter (`http.Server.Shutdown`).
- TLS endet am Reverse Proxy (Abschnitt 9). Der Port des Bots ist nur im Compose-Netz erreichbar, nicht auf dem Host.
- Diagramme werden serverseitig als SVG gerendert. Dafür ist kein JavaScript nötig, und die CSP kann `default-src 'self'` sein.
- Die Oberfläche ist auf Deutsch und Englisch verfügbar. Sie nutzt den bestehenden `i18n.Translator` und die Sprache, die die Person im Bot gewählt hat ([ADR 0002](0002-sprache-pro-person.md)).

### 2. Anmeldung mit Slack (OpenID Connect)

1. `GET /login` zeigt „Mit Slack anmelden“.
2. `GET /auth/start` erzeugt `state` und `nonce`. Beide sind zufällig, nur einmal verwendbar und 10 Minuten gültig. In `web_login_states` steht nur ihr Hash. Danach leitet die Seite weiter zu `https://slack.com/openid/connect/authorize`, mit diesen Parametern:
   - `response_type=code`
   - `scope=openid profile`
   - `client_id`
   - `redirect_uri={WEB_BASE_URL}/auth/callback`
   - `state`
   - `nonce`
   - `team` (die Team-ID des Bots)
3. `GET /auth/callback`:
   - prüft `state` und markiert ihn als verbraucht,
   - tauscht `code` serverseitig bei `https://slack.com/api/openid.connect.token` ein,
   - prüft im ID-Token `iss = https://slack.com`, `aud = SLACK_CLIENT_ID`, `exp` und `nonce`,
   - prüft, dass `https://slack.com/team_id` dem Workspace des Bots entspricht.

   Die Signatur des ID-Tokens wird nicht geprüft. Nach OIDC Core §3.1.3.7 ist das zulässig, weil das Token direkt vom Token-Endpunkt über TLS kommt.
4. **Autorisierung:** `https://slack.com/user_id` muss in `web_users` stehen. Wenn nicht, erscheint eine 403-Seite „Kein Zugang“, und es wird keine Session angelegt.
5. **Session:**
   - Die Session-ID ist ein zufälliger 256-Bit-Wert. In `web_sessions` liegt nur ihr SHA-256-Hash.
   - Cookie `__Host-officebot_session` mit `Secure`, `HttpOnly`, `SameSite=Lax` und `Path=/`.
   - Die Session läuft nach `WEB_SESSION_TTL` ab (Standard 12 h), oder früher nach `WEB_SESSION_IDLE` ohne Aktivität (Standard 2 h).
6. **Bei jeder Anfrage** werden Session und `web_users` neu gelesen. Rollenwechsel und Entzug gelten sofort. Beim Entzug werden zusätzlich alle Sessions der Person gelöscht.
7. **Abmelden** per `POST /logout`.

Weitere Schutzmaßnahmen:

- CSRF-Token pro Session für jeden `POST`, zusätzlich eine Prüfung des `Origin`-Headers.
- Header `Content-Security-Policy` (`default-src 'self'; frame-ancestors 'none'`), `Referrer-Policy: same-origin` und `X-Content-Type-Options: nosniff`.
- Eine einfache Begrenzung der Anfragen auf `/auth/*`.

Änderungen an der Slack-App (in `slack-app-setup.md` dokumentiert):

- „Sign in with Slack“ aktivieren,
- Redirect-URL `{WEB_BASE_URL}/auth/callback` eintragen,
- User-Scopes `openid` und `profile` hinzufügen.

### 3. Wer sich anmelden darf

Es gibt zwei Rollen:

| Rolle    | Darf                                                                                       |
| -------- | ------------------------------------------------------------------------------------------ |
| `admin`  | alles: Verwaltung, Korrekturen, Zugriffsverwaltung, Auswertungen pro Person (wenn erlaubt) |
| `viewer` | nur die zusammengefassten Auswertungen                                                     |

**Erster Zugang über die CLI.** Ohne mindestens einen Eintrag kann sich niemand anmelden.

```sh
officebot web-user list
officebot web-user add --user U123 [--role admin|viewer]   # Standard: admin
officebot web-user set-role --user U123 --role viewer
officebot web-user remove --user U123
```

**Danach auch in der Weboberfläche** unter `/access` (nur für Admins): Personen aus dem Workspace-Verzeichnis hinzufügen (`participants.Directory`), Rolle ändern, Personen entfernen.

Für CLI und Web gelten dieselben Regeln. Sie liegen zentral im Storage-Layer:

- Der letzte Admin kann weder entfernt noch herabgestuft werden.
- Jede Änderung landet in `audit_log`. Als Urheber steht dort die Slack-User-ID oder `cli`.

### 4. Verwaltungsseiten

| Route                | Rolle         | Inhalt                                                                                                                    |
| -------------------- | ------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `/`                  | admin, viewer | aktuelle Woche: wer geantwortet hat, Zustellstatus, Outbox (wie `status`)                                                 |
| `/weeks/{2026-W45}`  | admin         | Antworten der Woche, Antwort korrigieren (wahlweise mit Hinweis im Thread), `refresh`, `resync`                           |
| `/days/{2026-11-05}` | admin         | Tagesprüfung: alle Befragten des Tages mit Status, Status direkt umstellen, Tag als geprüft markieren (siehe Abschnitt 5) |
| `/absences`          | admin         | Abwesenheiten anzeigen, anlegen, entfernen (wie `away`)                                                                   |
| `/languages`         | admin         | Sprache pro Person setzen oder zurücksetzen (wie `lang`)                                                                  |
| `/closed-days`       | admin         | Tage pflegen, an denen das Office geschlossen ist (siehe Abschnitt 5)                                                     |
| `/access`            | admin         | Zugriffsverwaltung                                                                                                        |
| `/stats`             | admin, viewer | Auswertungen (siehe Abschnitt 6)                                                                                          |
| `/retention`         | admin         | Löschfristen anzeigen, Daten pro Person nach Jahr, Stichtag oder Person löschen (siehe Abschnitt 5)                       |
| `/audit`             | admin         | Audit-Log                                                                                                                 |

Alle Aktionen nutzen dieselben Service-Methoden wie die CLI. Damit gelten dieselben Regeln, etwa dass Abwesenheiten nicht in der Vergangenheit liegen dürfen. Außerdem laufen Änderungen über die Outbox und den Update-Worker, also mit DM-Update, aktualisierter Übersicht und Hinweis im Thread.

### 5. Datengrundlage für belastbare Zahlen

#### Tagesstatus pro Person

Neue Tabelle `attendance_days`, eine Zeile pro Person und Arbeitstag, an dem die Person befragt wurde:

| Spalte          | Bedeutung                                                |
| --------------- | -------------------------------------------------------- |
| `workspace_id`  | Workspace                                                |
| `slack_user_id` | Person                                                   |
| `day`           | Datum `YYYY-MM-DD` in `POLL_TIMEZONE`                    |
| `status`        | `office`, `remote`, `away` oder `no_answer`              |
| `iso_week`      | Woche der Abfrage, z. B. `2026-W45`, für Neuberechnungen |
| `updated_at`    | letzte Neuberechnung                                     |

Der Status wird so bestimmt, in dieser Reihenfolge:

1. **`office`:** Der Tag ist in der Antwort angekreuzt.
2. **`away`:** Eine Abwesenheit (`absences`) deckt den Tag ab.
3. **`remote`:** Die Person hat geantwortet, den Tag aber nicht angekreuzt.
4. **`no_answer`:** Die Person wurde befragt, hat aber nicht geantwortet.

Wer in einer Woche nicht befragt wurde, etwa weil die Person noch nicht im Team war, bekommt keine Zeilen. Solche Tage zählen weder für noch gegen die Person.

**Wann die Tabelle geschrieben wird:**

- beim Schließen einer Woche zur Frist,
- bei jeder späteren Korrektur einer geschlossenen Woche (`attendance` in CLI oder Web, `away remove --id`). Die betroffene Woche wird dann neu berechnet.

Offene Wochen erscheinen in den Auswertungen nicht. Sie gelten erst nach der Frist als abgeschlossen.

**Nachträglich befüllen:** Die Migration berechnet die Tabelle einmalig aus allen noch vorhandenen geschlossenen Wochen. Damit gibt es ab dem ersten Tag eine Historie von bis zu `RETENTION_WEEKS`.

#### Geprüfte Tage

Admins gleichen die geplante Anwesenheit mit der tatsächlichen ab. Das passiert heute über `attendance` und künftig über die Tagesprüfung `/days/{datum}`. Damit dieser Abgleich in den Zahlen sichtbar wird, hält der Bot fest, welche Tage geprüft sind:

- Neue Tabelle `verified_days(workspace_id, day, verified_by, verified_at)`. Ein Tag gilt als geprüft, sobald ein Admin ihn in der Tagesprüfung bestätigt. Das geht auch, wenn nichts korrigiert werden musste. Über die CLI geht das mit `officebot attendance verify --date 2026-11-05`.
- Ein geprüfter Tag bedeutet: Für **alle** Befragten dieses Tages entspricht der Status der tatsächlichen Anwesenheit.
- Ändert sich danach etwas an diesem Tag, etwa durch eine Antwort oder eine Abwesenheit, bleibt der Tag geprüft, und die Änderung landet im Audit-Log. Die Personen selbst können nach der Frist ohnehin nichts mehr ändern, nur Admins.
- `attendance_days` bekommt die Spalte `corrected`. Sie zeigt an, dass ein Admin den Status gegenüber der ursprünglichen Antwort geändert hat. Daraus ergibt sich, wie oft Planung und tatsächliche Anwesenheit voneinander abweichen.
- Tage, an denen kein Admin im Office war, bleiben ungeprüft. Die Auswertungen zeigen offen an, welcher Anteil der Tage geprüft ist.

#### Geschlossene Tage

Neue Tabelle `office_closed_days(workspace_id, day, reason)`. Admins pflegen dort Feiertage, Betriebsurlaub und Schließtage. Diese Tage fließen in keine Quote und in keinen Nenner ein. Ohne diese Tabelle sähe zum Beispiel der 3. Oktober wie ein Tag aus, an dem alle remote waren. Eine automatische Feiertagsliste gehört nicht zu diesem ADR.

#### Aufbewahrung

Für Zahlen ohne Personenbezug und für Daten pro Person gelten unterschiedliche Regeln.

**Zahlen ohne Personenbezug bleiben unbegrenzt erhalten.** Die Tabelle `monthly_stats` enthält pro Monat:

- Arbeitstage, geschlossene Tage, Personentage im Office,
- Durchschnitt, Median, P90, P95 und Maximum der Belegung, auch nach Wochentag,
- Anteil geprüfter Tage, Anteil korrigierter Einträge, Antwortquote,
- die **anonymen Verteilungen**: Anzahl Personen je Stufe der Office-Tage im Monat (0, 1–2, 3–5, 6–8, 9–12, mehr als 12) und je Stufe des längsten Zeitraums ohne Office (bis 1 Woche, bis 2, bis 4, bis 8, mehr als 8 Wochen).

Der Bot schreibt `monthly_stats` beim Monatswechsel. Er schreibt sie für betroffene Monate neu, wenn sich dort etwas ändert, und in jedem Fall unmittelbar vor dem Löschen von Daten pro Person. Damit bleiben alle Auswertungen ohne Namen für alte Monate vollständig, auch wenn die Tagesdaten dahinter gelöscht sind. Für Monate ohne Tagesdaten greifen die Auswertungen auf `monthly_stats` zurück. Die Oberfläche kennzeichnet solche Monate, weil sich dort der Filter „nur geprüfte Tage“ nicht mehr anwenden lässt.

**Daten pro Person** (`attendance_days`) werden auf zwei Wegen gelöscht:

1. **Automatisch** nach `STATS_RETENTION_MONTHS` (Standard 24) durch den bestehenden Cleanup-Job. Mit `0` ist die automatische Löschung aus. Dann wird nur noch manuell gelöscht.
2. **Manuell** über die CLI und die Weboberfläche (`/retention`, nur für Admins):

   ```sh
   officebot stats purge --year 2024 [--yes]            # ein Kalenderjahr löschen
   officebot stats purge --before 2025-01-01 [--yes]    # alles vor einem Stichtag löschen
   officebot stats purge --user U123 [--yes]            # alle Tagesdaten einer Person löschen
   ```

   - Ohne `--yes` zeigt der Befehl nur an, was gelöscht würde: Zeitraum, Anzahl der Zeilen und betroffene Personen.
   - Vor dem Löschen schreibt er `monthly_stats` für alle betroffenen Monate. Wird nur eine Person gelöscht, bleiben die Monatswerte so, wie sie mit ihr berechnet wurden. Sie werden danach nicht mehr neu berechnet.
   - `--user` ist für Austritte und Löschanfragen nach Art. 17 DSGVO gedacht. Antworten und Abwesenheiten in den Wochen, die noch innerhalb von `RETENTION_WEEKS` liegen, löscht der Befehl dabei mit. Sonst würden die Tagesdaten beim nächsten Neuberechnen wieder entstehen.
   - Jeder Aufruf landet im Audit-Log, mit Zeitraum oder Person und der Anzahl gelöschter Zeilen.

`RETENTION_WEEKS` gilt weiter für Abfragen, Antworten, Abwesenheiten und Slack-Bezüge. Die Auswertungen hängen nach dem Befüllen nicht mehr davon ab. Ein hoher Wert wie 106 lohnt sich vor allem einmalig: Er bringt beim Einführen bis zu zwei Jahre Historie in `attendance_days`. Danach kann man ihn wieder senken.

#### Was die Zahlen aussagen

Die Oberfläche nennt bei jeder Auswertung diese Grenzen ausdrücklich:

- **Geprüft:** Zu jedem Zeitraum wird der Anteil der geprüften Tage angezeigt. Ungeprüfte Tage geben die geplante Anwesenheit wieder. Ein Filter „nur geprüfte Tage“ beschränkt alle Auswertungen auf tatsächliche Anwesenheit.
- **Abweichung von der Planung:** Der Anteil korrigierter Einträge an geprüften Tagen zeigt, wie verlässlich die ungeprüften Tage voraussichtlich sind.
- **Abdeckung:** Zu jedem Zeitraum wird die Antwortquote angezeigt, also der Anteil der Tage mit einem anderen Status als `no_answer`. Liegt sie unter 90 %, steht ein Warnhinweis an den Zahlen.
- Jede Kennzahl hat eine kurze Definition, die per Klick sichtbar ist (Abschnitt „Methodik“).

### 6. Auswertungen

Der Zeitraum ist frei wählbar, zum Beispiel ein Monat, ein Quartal oder die letzten 12 Monate.

#### Office-Belegung (aggregiert, für `viewer` und `admin`)

- **Personen im Office pro Tag:** Durchschnitt, Median, P90, P95 und Maximum, dazu ein Verlauf über alle Tage.
- **Nach Wochentag:** durchschnittliche Belegung pro Wochentag. Das zeigt, ob sich Tage wie der Dienstag ballen.
- **Ausreichende Plätze:** eine Kurve, die zeigt, an wie viel Prozent der Tage *n* Plätze gereicht hätten. Wenn `OFFICE_DESKS` gesetzt ist, zeigt die Auswertung den heutigen Bestand im Vergleich, zum Beispiel: „14 Plätze vorhanden, 9 hätten an 95 % der Tage gereicht“.
- **Pro Monat:** Arbeitstage, Personentage im Office, durchschnittliche Belegung, Spitze und Antwortquote.
- **Verteilung der Office-Tage pro Person und Monat, ohne Namen:** zum Beispiel „7 von 20 Personen waren an höchstens 2 Tagen im Office, 3 an mehr als 12“. Das ist das zentrale Argument für Shared Desks, und dafür muss niemand namentlich genannt werden.
- **Dauer ohne Office, ohne Namen:** Verteilung der längsten Zeiträume ohne Office-Tag pro Person, zum Beispiel „5 Personen waren mehr als 4 Wochen am Stück nicht im Office“.

#### Pro Person (nur `admin`, nur mit `WEB_STATS_PER_PERSON=true`)

Eine Tabelle mit einer Zeile pro Person:

- **Office-Tage pro Monat**, als Anzahl und als Quote bezogen auf die Arbeitstage, an denen die Person befragt wurde und nicht abwesend war,
- Tage `remote`, `away` und `no_answer` pro Monat,
- **letzter Office-Tag** und **aktuelle Zeit ohne Office** in Arbeitstagen und Kalenderwochen,
- **längster Zeitraum ohne Office** im gewählten Zeitraum, mit Start- und Enddatum.

So wird ein **Zeitraum ohne Office** berechnet:

- Er ist die längste Folge aufeinanderfolgender Arbeitstage ohne `office`.
- Geschlossene Tage und Tage, an denen die Person nicht befragt wurde, unterbrechen die Folge nicht. Sie zählen aber auch nicht mit.
- Die Folge wird aufgeschlüsselt in `remote`, `away` und `no_answer`. Damit erscheint ein dreiwöchiger Urlaub nicht als drei Wochen Homeoffice. Enthält sie Tage mit `no_answer`, steht das als „davon X Tage ohne Antwort“ dabei.

#### Export

- `/stats.csv` exportiert die aggregierten Zahlen ohne Namen.
- Den Export pro Person gibt es nur für Admins mit `WEB_STATS_PER_PERSON=true`. Jeder solche Export wird im Audit-Log festgehalten.

### 7. Datenbank (Migration `0007_web_and_stats.sql`)

- `web_users(workspace_id, slack_user_id, role, display_name, added_by, added_at, last_login_at)`, Primärschlüssel `(workspace_id, slack_user_id)`, `role` mit Prüfung auf `admin` oder `viewer`
- `web_sessions(id_hash PRIMARY KEY, workspace_id, slack_user_id, csrf_token, created_at, last_seen_at, expires_at)`
- `web_login_states(state_hash PRIMARY KEY, nonce, return_to, expires_at)`
- `audit_log(id, at, actor, action, details)`, `details` als JSON
- `attendance_days`, Primärschlüssel `(workspace_id, slack_user_id, day)`, mit Indizes auf `(workspace_id, day)` und `(workspace_id, iso_week)`
- `office_closed_days`, Primärschlüssel `(workspace_id, day)`
- `verified_days`, Primärschlüssel `(workspace_id, day)`
- `monthly_stats`, Primärschlüssel `(workspace_id, month)`

Abgelaufene Sessions und Login-States sowie `attendance_days` älter als `STATS_RETENTION_MONTHS` (außer bei `0`) räumt der bestehende Cleanup-Job auf. `monthly_stats` wird nie automatisch gelöscht.

### 8. Konfiguration

| Variable                 | Standard | Zweck                                                                                    |
| ------------------------ | -------- | ---------------------------------------------------------------------------------------- |
| `WEB_LISTEN_ADDR`        | leer     | z. B. `:8080`. Leer bedeutet, dass die Weboberfläche aus ist.                            |
| `WEB_BASE_URL`           | –        | öffentliche `https://`-Adresse. Pflicht, wenn die Oberfläche aktiv ist.                  |
| `SLACK_CLIENT_ID`        | –        | Pflicht, wenn die Oberfläche aktiv ist                                                   |
| `SLACK_CLIENT_SECRET`    | –        | Pflicht, wenn die Oberfläche aktiv ist. Gehört in `.env.local` und wird im Log maskiert. |
| `WEB_SESSION_TTL`        | `12h`    | maximale Dauer einer Session                                                             |
| `WEB_SESSION_IDLE`       | `2h`     | Abmeldung nach dieser Zeit ohne Aktivität                                                |
| `WEB_STATS_PER_PERSON`   | `false`  | schaltet Auswertungen und Export pro Person für Admins frei                              |
| `STATS_RETENTION_MONTHS` | `24`     | Aufbewahrung von `attendance_days`. `0` schaltet die automatische Löschung ab.           |
| `OFFICE_DESKS`           | leer     | Anzahl der heute vorhandenen Plätze, als Vergleichswert in den Auswertungen              |

Ist die Oberfläche aktiv und fehlt `https://` in `WEB_BASE_URL` oder das Client Secret, startet der Bot nicht.

### 9. Betrieb hinter Caddy

Der Bot selbst spricht nur HTTP und kennt kein TLS. Für den Betrieb liefert das Repository einen optionalen **Caddy** als Reverse Proxy mit. Er wird über das Compose-Profil `web` gestartet (`docker compose --profile web up -d`). Ohne das Profil läuft nur der Bot. Dann kann ein beliebiger anderer Proxy `officebot:8080` erreichen.

#### Zwei Betriebsarten

| `CADDY_TLS`       | Wann                                                       | Zertifikat                                                                                                                                      |
| ----------------- | ---------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `auto` (Standard) | Die Ports 80 und 443 sind von außen erreichbar             | Caddy holt und erneuert es selbst bei Let's Encrypt, per HTTP-01 (Port 80) oder TLS-ALPN-01 (Port 443).                                         |
| `files`           | anderer öffentlicher Port, nur intern erreichbar, Wildcard | Ein externer Dienst stellt das Zertifikat per DNS-01 aus und legt es in `CADDY_CERT_DIR` ab. Caddy liest nur `fullchain.pem` und `privkey.pem`. |

Let's Encrypt prüft bei HTTP-01 und TLS-ALPN-01 immer Port 80 bzw. 443. Einen anderen Port kann man dafür nicht angeben. `auto` funktioniert mit anderen Host-Ports deshalb nur, wenn eine Firewall oder ein Router 80 bzw. 443 von außen dorthin weiterleitet. In allen anderen Fällen gilt `files`.

Der externe Dienst für DNS-01 ist bewusst **kein Teil dieses Projekts**. Dieselbe Aufgabe stellt sich auch bei anderen Diensten, etwa bei GitLab und bei Wildcard-Zertifikaten. Sie wird deshalb in einem eigenen Projekt gelöst. Dieses Projekt erwartet von ihm nur zwei Dinge: die beiden Dateien im Verzeichnis und einen Aufruf nach jeder Erneuerung.

#### Umsetzung

- Zwei Caddyfiles, eingebettet in `compose.yaml` als Umgebungsvariablen des Service `caddy` (`CADDYFILE_auto`, `CADDYFILE_files`). Beim Start schreibt der Container das per `CADDY_TLS` gewählte nach `/tmp/Caddyfile` (tmpfs) und startet Caddy damit. Ein Server braucht so weiterhin nur `compose.yaml` und die Env-Dateien, keinen Klon des Repositorys. In `compose.yaml` stehen die Platzhalter als `{$$VAR}`, damit Compose sie Caddy überlässt.

  ```
  # Caddyfile.auto
  {
      email {$ACME_EMAIL}
  }
  {$OFFICEBOT_DOMAIN} {
      reverse_proxy officebot:8080
  }

  # Caddyfile.files
  {$OFFICEBOT_DOMAIN} {
      tls /certs/fullchain.pem /certs/privkey.pem
      reverse_proxy officebot:8080
  }
  ```

- Der Service `caddy` in `compose.yaml`:
  - Image `caddy:2`, `container_name: officebot-caddy`, Profil `web`, `restart: unless-stopped`
  - `ports`: `${PROXY_HTTP_PORT:-80}:80` und `${PROXY_HTTPS_PORT:-443}:443`
  - Bind-Mounts unter `./volumes/caddy/data` und `./volumes/caddy/config`, wie beim Bot keine Docker-Volumes. Die Zertifikate aus `auto` liegen dort und müssen erhalten bleiben.
  - Bei `files` zusätzlich `${CADDY_CERT_DIR:-./volumes/certs}:/certs:ro`
  - Härtung wie beim Bot: `read_only`, `cap_drop: ALL` plus `cap_add: NET_BIND_SERVICE`, `no-new-privileges`
- Der Bot bekommt keinen Host-Port, nur `expose: ["8080"]`. Abschnitt 1 gilt entsprechend: Der Port ist nur im Compose-Netz erreichbar.
- **Neu laden nach einer Erneuerung (`files`):** Caddy liest geänderte Zertifikatsdateien nicht von selbst ein. Der externe Dienst ruft deshalb nach jeder Erneuerung `docker exec officebot-caddy caddy reload --config /tmp/Caddyfile --force` auf. Ohne `--force` würde Caddy den Reload bei unveränderter Konfiguration überspringen.
- Läuft Caddy auf einem anderen öffentlichen Port als 443, muss dieser Port in `WEB_BASE_URL` und in der Redirect-URL der Slack-App stehen, z. B. `https://officebot.example.com:8443`.

#### Variablen für Compose

Diese Variablen liest Compose, nicht der Bot. Sie stehen trotzdem in `.env` und werden in `dotenv.md` dokumentiert.

| Variable           | Standard          | Zweck                                                             |
| ------------------ | ----------------- | ----------------------------------------------------------------- |
| `OFFICEBOT_DOMAIN` | –                 | Hostname der Oberfläche, z. B. `officebot.example.com`            |
| `CADDY_TLS`        | `auto`            | `auto` oder `files`                                               |
| `ACME_EMAIL`       | –                 | optionale Kontaktadresse für das Let's-Encrypt-Konto (nur `auto`) |
| `PROXY_HTTP_PORT`  | `80`              | Host-Port für HTTP                                                |
| `PROXY_HTTPS_PORT` | `443`             | Host-Port für HTTPS                                               |
| `CADDY_CERT_DIR`   | `./volumes/certs` | Verzeichnis mit `fullchain.pem` und `privkey.pem` (nur `files`)   |

### 10. Datenschutz und Mitbestimmung

- Eine Auswertung der Anwesenheit pro Person ist eine Verarbeitung personenbezogener Daten und kann als technische Einrichtung zur Leistungs- und Verhaltenskontrolle gelten (§ 87 Abs. 1 Nr. 6 BetrVG). Deshalb ist die Ansicht pro Person **standardmäßig aus**. Sie erscheint nur mit `WEB_STATS_PER_PERSON=true` und nur für Admins.
- Die Argumentation für Shared Desks stützt sich auf die Auswertungen **ohne Namen** (Abschnitt 6). Sie reichen dafür aus.
- Jeder Aufruf der Ansicht pro Person und jeder Export pro Person landet im Audit-Log.
- Daten pro Person haben eine feste, dokumentierte Löschfrist (`STATS_RETENTION_MONTHS`). Unbegrenzt bleiben nur Zahlen ohne Personenbezug. Gegenüber Betriebsrat und Datenschutz ist das leichter zu begründen als eine unbegrenzte Speicherung (Speicherbegrenzung, Art. 5 Abs. 1 lit. e DSGVO).
- Nach einem Austritt oder einer Löschanfrage entfernt `stats purge --user` die Daten der Person.
- Das README bekommt unter „Bekannte Einschränkungen“ einen Abschnitt zum Datenschutz bei Auswertungen: Team informieren, Betriebsrat und Datenschutz einbinden, Aufbewahrungsfristen nennen.

### 11. Umsetzung in Schritten

Jeder Schritt ist ein eigener Commit:

1. **Fundament:** Migration, Storage für `web_users` (inklusive Schutz des letzten Admins), Audit-Log, Konfiguration, CLI `web-user`.
2. **Server und Anmeldung:** HTTP-Server im `run`-Loop, Middleware (Session, Rolle, CSRF, Header), OIDC-Login und -Logout, Layout, `/access`.
3. **Verwaltungsseiten:** Übersicht, Woche, Abwesenheiten, Sprachen, geschlossene Tage, Audit-Log.
4. **Datengrundlage:** `attendance_days` mit Befüllen aus vorhandenen Wochen, Neuberechnung bei Korrekturen, `verified_days` mit Tagesprüfung `/days/{datum}` und CLI `attendance verify`, `monthly_stats` mit anonymen Verteilungen, automatische Löschung, `stats purge` in CLI und unter `/retention`.
5. **Auswertungen:** Paket `internal/stats`, Seiten, SVG-Diagramme, CSV-Export, Methodik.
6. **Betrieb und Doku:** Caddy-Service und Caddyfiles (Abschnitt 9), README de/en, `dotenv.md` de/en, `slack-app-setup.md` de/en, Status dieses ADR aktualisieren.

**Tests:**

- Login gegen einen nachgebauten Slack-Server (`httptest`): falsches Team, falscher `state` oder `nonce`, unbekannte Person, entzogener Zugang während einer laufenden Session
- Rollen und CSRF
- Schutz des letzten Admins in CLI und Web
- Regeln für den Tagesstatus, Neuberechnung nach `attendance`, Markierung `corrected`
- geprüfte Tage: Bestätigen per Web und CLI, Filter „nur geprüfte Tage“, Anteil geprüfter Tage
- `stats purge`: Vorschau ohne `--yes`, `monthly_stats` vor dem Löschen geschrieben, Auswertungen ohne Namen danach unverändert, `--user` löscht auch Antworten und Abwesenheiten, `STATS_RETENTION_MONTHS=0`
- Zeiträume ohne Office über Wochen- und Monatsgrenzen sowie über geschlossene Tage hinweg
- Perzentile und Verteilungen mit festen Testdaten

### Hinweise zur Umsetzung

Wo die Umsetzung vom Plan oben abweicht oder ihn genauer fasst:

- **Neuberechnung über Trigger:** Statt jede Änderung einzeln eine Neuberechnung anstoßen zu lassen, markieren Datenbank-Trigger eine geschlossene Woche als geändert (`polls.stats_dirty`), sobald sich ihre Übersichtsversion oder eine Abwesenheit einer teilnehmenden Person ändert. Der Scheduler und die Weboberfläche berechnen markierte Wochen neu. So ist jeder Weg abgedeckt, auch die CLI.
- **Erstes Befüllen:** Die Migration markiert alle geschlossenen Wochen; der erste Lauf des Schedulers berechnet sie. Das Ergebnis ist dasselbe wie ein Befüllen in der Migration selbst.
- **Planung festhalten:** Bevor eine geschlossene Woche korrigiert wird (`attendance`, Tagesprüfung, `away remove --id`), berechnet der Bot ihren Tagesstatus, falls das noch nicht geschehen ist. So landet nie eine Korrektur als Planung in `attendance_days`.
- **Tagesprüfung und vergangene Abwesenheiten:** „Abwesend“ in der Tagesprüfung trägt eine Abwesenheit für diesen einen Tag ein, auch in der Vergangenheit. „Keine Antwort“ lässt sich dort nicht setzen. Wer keine Antwort hatte, bekommt mit der ersten Korrektur eine Antwort für die ganze Woche.
- **Monatswerte:** `monthly_stats` speichert die Werte eines Monats als JSON, darunter das Histogramm der täglichen Belegung. Daraus lassen sich Perzentile und die Kurve der ausreichenden Plätze für beliebige Zeiträume exakt zusammensetzen. Monate, aus denen Tagesdaten gelöscht wurden, sind eingefroren (`frozen`) und werden nicht mehr neu berechnet; die Auswertung nimmt für sie immer die gespeicherten Werte.
- **Löschfrist in ganzen Monaten:** `STATS_RETENTION_MONTHS` löscht immer ganze Monate, damit kein Monat nur teilweise Tagesdaten hat.
- **Sprache der Oberfläche:** Die Weboberfläche hat einen eigenen kleinen Katalog für Englisch und Deutsch, weil die 45 Kataloge des Bots sonst alle um die Texte der Oberfläche wachsen müssten. Standard ist Englisch; ein Umschalter **EN | DE** im Kopf (auch auf der Login-Seite) setzt ein Cookie (`officebot_lang`, ein Jahr). Die Bot-Sprache einer Person spielt keine Rolle mehr (ergänzt am 2026-11-05).
- **Dritte Rolle `analyst`** (ergänzt am 2026-11-05): sieht alles, was ein Viewer sieht, dazu die Auswertungen und den Export pro Person (nur mit `WEB_STATS_PER_PERSON=true`), ändert aber nichts und hat keinen Zugriff auf die Verwaltung und das Audit-Log. Gedacht für Teamleitung, HR oder Betriebsrat. Migration `0008` baut `web_users` neu auf, weil SQLite den CHECK auf die Rolle nicht ändern kann.
- **Entwicklungs-Login:** Mit `DRY_RUN=true` und einer `WEB_BASE_URL` auf localhost gibt es einen Login ohne Slack für Personen mit Zugang. Damit lässt sich die Oberfläche lokal ausprobieren.
- **Diagramme:** Die SVG-Diagramme zeigen exakte Werte als native Tooltips (`<title>`), daneben stehen die Zahlen in Tabellen.
- **Caddy als root im Container:** Caddy läuft im Container als root, damit er die Ports 80 und 443 öffnen kann, aber ohne weitere Capabilities (`cap_drop: ALL`, nur `NET_BIND_SERVICE`). Mit `no-new-privileges` würde ein normaler Benutzer die Datei-Capability des Caddy-Binaries nicht bekommen. Die Dateien unter `./volumes/caddy/` gehören deshalb root.
- **Caddyfile aus einer Umgebungsvariable:** Ein eingebundenes Caddyfile braucht die Datei auf dem Server; ohne Klon legt Docker an ihrer Stelle ein leeres Verzeichnis an, und Caddy startet nicht. Compose-`configs` mit eingebettetem `content` lassen sich nicht in einen schreibgeschützten Container schreiben und scheiterten auf dem Produktivserver mit `mkdirat etc/caddy/Caddyfile: file exists`. Die Umgebungsvariable funktioniert mit jeder Compose-Version, und der Container bleibt schreibgeschützt.
- **Zusätzlich:** `/weeks` zum Korrigieren ganzer Wochen, ein Export pro Person (`/stats/persons.csv`, Analysten und Admins mit `WEB_STATS_PER_PERSON`) und die Anmeldungen im Audit-Log (`login`, `login.denied`).

## Betrachtete Alternativen

- **Login-Link per Slack-DM:** Er braucht keine Änderung an der Slack-App und keine öffentliche HTTPS-Adresse. Allerdings muss man sich nach jedem Ablauf der Session neu einen Link holen. Verworfen zugunsten des gewohnten Login-Ablaufs.
- **Reverse Proxy mit Firmen-SSO** (oauth2-proxy, Authelia): Er braucht zusätzliche Infrastruktur, und der Bot müsste Headern vertrauen. Die Rechte lägen außerhalb des Bots und ließen sich nicht in der Oberfläche pflegen.
- **Statistiken nur aus `responses` berechnen:** Das ist einfacher, aber durch `RETENTION_WEEKS` auf ein halbes Jahr begrenzt. Außerdem ließe sich nicht unterscheiden, ob jemand im Urlaub war oder keine Antwort gegeben hat. Wer `RETENTION_WEEKS` stattdessen stark erhöht, hält auch alle Slack-Bezüge und DM-Daten länger vor als nötig.
- **Daten pro Person unbegrenzt aufbewahren:** Technisch kein Problem, die Datenmenge ist gering. Der Zweck, die Planung der Büroplätze, braucht aber keine Tagesdaten pro Person über Jahre. Langfristige Trends liefern die Zahlen ohne Namen. Außerdem blieben die Daten ehemaliger Kolleginnen und Kollegen für immer gespeichert, und eine Betriebsvereinbarung verlangt in der Regel eine feste Frist. Wer trotzdem nur manuell löschen will, setzt `STATS_RETENTION_MONTHS=0`.
- **Auswertungen im Slack-App-Home:** Dafür braucht es keinen Webserver. Slack-Blöcke eignen sich aber kaum für Diagramme, Zeiträume und Exporte.
- **JavaScript-Diagrammbibliothek:** schönere Interaktion, aber mehr Abhängigkeiten und eine lockerere CSP. SVG vom Server reicht für die benötigten Diagramme.
- **TLS direkt im Bot** (z. B. `autocert`): Er bräuchte selbst die Ports 80 oder 443 und müsste Zertifikate verwalten. Ein Proxy davor ist einfacher auszutauschen und hält den Bot frei von TLS.
- **Traefik statt Caddy:** Traefik kann DNS-01 bei AutoDNS über lego direkt. Für den Standardfall mit den Ports 80 und 443 ist Caddy aber deutlich einfacher zu konfigurieren. DNS-01 übernimmt ein eigener Dienst, der auch für andere Server gebraucht wird (Abschnitt 9).
- **Caddy mit eigenem DNS-Modul für AutoDNS:** Es gibt kein offizielles Modul, nur die Bibliothek `libdns/autodns`. Ein eigenes Modul und ein eigenes Caddy-Image müssten dauerhaft gepflegt werden.

## Konsequenzen

- Verwaltung ist auch ohne Zugang zum Server möglich. Die CLI bleibt vollständig erhalten.
- Für die Oberfläche braucht es eine HTTPS-Adresse, einen Reverse Proxy und eine angepasste Slack-App. Mit dem Profil `web` und freien Ports 80 und 443 bringt das Repository den Proxy samt Zertifikat selbst mit. Ohne `WEB_LISTEN_ADDR` ändert sich nichts.
- Für andere Ports, nur interne Erreichbarkeit oder Wildcard-Zertifikate ist der externe Zertifikatsdienst nötig.
- Auswertungen ohne Namen, einschließlich der Verteilungen für die Shared-Desk-Frage, reichen unbegrenzt zurück. Auswertungen pro Person und der Filter „nur geprüfte Tage“ reichen so weit zurück, wie Tagesdaten vorhanden sind. Zu Beginn reicht die Historie nur so weit, wie noch Wochen in der Datenbank liegen.
- Die Zahlen sind nur so gut wie die Antwortquote, der Anteil geprüfter Tage und die Pflege der geschlossenen Tage. Alle drei zeigt die Oberfläche offen an.
- Die Tagesprüfung macht den Abgleich, den Admins heute pro Person und Woche über die CLI erledigen, zu einem Klick pro Tag.
- Pro Person wird zusätzlich ein Tagesstatus über bis zu 24 Monate gespeichert. Das muss vor der Einführung mit Team, Betriebsrat und Datenschutz abgestimmt werden.
