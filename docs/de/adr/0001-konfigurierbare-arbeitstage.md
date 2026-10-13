# ADR 0001: Konfigurierbare Arbeitstage

- **Status:** Vorgeschlagen (für später geplant, nicht umgesetzt)
- **Datum:** 2026-10-13

## Kontext

Der Bot geht fest von einer Arbeitswoche von Montag bis Freitag aus:

- Antworten werden in festen Spalten `monday` … `friday` gespeichert (`internal/storage/migrations/0001_init.sql`, `domain.Days` mit `NumDays = 5`).
- Eine Woche wird über ISO-Jahr und ISO-Kalenderwoche identifiziert. `WeekStart` ist der Montag, `WeekEnd` der Freitag.
- Checkboxen, Übersicht, Datumsbereich und Erinnerungen richten sich nach Mo–Fr.

Für Teams in Deutschland und den USA passt das, denn dort wird ebenfalls von Montag bis Freitag gearbeitet. Der Sonntag steht im US-Kalender nur vorne. Nicht abbilden lassen sich dagegen:

- Arbeitswochen von Sonntag bis Donnerstag (z. B. Israel, Saudi-Arabien),
- Büros, die nur an einzelnen Tagen genutzt werden (z. B. Di–Do),
- 4-Tage-Wochen.

Welche Woche abgefragt wird, ergibt sich aus `POLL_WEEKDAY`: Eine Abfrage am Montag betrifft die laufende Woche, eine Abfrage an jedem anderen Tag die kommende. Die Regel nimmt den Montag als ersten Tag der Arbeitswoche an.

## Entscheidung (geplant)

Die Arbeitstage werden über eine Liste konfigurierbar statt nur über einen Starttag:

```dotenv
WORK_DAYS=MONDAY,TUESDAY,WEDNESDAY,THURSDAY,FRIDAY   # Standard
# WORK_DAYS=SUNDAY,MONDAY,TUESDAY,WEDNESDAY,THURSDAY
# WORK_DAYS=TUESDAY,WEDNESDAY,THURSDAY
```

- Der **erste Tag der Liste** beginnt die Arbeitswoche. Die heutige Regel wird dadurch allgemeiner: Eine Abfrage am ersten Arbeitstag gilt für die laufende Woche, an jedem anderen Tag für die nächste Woche, die mit diesem Tag beginnt.
- Die Liste ist in Wochenreihenfolge ab dem ersten Tag anzugeben. Sie darf nicht leer sein und keine Tage doppelt enthalten.
- Ohne `WORK_DAYS` verhält sich der Bot genau wie heute.

### Nötige Änderungen

1. **Woche als Einheit:** Eine Woche wird über das Datum ihres ersten Arbeitstags identifiziert (`week_start` als lokales Datum) statt über ISO-Jahr/KW. Nur so werden Wochen, die am Sonntag beginnen, und Wochen über den Jahreswechsel eindeutig. ISO-KW bleibt als Anzeige erhalten, wo sie sinnvoll ist.
2. **Schema:** Die Spalten `monday` … `friday` weichen einer Bitmaske über die Wochentage (`days INTEGER`, Bit 0 = Sonntag … Bit 6 = Samstag). Eine Migration überführt die vorhandenen Antworten verlustfrei. Die Bitmaske hängt am Wochentag und nicht an der Position in der Liste, damit eine spätere Änderung von `WORK_DAYS` alte Antworten nicht verschiebt.
3. **Domäne:** `domain.Days` wird zu einer Menge von `time.Weekday`. `NumDays`, `DayKeys` und `DayWeekdays` ergeben sich aus der Konfiguration.
4. **Darstellung:** Checkboxen, Abschnitte der Übersicht, Datumsbereich (erster bis letzter Arbeitstag) und Kurzlisten verwenden die konfigurierten Tage. Wochentagsnamen für alle sieben Tage gibt es bereits in den Übersetzungsdateien.
5. **Frist und Erinnerungen:** `POLL_DEADLINE_WEEKDAY` und `REMINDER_WEEKDAY` beziehen sich auf die Woche ab ihrem ersten Arbeitstag statt ab Montag. Die Prüfung „Frist liegt nach dem Start“ bleibt.
6. **Abwesenheiten:** „Die ganze Woche abwesend“ (`domain.AwayAllWeek`) prüft heute Montag bis Freitag der abgefragten Woche. Künftig prüft es die konfigurierten Arbeitstage. Die gespeicherten Abwesenheiten (Kalendertage von–bis) bleiben unverändert.
7. **Interaktionen:** Die Checkbox-Werte bleiben stabile Schlüssel (`mon` … `sun`). Werte, die nicht in `WORK_DAYS` vorkommen, werden wie heute abgelehnt.

### Zusammenhang mit der Internationalisierung

Die Slack-Texte sind bereits übersetzbar (`BOT_LANGUAGE`, `internal/i18n/locales/*.json`). Für Teams außerhalb Deutschlands kommen dazu:

- **Wochenbezeichnung:** ISO-Kalenderwochen sind in vielen Ländern unüblich. Die Übersetzung sollte wählen können zwischen „KW 43“ und „Week of Oct 19“.
- **Formate:** Die Datums- und Zeit-Layouts liegen bereits pro Sprache vor. Langfristig kann eine Locale (z. B. `en-US` vs. `en-GB`) Sprache und Format trennen.

Diese Punkte sind unabhängig von `WORK_DAYS` und können getrennt umgesetzt werden.

## Betrachtete Alternativen

- **Nur Mo–Fr beibehalten:** keine Arbeit, aber Teams mit anderer Arbeitswoche können den Bot nicht nutzen.
- **Nur einen Starttag konfigurieren (`WORK_WEEK_START`):** einfacher, legt aber fünf aufeinanderfolgende Tage fest. Büros mit Di–Do oder 4-Tage-Wochen gehen damit nicht. Die Liste deckt den Starttag mit ab.
- **Positionen statt Wochentage speichern (Index 0 … n):** einfacheres Schema, aber eine Änderung von `WORK_DAYS` würde vorhandene Antworten falschen Tagen zuordnen.

## Konsequenzen

- Größerer Umbau über Schema, Domäne, Darstellung und Scheduler, mit einer Datenmigration. Er sollte erst angegangen werden, wenn es konkrete Nutzer mit abweichender Arbeitswoche gibt.
- Die jetzt eingeführte Wochenregel (erster Arbeitstag = laufende Woche, sonst kommende) und die Übersetzungsdateien sind so gebaut, dass sie ohne Bruch erweitert werden können.
- Bestehende Installationen ohne `WORK_DAYS` merken nach der Migration keinen Unterschied.
