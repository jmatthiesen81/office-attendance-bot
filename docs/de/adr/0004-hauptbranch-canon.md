# ADR 0004: Hauptbranch `canon` statt Versionsbranches

- **Status:** Angenommen
- **Datum:** 2026-11-19

## Kontext

Bisher wurde auf einem Versionsbranch entwickelt, der nach der Release-Linie benannt war (`1.0`). Er war auf GitHub der Default-Branch, und jeder Push darauf veröffentlichte das Image `edge`. Für jedes neue Feature-Release (1.1, 1.2, 1.3) hätte ein neuer Branch `1.1`, `1.2`, … angelegt und der Default-Branch umgestellt werden müssen. Das ist nie passiert, also trägt `1.0` seit 1.1.0 alle Releases, und sein Name führt in die Irre.

Die Version ist ohnehin vollständig durch den Git-Tag festgelegt: `1.3.1` veröffentlicht die Image-Tags `1.3.1`, `1.3`, `1` und `latest`. Versionsbranches lohnen sich nur, wenn mehrere Release-Linien parallel gepflegt werden, etwa Fixes für 1.2.x, während 1.3 aktuell ist. Das ist hier nicht der Fall: Es gibt eine Produktivinstanz, und die folgt `:1`.

## Entscheidung

Das Projekt arbeitet trunk-basiert mit genau einem langlebigen Branch namens **`canon`**.

- **`canon`** ist der Default-Branch. Jeder Push veröffentlicht `:edge` und `:sha-<commit>` zum Testen im Live-Betrieb.
- **Feature-Branches** (`feat/<thema>`, `fix/<thema>`, …) zweigen von `canon` ab und werden wieder in `canon` gemergt.
- **Releases** sind nur Git-Tags auf `canon` (`1.4.0`, `1.4.1`, …). Für ein Release wird kein Branch angelegt.
- **Alte Release-Linien** werden nur gepflegt, wenn es wirklich nötig ist. Der Branch entsteht dann aus dem Tag: `git switch -c 1.3 1.3.1`, Fix committen, `1.3.2` taggen. Solche Branches veröffentlichen kein `:edge`.

### Warum `canon`

Der Name ist bewusst individuell statt `main`, `master` oder `trunk`. Die Metapher passt zum Ablauf: Feature-Branches sind Fanfiction, also Ideen, die noch nicht offiziell sind. Erst der Merge macht sie zum Kanon, und ein Release-Tag ist eine veröffentlichte Ausgabe des Kanons. Außerdem klingt „canonical“ an, die maßgebliche Quelle. Der Name ist also auch ohne den Witz verständlich.

Verworfen: `origin` und `HEAD` (in Git schon belegt), Namen, die wie Versionen aussehen (sie würden mit den Tag-Mustern kollidieren), `edge` (würde Branch und Image-Tag vermischen).

## Folgen

- Für ein Feature-Release muss der Default-Branch nicht mehr umgestellt werden.
- `release.yml` reagiert auf `canon` statt auf das Muster `[0-9]+.[0-9]+`.
- Dependabot hat kein `target-branch` und nimmt automatisch den Default-Branch.
- Wer schon einen Klon hat, muss den lokalen Branch einmal umbenennen (siehe unten).

## Umstellung (für andere Projekte wiederverwendbar)

1. **Repository vorbereiten**, in einem Feature-Branch, der in den alten Default-Branch gemergt wird:
   - CI-Trigger: Branch-Muster durch `canon` ersetzen (GitHub Actions: `on.push.branches`; GitLab CI: `rules`/`only` mit `$CI_DEFAULT_BRANCH` oder `canon`).
   - Nach dem alten Branch-Namen suchen: `git grep -nwE "<alt>|main|master"` in Workflows, `dependabot.yml` (`target-branch`), Renovate-Konfiguration, Makefile, Skripten, Badges und Doku.
2. **Branch auf dem Server umbenennen:**
   - **GitHub:** *Settings → General → Default branch*, Stift-Symbol neben dem Branch, in `canon` umbenennen. GitHub stellt offene Pull Requests um, passt Branch-Protection-Regeln an, die den Branch ausdrücklich nennen, und leitet alte Links weiter. Rulesets mit Mustern von Hand prüfen.
   - **GitLab** (kann nicht umbenennen): `git push origin <alt>:refs/heads/canon`, dann unter *Settings → Repository → Branch defaults* `canon` als Default setzen, die Einstellungen für geschützte Branches auf `canon` übertragen, offene Merge Requests umstellen und danach den alten Branch löschen.
3. **Jeden lokalen Klon anpassen:**
   ```sh
   git branch -m <alt> canon
   git fetch origin --prune
   git branch -u origin/canon canon
   git remote set-head origin -a
   ```
4. **Prüfen:** einen Commit auf `canon` pushen und kontrollieren, dass CI `:edge` veröffentlicht. Das nächste Release ist ein Tag auf `canon`.
