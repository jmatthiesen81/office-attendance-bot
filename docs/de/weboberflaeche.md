# Weboberfläche und Auswertungen

Der Bot kann optional eine Weboberfläche für Verwaltung und Auswertungen bereitstellen. Angemeldet wird mit dem Slack-Konto („Sign in with Slack“). Ohne `WEB_LISTEN_ADDR` verhält sich der Bot genau wie bisher. Hintergrund und Entscheidungen: [ADR 0003](adr/0003-weboberflaeche-und-auswertungen.md). 🇬🇧 English: [../en/web-interface.md](../en/web-interface.md)

## Inhalt

1. [Was sie bietet](#was-sie-bietet)
2. [Einrichtung](#einrichtung)
3. [Zugang und Rollen](#zugang-und-rollen)
4. [Tagesprüfung: tatsächliche Anwesenheit](#tagesprüfung-tatsächliche-anwesenheit)
5. [Auswertungen](#auswertungen)
6. [Aufbewahrung und Löschen](#aufbewahrung-und-löschen)
7. [Sicherheit](#sicherheit)
8. [Datenschutz und Mitbestimmung](#datenschutz-und-mitbestimmung)

---

## Was sie bietet

| Seite             | Rolle         | Inhalt                                                                                             |
| ----------------- | ------------- | -------------------------------------------------------------------------------------------------- |
| Übersicht         | admin, viewer | Laufende Abfragen: wer geantwortet hat, Zustellstatus, Outbox (wie `officebot status`)             |
| Auswertungen      | admin, viewer | Belegung des Office, benötigte Plätze, Verteilungen; pro Person nur für Admins (siehe unten)       |
| Tagesprüfung      | admin         | Planung pro Tag mit der tatsächlichen Anwesenheit abgleichen und den Tag als geprüft markieren     |
| Wochen            | admin         | Antworten einer Woche korrigieren (wahlweise mit Hinweis im Thread), DMs und Übersicht neu rendern |
| Abwesenheiten     | admin         | Abwesenheiten eintragen und löschen (wie `officebot away`)                                         |
| Geschlossene Tage | admin         | Feiertage und Betriebsurlaub; sie zählen in keiner Auswertung                                      |
| Sprachen          | admin         | Sprache der DMs einer Person (wie `officebot lang`)                                                |
| Zugriff           | admin         | Wer sich mit welcher Rolle anmelden darf                                                           |
| Löschfristen      | admin         | Aufbewahrung und Löschen von Tagesdaten nach Jahr, Stichtag oder Person                            |
| Audit-Log         | admin         | Jede Änderung mit Urheber und Zeitpunkt                                                            |

Die Oberfläche spricht Deutsch und Englisch: Deutsch für Personen, die im Bot eine deutsche Variante gewählt haben, sonst Englisch. Alle Änderungen folgen denselben Regeln wie die CLI und erreichen Slack über den laufenden Bot innerhalb von 30 Sekunden.

## Einrichtung

### 1. Slack-App erweitern

In den App-Einstellungen unter <https://api.slack.com/apps>:

1. **OAuth & Permissions** → **Redirect URLs**: `https://officebot.example.com/auth/callback` eintragen (eure `WEB_BASE_URL` plus `/auth/callback`).
2. **OAuth & Permissions** → **User Token Scopes**: `openid` und `profile` hinzufügen.
3. **Basic Information** → **App Credentials**: **Client ID** und **Client Secret** kopieren.

Das passende Manifest und eine Schritt-für-Schritt-Anleitung stehen in [Slack-App anlegen, Schritt 9](slack-app-setup.md#schritt-9-optional-anmeldung-mit-slack-für-die-weboberfläche). Die App muss dafür nicht neu installiert werden, der Bot-Token bleibt gleich.

### 2. Konfigurieren

In `.env` (vollständige Vorlagen: [dotenv.md](dotenv.md)):

```dotenv
WEB_LISTEN_ADDR=:8080
WEB_BASE_URL=https://officebot.example.com
# Für den Reverse Proxy Caddy (Profil "web")
OFFICEBOT_DOMAIN=officebot.example.com
ACME_EMAIL=it@example.com
```

In `.env.local` (geheim):

```dotenv
SLACK_CLIENT_ID=1234567890.1234567890
SLACK_CLIENT_SECRET=...
```

| Variable                 | Standard | Bedeutung                                                                                                                 |
| ------------------------ | -------- | ------------------------------------------------------------------------------------------------------------------------- |
| `WEB_LISTEN_ADDR`        | leer     | Adresse des Webservers im Container, z. B. `:8080`. Leer = keine Weboberfläche.                                           |
| `WEB_BASE_URL`           | –        | Öffentliche Adresse, nur Schema und Host, z. B. `https://officebot.example.com`. `http://` ist nur für localhost erlaubt. |
| `SLACK_CLIENT_ID`        | –        | Client ID der Slack-App                                                                                                   |
| `SLACK_CLIENT_SECRET`    | –        | Client Secret der Slack-App (`.env.local`); wird im Log maskiert                                                          |
| `WEB_SESSION_TTL`        | `12h`    | Maximale Dauer einer Anmeldung                                                                                            |
| `WEB_SESSION_IDLE`       | `2h`     | Abmeldung nach dieser Zeit ohne Aktivität                                                                                 |
| `WEB_STATS_PER_PERSON`   | `false`  | `true`: Admins sehen und exportieren Auswertungen pro Person                                                              |
| `STATS_RETENTION_MONTHS` | `24`     | Aufbewahrung des Tagesstatus pro Person; `0` = keine automatische Löschung                                                |
| `OFFICE_DESKS`           | leer     | Anzahl der heute vorhandenen Plätze; erscheint als Vergleichswert in den Auswertungen                                     |

Ist die Weboberfläche aktiv und fehlen `WEB_BASE_URL` oder die Client-Zugangsdaten oder sind ungültig, startet der Bot nicht und nennt den Grund.

### 3. Reverse Proxy und Zertifikat

Der Bot selbst spricht nur HTTP. `compose.yaml` enthält einen optionalen **Caddy**, der TLS übernimmt. Er startet nur mit dem Profil `web`:

```bash
docker compose --profile web up -d
```

| `CADDY_TLS`       | Wann                                                       | Zertifikat                                                                                                     |
| ----------------- | ---------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `auto` (Standard) | Die Ports 80 und 443 sind aus dem Internet erreichbar      | Caddy holt und erneuert es bei Let's Encrypt (HTTP-01 über Port 80 oder TLS-ALPN-01 über Port 443)             |
| `files`           | anderer öffentlicher Port, nur intern erreichbar, Wildcard | Ein externer Dienst stellt es per DNS-01 aus und legt `fullchain.pem` und `privkey.pem` in `CADDY_CERT_DIR` ab |

Weitere Variablen für Compose (in `.env`):

| Variable           | Standard          | Bedeutung                                                    |
| ------------------ | ----------------- | ------------------------------------------------------------ |
| `OFFICEBOT_DOMAIN` | –                 | Hostname der Oberfläche; ohne ihn startet Caddy nicht        |
| `CADDY_TLS`        | `auto`            | `auto` oder `files` (wählt `deploy/caddy/Caddyfile.<modus>`) |
| `ACME_EMAIL`       | leer              | Kontaktadresse für das Let's-Encrypt-Konto (`auto`)          |
| `PROXY_HTTP_PORT`  | `80`              | Host-Port für HTTP                                           |
| `PROXY_HTTPS_PORT` | `443`             | Host-Port für HTTPS                                          |
| `CADDY_CERT_DIR`   | `./volumes/certs` | Verzeichnis mit `fullchain.pem` und `privkey.pem` (`files`)  |

Hinweise:

- Let's Encrypt prüft immer Port 80 oder 443. `auto` funktioniert mit anderen Host-Ports nur, wenn eine Firewall oder ein Router 80/443 von außen dorthin weiterleitet.
- Bei einem anderen öffentlichen Port als 443 gehört dieser Port in `WEB_BASE_URL` und in die Redirect-URL der Slack-App, z. B. `https://officebot.example.com:8443`.
- Bei `files` muss der externe Dienst Caddy nach jeder Erneuerung neu laden, weil Caddy geänderte Zertifikatsdateien nicht von selbst liest:
  ```bash
  docker exec officebot-caddy caddy reload --config /etc/caddy/Caddyfile --force
  ```
- Die Zertifikate von Caddy (`auto`) liegen in `./volumes/caddy/` und müssen erhalten bleiben.
- Anderer Reverse Proxy statt Caddy: Profil weglassen und im Compose-Netz auf `officebot:8080` weiterleiten.

### 4. Ersten Zugang freischalten

Anmelden kann sich erst jemand, wenn der erste Admin auf der Konsole eingetragen ist:

```bash
docker compose exec officebot /officebot web-user add --user U0123ABC          # Admin
docker compose exec officebot /officebot web-user add --user U0456DEF --role viewer
docker compose exec officebot /officebot web-user list
```

Danach können Admins den Zugang auch auf der Seite **Zugriff** verwalten.

### Entwicklung ohne Slack

Mit `DRY_RUN=true` und `WEB_BASE_URL=http://localhost:8080` bietet die Anmeldeseite für jede Person mit Zugang einen Entwicklungs-Login ohne Slack an. In keiner anderen Konfiguration ist er verfügbar.

## Zugang und Rollen

| Rolle    | Darf                                                                                            |
| -------- | ----------------------------------------------------------------------------------------------- |
| `admin`  | Alles: Verwaltung, Korrekturen, Zugriff, Löschfristen, Auswertungen pro Person (wenn aktiviert) |
| `viewer` | Übersicht und die zusammengefassten Auswertungen                                                |

CLI:

| Befehl                                            | Wirkung                                            |
| ------------------------------------------------- | -------------------------------------------------- |
| `web-user list`                                   | Alle mit Zugang, Rolle, letzte Anmeldung           |
| `web-user add --user U [--role admin\|viewer]`    | Zugang freischalten (Standard: admin)              |
| `web-user set-role --user U --role admin\|viewer` | Rolle ändern                                       |
| `web-user remove --user U`                        | Zugang entziehen und alle Sitzungen sofort beenden |

Der letzte Admin kann weder entfernt noch herabgestuft werden, weder in der CLI noch in der Oberfläche. Rollenwechsel und Entzug gelten sofort, auch für laufende Sitzungen. Jede Änderung landet im Audit-Log.

## Tagesprüfung: tatsächliche Anwesenheit

Die Abfrage erfasst die **geplante** Anwesenheit. Auf der Seite **Tagesprüfung** gleicht ein Admin einen Tag mit der tatsächlichen Anwesenheit ab:

- Alle an diesem Tag Befragten stehen mit Office, remote, abwesend oder keine Antwort in der Liste.
- Eine Änderung korrigiert die Antwort (Office/remote) oder trägt für diesen Tag eine Abwesenheit ein (abwesend). Vergangene Tage sind hier erlaubt.
- Wer keine Antwort hatte, bekommt mit der ersten Korrektur eine Antwort für die ganze Woche; die übrigen Tage zählen dann als remote.
- **Tag als geprüft markieren** bestätigt, dass der Status aller Befragten an diesem Tag stimmt. In der CLI: `officebot attendance verify --date 2026-11-04` (`--undo` entfernt die Markierung).

Der Stand zur Frist bleibt als Planung erhalten. Jede spätere Änderung zählt als Korrektur; der Anteil der Korrekturen an geprüften Tagen zeigt, wie verlässlich die ungeprüften Tage sind.

## Auswertungen

Der Bot führt für jede geschlossene Abfrage einen Tagesstatus pro Person und Arbeitstag (Tabelle `attendance_days`): Office, wenn der Tag in der Antwort angekreuzt ist; sonst abwesend, wenn eine Abwesenheit den Tag abdeckt; sonst remote, wenn die Person geantwortet hat; sonst keine Antwort. Offene Wochen erscheinen erst nach ihrer Frist.

Die Seite **Auswertungen** zeigt für einen Zeitraum von Monaten:

- **Aussagekraft:** Anteil geprüfter Tage, Antwortquote (Warnung unter 90 %), Abweichung von der Planung. Ungeprüfte Tage geben die geplante Anwesenheit wieder. Der Filter **Nur geprüfte Tage** beschränkt alles auf die tatsächliche Anwesenheit.
- **Belegung:** Arbeitstage, Durchschnitt, Median, P90, P95 und Maximum der Personen im Office; wie viele Plätze an 95 %, 90 % und allen Tagen gereicht hätten, verglichen mit `OFFICE_DESKS`.
- **Diagramme:** Personen im Office pro Tag, Durchschnitt nach Wochentag, Anteil der Tage, an denen *n* Plätze gereicht hätten, Office-Tage pro Person und Monat, längste Zeit ohne Office pro Person. Die letzten beiden sind Verteilungen ohne Namen.
- **Pro Monat:** Arbeitstage, Personentage im Office, Durchschnitt, Maximum, Antwortquote, geprüfte Tage.
- **Pro Person** (Admins, nur mit `WEB_STATS_PER_PERSON=true`): Office-Tage und Quote, remote, abwesend, keine Antwort, letzter Office-Tag, aktueller und längster Zeitraum ohne Office mit Aufschlüsselung nach Grund.
- **Methodik:** eine Definition jeder Kennzahl.

Geschlossene Tage (Feiertage, Betriebsurlaub) zählen in keiner Kennzahl. Ein Zeitraum ohne Office übergeht geschlossene Tage und Tage, an denen die Person nicht befragt wurde.

Exporte: **CSV ohne Namen** (`/stats.csv`, Monatswerte) für alle mit Zugang; **CSV pro Person** (`/stats/persons.csv`) für Admins mit `WEB_STATS_PER_PERSON=true`.

## Aufbewahrung und Löschen

| Daten                                            | Aufbewahrung                                                                      |
| ------------------------------------------------ | --------------------------------------------------------------------------------- |
| Abfragen, Antworten, Abwesenheiten, Slack-Bezüge | `RETENTION_WEEKS`                                                                 |
| Tagesstatus pro Person (`attendance_days`)       | `STATS_RETENTION_MONTHS` (Standard 24), ganze Monate; `0` = nur manuelles Löschen |
| Monatswerte ohne Namen (`monthly_stats`)         | unbegrenzt                                                                        |

Bevor Tagesdaten gelöscht werden, sichert der Bot die Monatswerte der betroffenen Monate und friert sie ein. So bleiben die Auswertungen ohne Namen vollständig. Solche Monate sind in den Auswertungen gekennzeichnet.

Manuell löschen, mit Vorschau auf der Seite **Löschfristen** oder in der CLI:

```bash
officebot stats purge --year 2024              # zeigt, was gelöscht würde
officebot stats purge --year 2024 --yes        # löscht
officebot stats purge --before 2025-01-01 --yes
officebot stats purge --user U0123ABC --yes    # eine Person, z. B. nach dem Austritt
```

`--user` löscht zusätzlich die Teilnahme der Person an gespeicherten Abfragen (samt Antworten), ihre Abwesenheiten und ihre Sprachwahl, damit der Tagesstatus nicht neu entstehen kann.

Ein hoher Wert für `RETENTION_WEEKS` lohnt sich vor allem einmal: Beim Einführen der Weboberfläche berechnet der Bot den Tagesstatus aus allen Wochen, die noch in der Datenbank liegen.

## Sicherheit

- Anmeldung mit Slack (OpenID Connect): `state` und `nonce` nur einmal verwendbar, Prüfung von Aussteller, Empfänger, Ablauf, Nonce und Workspace.
- Eine Sitzung bekommt nur, wer in der Zugriffsverwaltung steht. Sitzungen liegen nur als Hash in der Datenbank; das Cookie ist `HttpOnly`, `Secure`, `SameSite=Lax` (bei https mit `__Host-`-Präfix).
- CSRF-Token und Prüfung der Herkunft bei jeder Änderung; strikte Content Security Policy ohne JavaScript; Begrenzung der Anmeldeversuche.
- Der Port des Bots ist nur im Compose-Netz erreichbar.

## Datenschutz und Mitbestimmung

Eine Auswertung der Anwesenheit pro Person ist eine Verarbeitung personenbezogener Daten und kann als technische Einrichtung zur Leistungs- und Verhaltenskontrolle gelten (§ 87 Abs. 1 Nr. 6 BetrVG). Deshalb:

- Auswertungen pro Person sind **standardmäßig aus** (`WEB_STATS_PER_PERSON`) und nur für Admins sichtbar. Jeder Aufruf und jeder Export landet im Audit-Log.
- Die Argumentation für Shared Desks gelingt mit den Auswertungen ohne Namen.
- Tagesdaten pro Person haben eine feste Löschfrist; unbegrenzt bleiben nur Zahlen ohne Namen.
- Vor der Einführung das Team informieren und Betriebsrat und Datenschutz einbinden.
