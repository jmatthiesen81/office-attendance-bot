# ADR 0002: Sprache pro Person für private Nachrichten

- **Status:** Vorgeschlagen (für später geplant, nicht umgesetzt)
- **Datum:** 2026-10-13

## Kontext

Der Bot spricht heute eine einzige Sprache, festgelegt über `BOT_LANGUAGE` (`internal/config/config.go`, `cfg.Translator`). Sie gilt für alles:

- die Abfrage-DM inklusive Status, Hinweisen und Buttons (`slack.DMView.T`),
- Erinnerungen,
- ephemere Antworten auf Klicks (`Service.RejectInteraction` und die `reply.*`-Texte),
- das Abwesenheitsformular (`slack.TimeOffModal`),
- die Übersicht im Kanal (`summary.Renderer.T`).

Mit 14 Sprachen samt Nerd- und Piratenvarianten (`internal/i18n/locales/`) liegt es nahe, dass gemischte Teams nicht alle dieselbe Sprache lesen wollen. Die Übersicht im Kanal ist für alle gleich und muss bei einer Sprache bleiben. Alles, was nur eine Person sieht, kann dagegen in deren Sprache erscheinen.

Die Übersetzungsschicht ist dafür schon vorbereitet. DM-Blöcke, Formular und Datumsformate bekommen den `Translator` als Parameter und greifen nicht auf eine globale Sprache zu.

## Entscheidung (geplant)

Jede Person kann selbst festlegen, in welcher Sprache der Bot ihr private Nachrichten schreibt. Die Übersicht im Kanal bleibt bei `BOT_LANGUAGE`.

### Welche Sprache gilt

Die Sprache wird pro Person in dieser Reihenfolge bestimmt:

1. **Eigene Wahl** der Person, gespeichert in der Datenbank.
2. **Slack-Sprache** der Person (`locale` aus `users.info`), aber nur, wenn `BOT_LANGUAGE_FROM_SLACK=true` gesetzt ist und es dazu einen Katalog gibt. `de-DE` wird zu `de`, `pt-BR` zu `pt` und so weiter.
3. **`BOT_LANGUAGE`** als Teamstandard.

Stufe 2 ist standardmäßig aus. Sonst bekäme ein deutsches Team, dessen Mitglieder Slack auf Englisch nutzen, nach einem Update plötzlich englische Nachrichten. Außerdem bietet Slack viele unserer Sprachen gar nicht an, etwa Dänisch, Polnisch, Tschechisch, Türkisch und Ukrainisch. Nerd und Pirat gibt es dort natürlich auch nicht. Eine eigene Wahl ist deshalb in jedem Fall nötig.

### Bedienung in Slack

- Die Abfrage-DM bekommt im Aktionsbereich neben „🌴 Abwesenheit planen“ einen Button **„🌐 Sprache“**.
- Er öffnet ein Modal mit einer Auswahlliste (`static_select` mit Optionsgruppen):
  - „Teamstandard (Deutsch)“, um die eigene Wahl zu löschen,
  - die Sprachen,
  - die Nerd-Varianten,
  - die Piraten-Varianten.
- Jede Sprache steht in ihrer eigenen Sprache in der Liste: „Deutsch“, „Français“, „Українська“, „Nerd (Deutsch)“. Dafür bekommt jeder Katalog einen neuen Schlüssel `language.name`. Bei 42 Einträgen bleibt die Liste unter Slacks Grenze von 100 Optionen.
- Nach dem Speichern rendert der Bot die aktuelle DM jeder offenen Abfrage sofort neu in der neuen Sprache, als `update_dm`-Job wie bei Abwesenheiten. Ist das Abwesenheitsformular gerade offen, bleibt es bis zum nächsten Öffnen in der alten Sprache.
- Derselbe Button steht auch im Abwesenheitsformular und unter der Übersicht im Kanal neben „📩 Abfrage an mich senden“. So kommen auch Personen ohne aktuelle DM an die Einstellung.

### Betrieb (CLI)

```sh
officebot lang list                  # alle gespeicherten Sprachwahlen
officebot lang set --user U123 de    # für eine Person setzen
officebot lang reset --user U123     # zurück auf den Teamstandard
```

Wie `away` schreibt die CLI nur in die Datenbank und legt `update_dm`-Jobs an. Das Senden übernimmt der laufende Bot.

### Nötige Änderungen

