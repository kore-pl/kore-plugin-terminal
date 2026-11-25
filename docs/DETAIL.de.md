# kore-plugin-terminal — Web-Terminal-Plugin

## Was es macht

Dies ist kores Web-Terminal-Plugin (id `terminal`). Jede Sitzung ist ein **echtes PTY mit einer
Login-Shell**, kein Wrapper für die Ausführung von Befehlen: Schreiben bedeutet Tastatureingaben,
Lesen bedeutet Terminalausgabe (einschließlich Escape-Sequenzen), bereit für ein Frontend wie
xterm.js. Seit 0.2.0 bietet es **Methoden-Capabilities**, einen **pro Sitzung persistierten
Befehlsverlauf** und **Wiederverbindung** (fortsetzbare Lesecursor). Reine Darstellungsaspekte wie
Schriftgröße, Vollbild, Themes und geteilte Fenster liegen außerhalb dieses Repositories.

- Protokoll: `kore.plugin.v1`, `lifecycle=resident` (das Terminal muss aktiv bleiben), Tiefe `production`.
- Capabilities: `system:read` + `system:admin` + `proc:write`, mit `method_capabilities` pro Methode.
- Ressourcenbudget: 12 MiB Speicher / 25% CPU / 64 pids.

## PTY-Implementierung (ohne Fremdbibliotheken)

`internal/terminal/pty_linux.go` nutzt nur die Standardbibliothek und folgt dem festen Linux-Ablauf
zum Öffnen eines Pseudo-Terminals: `/dev/ptmx` öffnen, den Slave entsperren (TIOCSPTLCK), die
Slave-Nummer holen (TIOCGPTN) und `/dev/pts/<n>` öffnen, eine Login-Shell mit `Setsid` + `Setctty`
starten (so dass die Prozessgruppen-ID der pid entspricht und `kill(-pgid)` beim Schließen den
gesamten Baum beenden kann) und mit `TIOCSWINSZ` die Fenstergröße anpassen. Der Master-fd ist
nicht blockierend, Lese-/Schreibzugriffe nutzen `select`, damit ein einzelnes Einfügen die Schleife
nicht blockiert. Plattformumfang: Baut auf `linux/amd64`, `linux/arm64`, `linux/riscv64` und
`linux/loong64`; andere Plattformen geben explizit `E_UNSUPPORTED` zurück.

## Protokollmethoden (kore.plugin.v1)

| Methode | Eingabe | Beschreibung |
| --- | --- | --- |
| `kore.ping` | `{}` | Lebenszeichen und Laufzeitmetriken |
| `kore.shutdown` | `{}` | Verlauf schreiben, alle Sitzungen schließen, sauber beenden |
| `terminal.create` | `{client_id?,cols?,rows?,shell?}` | Standard 80x24, `/bin/bash`, `default` |
| `terminal.write` | `{session_id,client_id?,data}` | `data` ist base64; Eingabe wird im Verlauf erfasst |
| `terminal.resize` | `{session_id,client_id?,cols,rows}` | Gibt den Wert nach `TIOCSWINSZ` zurück |
| `terminal.read` | `{session_id,client_id?,cursor?,max_bytes?}` | Inkrementelles Lesen; mit `cursor` ab dort fortsetzen |
| `terminal.attach` | `{session_id,client_id?,cursor?}` | Aktive Sitzung wieder verbinden, Cursor-Fortsetzbarkeit prüfen |
| `terminal.close` | `{session_id,client_id?}` | `kill(-pgid)` + fd schließen + aus Map entfernen |
| `terminal.list` | `{client_id?}` | Ohne: Admin-Ansicht (alle); mit: nach Besitzer filtern |
| `terminal.history` | `{session_id?,client_id?,limit?}` | Befehlsverlauf abfragen (neueste zuerst) |
| `terminal.history.clear` | `{session_id?,client_id?}` | Verlauf des Aufrufers löschen |

**Datenkodierung**: `terminal.write.data`, `terminal.read.data` und
`terminal.history.entries[].data` verwenden alle Standard-Base64 (mit `=`-Padding) — die
Terminalausgabe kann beliebige Binärdaten und über Frames geteilte multibyte UTF-8-Sequenzen
enthalten; direkt in JSON wären diese stillschweigend durch U+FFFD ersetzt worden. Das Frontend
dekodiert einmal und übergibt die Bytes an `term.write(Uint8Array)`. Ungültiges Base64 liefert
`E_INVALID`.

## Sitzungsbesitz und Ablehnung unbefugter Zugriffe

`terminal.create` kann ein `client_id` tragen (Buchstaben/Ziffern/`-_.:@`, <=64 Bytes, Standard
`default`); der Besitz ist nach der Erstellung unveränderlich. `write/read/resize/close/attach/history`
müssen dasselbe `client_id` tragen, sonst liefern sie `E_UNAUTHORIZED`, ohne die Sitzung des
Besitzers zu berühren. **Vertrauensmodell**: `client_id` ist eine logische Besitzmarkierung im
Plugin, kein Authentifizierungsmerkmal; die eigentliche Authentifizierungsgrenze ist die
`method_capabilities`-Autorisierung des Kerns.

