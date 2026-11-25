# kore-plugin-terminal — Plugin de terminal web

## Qué hace

Este es el plugin de terminal web de kore (id `terminal`). Cada sesión es un **PTY real con un shell
de inicio de sesión**, no un envoltorio de ejecución de comandos: escribir equivale a enviar teclas y
leer equivale a obtener la salida del terminal (incluidas las secuencias de escape), listo para un
front-end como xterm.js. Desde 0.2.0 añade **capacidades por método**, **historial de comandos
persistente por sesión** y **reconexión** (cursores de lectura reanudables). Los aspectos puramente
visuales como tamaño de fuente, pantalla completa, temas y paneles divididos quedan fuera de este
repositorio.

- Protocolo: `kore.plugin.v1`, `lifecycle=resident` (el terminal debe permanecer activo), nivel de profundidad `production`.
- Capacidades: `system:read` + `system:admin` + `proc:write`, con `method_capabilities` por método.
- Presupuesto de recursos: 12 MiB de memoria / 25% de CPU / 64 pids.

## Implementación del PTY (sin bibliotecas de terceros)

`internal/terminal/pty_linux.go` usa solo la biblioteca estándar y sigue el flujo fijo de Linux para
abrir un pseudo-terminal: abrir `/dev/ptmx`, desbloquear el esclavo (TIOCSPTLCK), obtener el número
del esclavo (TIOCGPTN) y abrir `/dev/pts/<n>`, iniciar un shell de inicio de sesión con `Setsid` +
`Setctty` (de modo que el id de grupo de proceso sea igual al pid, permitiendo `kill(-pgid)` para
terminar todo el árbol al cerrar) y ajustar el tamaño con `TIOCSWINSZ`. El fd maestro es no bloqueante
y las lecturas/escrituras usan `select` para que un solo pegado no bloquee el bucle. Ámbito de
plataforma: compila en `linux/amd64`, `linux/arm64`, `linux/riscv64` y `linux/loong64`; otras
plataformas devuelven explícitamente `E_UNSUPPORTED`.

## Métodos del protocolo (kore.plugin.v1)

| Método | Entrada | Descripción |
| --- | --- | --- |
| `kore.ping` | `{}` | Vivacidad y métricas de ejecución |
| `kore.shutdown` | `{}` | Guarda el historial, cierra todas las sesiones y sale con gracia |
| `terminal.create` | `{client_id?,cols?,rows?,shell?}` | Valores por defecto 80x24, `/bin/bash`, `default` |
| `terminal.write` | `{session_id,client_id?,data}` | `data` es base64; la entrada se registra en el historial |
| `terminal.resize` | `{session_id,client_id?,cols,rows}` | Devuelve el valor tras aplicar `TIOCSWINSZ` |
| `terminal.read` | `{session_id,client_id?,cursor?,max_bytes?}` | Lectura incremental; con `cursor` reanuda desde él |
| `terminal.attach` | `{session_id,client_id?,cursor?}` | Reconecta una sesión viva y verifica que el cursor se pueda reanudar |
| `terminal.close` | `{session_id,client_id?}` | `kill(-pgid)` + cerrar fd + eliminar del mapa |
| `terminal.list` | `{client_id?}` | Sin él: vista de administración (todas); con él: filtrar por propietario |
| `terminal.history` | `{session_id?,client_id?,limit?}` | Consultar el historial de comandos (más reciente primero) |
| `terminal.history.clear` | `{session_id?,client_id?}` | Borrar el historial de quien llama |

**Codificación de datos**: `terminal.write.data`, `terminal.read.data` y
`terminal.history.entries[].data` usan todos base64 estándar (con relleno `=`): la salida del
terminal puede contener datos binarios arbitrarios y secuencias UTF-8 multibyte divididas entre
tramas; ponerlos directamente en JSON los reemplazaría silenciosamente por U+FFFD. El front-end
decodifica una vez y entrega los bytes a `term.write(Uint8Array)`. El base64 inválido devuelve `E_INVALID`.

## Propiedad de la sesión y rechazo de accesos no autorizados

`terminal.create` puede llevar un `client_id` (letras/dígitos/`-_.:@`, <=64 bytes, por defecto
`default`); la propiedad es inmutable tras la creación. `write/read/resize/close/attach/history`
deben llevar el mismo `client_id` o devuelven `E_UNAUTHORIZED` sin tocar la sesión del propietario.
**Modelo de confianza**: `client_id` es una etiqueta lógica de propiedad dentro del plugin, no una
credencial de autenticación; el límite real de autenticación está en la autorización
`method_capabilities` del núcleo.

