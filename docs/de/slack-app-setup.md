# Slack-App anlegen

Schritt-für-Schritt-Anleitung, um die Slack-App für den Office Attendance Bot anzulegen. Dafür muss man den Betrieb des Bots oder Docker nicht kennen. Am Ende stehen ein paar Werte für `.env.local`, die du an die Person weitergibst, die den Bot betreibt.

Zeitbedarf: ca. 10 Minuten. 🇬🇧 [English version](../en/slack-app-setup.md)

---

## Vorbereitung

- Du brauchst die Berechtigung, Apps im Slack-Workspace zu installieren. In vielen Workspaces dürfen das nur Admins. Sonst bietet Slack **Request to Install** an, und ein Admin muss die Anfrage genehmigen.
- Lege einen **Ergebnis-Channel** fest, in dem die wöchentliche Übersicht erscheint, z. B. `#office`.
- Lege fest, **wer befragt wird**:
  - **Modus A:** alle Mitglieder eines Channels, z. B. `#team-berlin`
  - **Modus B:** eine feste Liste von Personen

Die Konfigurationsseiten unter api.slack.com sind nur auf Englisch. Die Bezeichnungen unten beziehen sich darauf.

---

## Schritt 1: App aus dem Manifest anlegen

1. <https://api.slack.com/apps> öffnen und anmelden.
2. **Create New App** → **From a manifest** wählen.
3. Den Workspace auswählen und **Next** klicken.
4. Zum Tab **YAML** wechseln, den Inhalt durch folgendes Manifest ersetzen und **Next** → **Create** klicken.

```yaml
display_information:
  name: Büroplanung
  description: Wöchentliche Abfrage der Bürotage
features:
  bot_user:
    display_name: bueroplanung
    always_online: true
  app_home:
    home_tab_enabled: false
    messages_tab_enabled: true
    messages_tab_read_only_enabled: true
oauth_config:
  scopes:
    bot:
      - chat:write
      - im:write
      - im:history
      - users:read
      - channels:read
      - groups:read
      - channels:history
      - groups:history
settings:
  interactivity:
    is_enabled: true
  socket_mode_enabled: true
  org_deploy_enabled: false
  token_rotation_enabled: false
```

Unter dem Namen „Büroplanung“ (`name`) sehen alle den Bot in Slack. Erwähnt und eingeladen wird er über `display_name`, also `@bueroplanung`. Der `display_name` darf keine Umlaute und keine Leerzeichen enthalten, deshalb „ue“ statt „ü“; am sichersten sind Kleinbuchstaben, Ziffern, `-`, `_` und `.`. Beide Werte kannst du vor dem Anlegen ändern. Verwende den neuen `display_name` dann überall dort, wo diese Anleitung `@bueroplanung` schreibt.