## Wiederverbindung und Lesecursor

Jede Sitzung führt einen monoton steigenden absoluten Byte-Cursor `offset` (gelieferte Bytes), der nur
durch `read` vorgeschoben wird. `cursor == offset` ist inkrementelles Lesen; `cursor < offset` innerhalb
des Replay-Fensters gibt die Bytes `[cursor, offset)` zurück (`replay=true`); vor der unteren
Fenstergrenze liefert es `E_CONFLICT`, dahinter `E_INVALID`. Das Fenster `KORE_TERMINAL_REPLAY_BYTES`
beträgt standardmäßig 64 KiB (max. 4 MiB) und bewahrt nur die zuletzt gelieferten Bytes; mehrfaches
Lesen desselben Cursors ist idempotent. Die Sitzung lebt bis zum expliziten Schließen, dem Exit der
Shell oder der Leerlaufbereinigung (standardmäßig 1800s, träge durch Aufrufe ausgelöst, ohne Timer);
nach der Bereinigung ist sie nicht wiederherstellbar — `attach` liefert `E_NOTFOUND`.

## Befehlsverlauf

Nur die Eingabe von `terminal.write` wird pro Sitzung erfasst und unter
`<KORE_STATE_DIR>/history/session-<id>.json` gespeichert (Verzeichnis 0700, Datei 0600, atomares
Schreiben). Einträge pro Sitzung (Standard 200), Bytes pro Eintrag (Standard 4096) und behaltene
Sitzungen (Standard 50) sind konfigurierbar; Überschüsse werden von den ältesten entfernten, zu große
Einträge gekürzt und mit `truncated` markiert. Das Speichern ist best-effort: Fehler lehnen die
Eingabe nicht ab und werden ehrlich über `terminal.history.storage_error` zurückgegeben. Mit
`KORE_TERMINAL_HISTORY=off` oder fehlendem `KORE_STATE_DIR` wird der Verlauf mit einer WARN deaktiviert.
**Datenschutz**: Der Verlauf kann vom Benutzer eingegebene Passwörter enthalten; das Plugin schreibt
Eingaben nie in Logs, das Einfügen von Klartext-Passwörtern ins Terminal bleibt dennoch nicht empfohlen.

## Sicherheit und Ressourcen

Sitzungslimit standardmäßig 4 (`KORE_TERMINAL_MAX_SESSIONS`, 1-16, Überschreitung → `E_RATELIMIT`);
einzelne Lese-/Schreiboperation 1 MiB, 5s-Budget pro Anfrage; `shell` muss ein absoluter Pfad zu einer
vorhandenen ausführbaren Datei sein; die Umgebung der Kind-Shell entfernt `KORE_PLUGIN_TOKEN`, damit
Protokoll-Zugangsdaten nicht ins Terminal gelangen.

Umgebungsvariablen: `KORE_TERMINAL_MAX_SESSIONS` (4), `KORE_TERMINAL_IDLE_SEC` (1800),
`KORE_TERMINAL_SHELL` (`/bin/bash`), `KORE_TERMINAL_REPLAY_BYTES` (65536),
`KORE_TERMINAL_HISTORY` (on), `KORE_TERMINAL_HISTORY_MAX` (200),
`KORE_TERMINAL_HISTORY_ENTRY_BYTES` (4096), `KORE_TERMINAL_HISTORY_SESSIONS` (50).

## Ausgabekanal: Polling, keine Events

Die Schlussfolgerung lautet: `terminal.read` (inkrementelles Lesen) plus `attach`/`cursor` zur
Wiederherstellung abfragen, kein Event-Push: Das Protokoll kennt kein registriertes
`terminal.output`-Event und der Kern leitet Plugin-Events nicht an SSE weiter. Ein Frontend kann alle
30-80ms abfragen oder direkt nach einer Eingabe lesen.

## Installation und Build

Ein eigenständiges Go-Modul, das nur von `kore/pkg/pluginapi` und der Standardbibliothek abhängt;
keine Fremdabhängigkeiten und kein versioniertes `go.work`. Build-Baseline:
`CGO_ENABLED=0 GOWORK=off GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`.
Lokale Prüfung: `gofmt -l .`, `GOWORK=off go vet ./...`,
`GOWORK=off go test -count=1 -timeout 200s ./...`, `make dist` (bitgenau reproduzierbares Packaging).
Release-Artefakte: `dist/terminal-<Version>.tar.gz` + `.sha256`. Schwachstellen nicht in einem
öffentlichen Issue melden — vertraulich gemäß
[SECURITY.md](https://github.com/kore-pl/kore-plugin-terminal/blob/main/SECURITY.md) offenlegen.