## Reconexión y cursores de lectura

Cada sesión mantiene un cursor de bytes absoluto monótonamente creciente `offset` (bytes entregados),
avanzado solo por `read`. `cursor == offset` es una lectura incremental; `cursor < offset` dentro de la
ventana de reproducción reproduce los bytes `[cursor, offset)` (`replay=true`); anterior al límite
inferior devuelve `E_CONFLICT` y posterior devuelve `E_INVALID`. La ventana
`KORE_TERMINAL_REPLAY_BYTES` por defecto es 64 KiB (máx. 4 MiB) y conserva solo los bytes entregados
más recientemente; leer dos veces el mismo cursor es idempotente. La sesión vive hasta el cierre
explícito, la salida del shell o la recolección por inactividad (1800s por defecto, disparada
perezosamente por las llamadas, sin temporizador); tras la recolección no se puede reanudar y
`attach` devuelve `E_NOTFOUND`.

## Historial de comandos

Solo se registra la entrada de `terminal.write`, agrupada por sesión y almacenada en
`<KORE_STATE_DIR>/history/session-<id>.json` (directorio 0700, archivo 0600, escritura atómica). El
número de entradas por sesión (200 por defecto), los bytes por entrada (4096 por defecto) y las
sesiones conservadas (50 por defecto) son configurables; el exceso se elimina del más antiguo y las
entradas demasiado grandes se truncan marcándose `truncated`. El guardado es best-effort: los fallos
no rechazan la entrada del terminal y el error se devuelve honestamente mediante
`terminal.history.storage_error`. Con `KORE_TERMINAL_HISTORY=off` o sin `KORE_STATE_DIR`, el historial
se desactiva con un WARN. **Privacidad**: el historial puede contener contraseñas escritas por el
usuario; el plugin nunca escribe la entrada en los registros, pero se desaconseja pegar contraseñas
en texto plano en el terminal.

## Seguridad y recursos

Límite de sesiones 4 por defecto (`KORE_TERMINAL_MAX_SESSIONS`, 1-16, excederlo devuelve
`E_RATELIMIT`); lectura/escritura única de 1 MiB, presupuesto de 5s por solicitud; `shell` debe ser
una ruta absoluta a un ejecutable existente; el entorno del shell hijo elimina `KORE_PLUGIN_TOKEN`
para que las credenciales del protocolo no se filtren al terminal.

Variables de entorno: `KORE_TERMINAL_MAX_SESSIONS` (4), `KORE_TERMINAL_IDLE_SEC` (1800),
`KORE_TERMINAL_SHELL` (`/bin/bash`), `KORE_TERMINAL_REPLAY_BYTES` (65536),
`KORE_TERMINAL_HISTORY` (on), `KORE_TERMINAL_HISTORY_MAX` (200),
`KORE_TERMINAL_HISTORY_ENTRY_BYTES` (4096), `KORE_TERMINAL_HISTORY_SESSIONS` (50).

## Canal de salida: sondeo, no eventos

La conclusión es sondear `terminal.read` (lectura incremental) junto con `attach`/`cursor` para la
recuperación, no usar eventos push: el protocolo no tiene registrado el evento `terminal.output` y el
núcleo no reenvía los eventos del plugin a SSE. Un front-end puede sondear cada 30-80ms o leer justo
después de la entrada.

## Instalación y compilación

Módulo Go independiente que solo depende de `kore/pkg/pluginapi` y la biblioteca estándar; sin
dependencias de terceros y sin `go.work` versionado. Línea de base de compilación:
`CGO_ENABLED=0 GOWORK=off GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`.
Verificación local: `gofmt -l .`, `GOWORK=off go vet ./...`,
`GOWORK=off go test -count=1 -timeout 200s ./...`, `make dist` (empaquetado reproducible bit a bit).
Artefactos de lanzamiento: `dist/terminal-<versión>.tar.gz` + `.sha256`. No abras un issue público
para vulnerabilidades; divúlgala de forma privada según
[SECURITY.md](https://github.com/kore-pl/kore-plugin-terminal/blob/main/SECURITY.md).