1. **Schema:** Migration `0004_user_settings.sql` mit der Tabelle `user_settings(workspace_id, slack_user_id, language, updated_at)`, Primärschlüssel `(workspace_id, slack_user_id)`. Die Tabelle heißt bewusst allgemein, damit spätere persönliche Einstellungen dort Platz finden.
2. **Storage:** `GetUserLanguage`, `SetUserLanguage` und `ResetUserLanguage`. Für den Versand von Erinnerungen an viele Personen kommt `UserLanguages(ctx, ws, userIDs)` dazu.
3. **Service:** `s.tr()` wird durch `s.trFor(ctx, userID)` ergänzt, das die Reihenfolge oben umsetzt. Alle Stellen, die eine einzelne Person ansprechen, nutzen `trFor`: `sendDM`, `RefreshDM`, `sendReminder`, `RejectInteraction`, `HandleRequestDM`, die Abwesenheits-Handler und `timeOffModal`. Die Übersicht nutzt weiter `cfg.Translator`.
4. **Slack-Locale:** Mit Stufe 2 wird `locale` beim Auflösen der Teilnehmer mitgelesen und am Teilnehmer gespeichert. Die Abfrage braucht `users.info` mit `include_locale=true`, der vorhandene Scope `users:read` reicht. Vor der Umsetzung prüfen, ob slack-go den Parameter setzt.
5. **Socket:** Die Fehlermeldung bei einer Zeitüberschreitung im Formular (`slack.Socket.tr`) braucht ebenfalls die Sprache der Person. Dafür bekommt `InteractionHandler` eine Methode, die zu einer User-ID den Translator liefert.
6. **Robustheit:** Ist eine gespeicherte Sprache nach einem Update nicht mehr vorhanden, gilt die nächste Stufe der Reihenfolge, und der Bot schreibt eine Warnung ins Log. Die Wahl selbst bleibt gespeichert, damit sie nach einem Rollback wieder greift.
7. **Interaktionen:** Neue Aktionen `language_open` (Button) und der Callback `language` (Modal-Submit). Der Submit wird wie beim Abwesenheitsformular synchron geprüft. Ein Wert, für den es keinen Katalog gibt, wird abgelehnt.

### Abgrenzung

- **Zeitzone und Formate:** Uhrzeiten werden weiterhin in `POLL_TIMEZONE` angezeigt. Datums- und Uhrzeitformate kommen aus dem Katalog der gewählten Sprache. Eine Zeitzone pro Person (`tz` aus Slack) wäre ein eigener Schritt.
- **Übersicht:** Sie bleibt einsprachig. Mehrsprachige Übersichten oder eine Übersicht pro Sprache würden den Kanal überladen.
- **Datenschutz:** Gespeichert wird nur ein Sprachcode pro Person. Der Cleanup-Job (`Service.Cleanup`) löscht Einträge von Personen, die seit `RETENTION_WEEKS` an keiner Abfrage mehr teilgenommen haben. Die Datenschutzhinweise in der README werden entsprechend ergänzt.

## Betrachtete Alternativen

- **Nur die Slack-Sprache verwenden:** keine Bedienung nötig, aber viele Sprachen und die Spaßvarianten sind so nicht erreichbar. Zudem stimmt die Slack-Sprache oft nicht mit der gewünschten Bot-Sprache überein.
- **Zuordnung per Umgebungsvariable** (`BOT_USER_LANGUAGES=U123:de,U456:fr`): einfach umzusetzen, aber ohne Selbstbedienung. Jede Änderung braucht einen Neustart durch den Betrieb.
- **Slash-Command** (`/officebot-sprache fr`): für erfahrene Nutzer bequem, braucht aber den Scope `commands` und eine Änderung am Manifest. Wer die Befehle nicht kennt, findet sie schlecht. Kann später zusätzlich kommen.
- **App-Home-Tab mit Einstellungen:** übersichtlich und ausbaufähig. Er braucht aber `home_tab_enabled`, das Event `app_home_opened` und eine eigene Ansicht. Er lohnt sich, sobald es mehrere persönliche Einstellungen gibt. Die Tabelle `user_settings` ist dafür schon vorbereitet.

## Konsequenzen

- Jede Person liest DMs, Erinnerungen und Antworten in ihrer Sprache. Die Übersicht bleibt für alle gleich.
- Neue Sprachen brauchen zusätzlich den Schlüssel `language.name`. Der bestehende Vollständigkeitstest erzwingt das automatisch.
- Ohne eigene Wahl und mit `BOT_LANGUAGE_FROM_SLACK=false` verhält sich der Bot genau wie heute.
- Tests müssen gemischte Sprachen abdecken: zwei Personen mit verschiedenen Sprachen in derselben Abfrage, eine Sprachänderung bei offener DM, eine unbekannte gespeicherte Sprache und die CLI.