Das Manifest schaltet bereits Socket Mode (keine öffentliche URL nötig) und Interaktivität (der **Speichern**-Button und das Formular **Abwesenheit planen**) ein. Für das Formular sind keine zusätzlichen Berechtigungen nötig. Außerdem setzt es alle nötigen Berechtigungen. Wofür jede Berechtigung gebraucht wird, steht unter [Benötigte OAuth-Scopes](README.md#benötigte-oauth-scopes).

---

## Schritt 2: App-Level-Token erzeugen (`xapp-…`)

Mit diesem Token baut der Bot seine Socket-Mode-Verbindung zu Slack auf.

1. In der App zu **Settings → Basic Information** gehen.
2. Unter **App-Level Tokens** auf **Generate Token and Scopes** klicken.
3. Einen Namen eingeben, z. B. `socket`. Über **Add Scope** den Scope **`connections:write`** wählen und **Generate** klicken.
4. Den Token kopieren, der mit `xapp-` beginnt. Er wird zu **`SLACK_APP_TOKEN`**.

---

## Schritt 3: Interaktivität und Socket Mode prüfen

Beides setzt das Manifest bereits. Zur Kontrolle:

- **Settings → Socket Mode**: *Enable Socket Mode* muss an sein.
- **Features → Interactivity & Shortcuts**: *Interactivity* muss an sein. Eine Request-URL ist nicht nötig.

---

## Schritt 4: App installieren und Bot-Token kopieren (`xoxb-…`)

1. Zu **Settings → Install App** gehen und **Install to Workspace** (bzw. **Request to Install**) klicken.
2. Die Berechtigungen mit **Allow** bestätigen.
3. Den **Bot User OAuth Token** kopieren, der mit `xoxb-` beginnt. Er wird zu **`SLACK_BOT_TOKEN`**.

> Werden die Berechtigungen später geändert, zeigt Slack einen gelben Hinweis. Die App muss dann **neu installiert** werden, sonst wirkt die Änderung nicht.

---

## Schritt 5: Bot in die Channels einladen

In Slack (nicht auf api.slack.com):

- **Ergebnis-Channel:** im Channel `/invite @bueroplanung` eingeben. Ohne Einladung darf der Bot die Übersicht nicht posten.
- **Teilnehmer-Channel (nur Modus A):** dort ebenfalls `/invite @bueroplanung`, damit der Bot die Mitgliederliste lesen darf.

Private Channels funktionieren auch. Der Bot muss nur eingeladen sein.

---

## Schritt 6: IDs herausfinden

Der Bot braucht IDs, keine Namen.

- **Channel-ID** (`C…`): Den Channel öffnen und oben auf seinen Namen klicken. Die ID steht ganz unten im Dialog, mit einem Kopier-Button.
- **User-ID** (`U…`, nur Modus B): Auf den Namen einer Person klicken → **Vollständiges Profil anzeigen** → **⋮** (Mehr) → **Mitglieds-ID kopieren**.
- **Viele User-IDs auf einmal:** Sobald der Bot-Token in `.env.local` steht, listet `docker compose run --rm officebot list-users` alle aktiven Personen mit ihren IDs auf. Am Ende steht eine fertige Zeile `SLACK_USER_IDS=…` zum Kopieren. Mit `--channel C…` werden nur die Mitglieder eines Channels gezeigt. Bots und deaktivierte Accounts fehlen in der Liste, `--all` zeigt sie mit an.

---

## Schritt 7: Werte übergeben

Trage diese Werte in `.env.local` ein (Vorlage: `.env.local.example`) oder gib sie an die Person weiter, die den Bot betreibt. **Die Tokens sind geheim.** Gib sie nur über einen sicheren Weg weiter, nie im Klartext per Slack oder E-Mail, und committe sie nie ins Git.

```dotenv
SLACK_BOT_TOKEN=xoxb-...            # Schritt 4
SLACK_APP_TOKEN=xapp-...            # Schritt 2
SLACK_SUMMARY_CHANNEL_ID=C...       # Schritt 6, Ergebnis-Channel

# Modus A: alle Mitglieder eines Channels
SLACK_PARTICIPANTS_CHANNEL_ID=C...  # Schritt 6, Teilnehmer-Channel
# Modus B: feste Liste (SLACK_PARTICIPANTS_CHANNEL_ID dann leer lassen)
SLACK_USER_IDS=U...,U...
```

Setze **entweder** `SLACK_PARTICIPANTS_CHANNEL_ID` **oder** `SLACK_USER_IDS`, nicht beides.

---

## Schritt 8: Kurzer Funktionstest

Sobald der Bot läuft (`docker compose up -d --build`):

1. `docker compose ps` zeigt den Container als **healthy**. Dann steht die Socket-Mode-Verbindung.
2. Abfrage auslösen mit `docker compose exec officebot /officebot start-week`. Innerhalb von ca. 30 s bekommen alle Befragten eine DM von **Büroplanung**, und im Ergebnis-Channel erscheint die Übersicht.
3. In der DM einen Tag ankreuzen und **Speichern** klicken. Die DM bestätigt mit „✅ Gespeichert“, und die Übersicht wird aktualisiert.

Für den ersten Test am besten nur die eigene ID in `SLACK_USER_IDS` eintragen und einen Test-Channel als Ergebnis-Channel verwenden.

---

## Häufige Probleme

| Problem                                     | Lösung                                                                                                               |
|---------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| Log: `invalid_auth` / `not_authed`          | `SLACK_BOT_TOKEN` falsch oder unvollständig. Unter **Install App** neu kopieren.                                     |
| Log: `socket mode: invalid app token`       | `SLACK_APP_TOKEN` falsch oder ohne `connections:write`. Neuen Token erzeugen (Schritt 2).                            |
| Log: `missing_scope`                        | Eine Berechtigung fehlt. Mit dem Manifest abgleichen (**Features → App Manifest**) und die App **neu installieren**. |
| Log: `not_in_channel` / `channel_not_found` | Bot in den Channel einladen (Schritt 5) und Channel-ID prüfen (Schritt 6).                                           |
| Keine Teilnehmer gefunden (Modus A)         | Der Bot ist nicht im Teilnehmer-Channel, oder der Channel enthält nur Bots bzw. deaktivierte Benutzer.               |
| Klick auf **Speichern** ohne Wirkung        | Prüfen, ob Interaktivität an ist (Schritt 3) und der Container healthy ist.                                          |
| App lässt sich nicht installieren           | Der Workspace verlangt eine Admin-Freigabe. **Request to Install** nutzen und einen Admin bitten.                    |

Weitere Fehlerdiagnose: [README → Fehlerdiagnose](README.md#fehlerdiagnose).
