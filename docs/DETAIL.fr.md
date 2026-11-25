# kore-plugin-terminal — Plugin de terminal Web

## Ce que fait ce plugin

Ceci est le plugin de terminal Web de kore (id `terminal`). Chaque session est un **vrai PTY avec un
shell de connexion**, et non un simple wrapper d'exécution de commandes : écrire revient à frapper au
clavier et lire revient à récupérer la sortie du terminal (séquences d'échappement comprises), prêt
pour un front-end tel que xterm.js. Depuis 0.2.0, il ajoute les **capacités par méthode**,
**l'historique des commandes persistant par session** et la **reconnexion** (curseurs de lecture
reprenables). Les aspects purement visuels comme la taille de police, le plein écran, les thèmes et
les panneaux fractionnés ne relèvent pas de ce dépôt.

- Protocole : `kore.plugin.v1`, `lifecycle=resident` (le terminal doit rester actif), niveau de profondeur `production`.
- Capacités : `system:read` + `system:admin` + `proc:write`, avec `method_capabilities` par méthode.
- Budget de ressources : 12 Mio de mémoire / 25% CPU / 64 pids.

## Implémentation du PTY (sans bibliothèque tierce)

`internal/terminal/pty_linux.go` n'utilise que la bibliothèque standard et suit le flux Linux fixe
pour ouvrir un pseudo-terminal : ouvrir `/dev/ptmx`, déverrouiller l'esclave (TIOCSPTLCK), obtenir le
numéro de l'esclave (TIOCGPTN) et ouvrir `/dev/pts/<n>`, démarrer un shell de connexion avec `Setsid`
+ `Setctty` (de sorte que l'identifiant du groupe de processus égale le pid, ce qui permet
`kill(-pgid)` pour tuer tout l'arbre à la fermeture), redimensionner avec `TIOCSWINSZ`. Le fd maître
est non bloquant et les lectures/écritures utilisent `select` pour qu'un simple collage ne bloque pas
la boucle. Périmètre de plateforme : compile sur `linux/amd64`, `linux/arm64`, `linux/riscv64` et
`linux/loong64` ; les autres plateformes renvoient explicitement `E_UNSUPPORTED`.

## Méthodes du protocole (kore.plugin.v1)

| Méthode | Entrée | Description |
| --- | --- | --- |
| `kore.ping` | `{}` | Vivacité et métriques d'exécution |
| `kore.shutdown` | `{}` | Écrit l'historique, ferme toutes les sessions et quitte proprement |
| `terminal.create` | `{client_id?,cols?,rows?,shell?}` | Valeurs par défaut 80x24, `/bin/bash`, `default` |
| `terminal.write` | `{session_id,client_id?,data}` | `data` est en base64 ; la saisie est enregistrée dans l'historique |
| `terminal.resize` | `{session_id,client_id?,cols,rows}` | Renvoie la valeur après application de `TIOCSWINSZ` |
| `terminal.read` | `{session_id,client_id?,cursor?,max_bytes?}` | Lecture incrémentale ; avec `cursor`, reprend depuis celui-ci |
| `terminal.attach` | `{session_id,client_id?,cursor?}` | Reconnecte une session vivante et vérifie que le curseur est reprenable |
| `terminal.close` | `{session_id,client_id?}` | `kill(-pgid)` + fermeture du fd + retrait de la map |
| `terminal.list` | `{client_id?}` | Sans : vue d'administration (toutes) ; avec : filtre par propriétaire |
| `terminal.history` | `{session_id?,client_id?,limit?}` | Consulter l'historique des commandes (plus récent d'abord) |
| `terminal.history.clear` | `{session_id?,client_id?}` | Effacer l'historique de l'appelant |

**Encodage des données** : `terminal.write.data`, `terminal.read.data` et
`terminal.history.entries[].data` utilisent tous le base64 standard (avec remplissage `=`) : la sortie
du terminal peut contenir des données binaires arbitraires et des séquences UTF-8 multioctet coupées
entre deux trames ; les placer directement en JSON les remplacerait silencieusement par U+FFFD. Le
front-end décode une fois et transmet les octets à `term.write(Uint8Array)`. Un base64 invalide
renvoie `E_INVALID`.

## Propriété de la session et rejet des accès non autorisés

`terminal.create` peut porter un `client_id` (lettres/chiffres/`-_.:@`, <=64 octets, par défaut
`default`) ; la propriété est immuable après création. `write/read/resize/close/attach/history`
doivent porter le même `client_id`, sinon ils renvoient `E_UNAUTHORIZED` sans toucher à la session du
propriétaire. **Modèle de confiance** : `client_id` est une étiquette logique de propriété dans le
plugin, pas une authentification ; la vraie frontière d'authentification est l'autorisation
`method_capabilities` du noyau.

## Reconnexion et curseurs de lecture

Chaque session maintient un curseur d'octets absolu croissant `offset` (octets délivrés), avancé
uniquement par `read`. `cursor == offset` est une lecture incrémentale ; `cursor < offset` dans la
fenêtre de relecture rejoue les octets `[cursor, offset)` (`replay=true`) ; avant la limite basse de la
fenêtre renvoie `E_CONFLICT`, au-delà renvoie `E_INVALID`. La fenêtre `KORE_TERMINAL_REPLAY_BYTES`
vaut 64 Kio par défaut (max 4 Mio) et ne conserve que les octets délivrés les plus récents ; lire deux
fois le même curseur est idempotent. La session vit jusqu'à la fermeture explicite, la sortie du shell
ou la réclamation pour inactivité (1800s par défaut, déclenchée paresseusement par les appels, sans
temporisateur) ; après réclamation elle ne peut plus être reprise — `attach` renvoie `E_NOTFOUND`.

## Historique des commandes

Seule l'entrée de `terminal.write` est enregistrée, groupée par session, dans
`<KORE_STATE_DIR>/history/session-<id>.json` (répertoire 0700, fichier 0600, écriture atomique). Le
nombre d'entrées par session (200 par défaut), les octets par entrée (4096 par défaut) et les sessions
conservées (50 par défaut) sont configurables ; l'excédent est éliminé du plus ancien et les entrées
trop grandes sont tronquées avec `truncated`. La persistance est best-effort : les échecs ne rejettent
pas la saisie et l'erreur est renvoyée honnêtement via `terminal.history.storage_error`. Avec
`KORE_TERMINAL_HISTORY=off` ou un `KORE_STATE_DIR` absent, l'historique est désactivé avec un WARN.
**Confidentialité** : l'historique peut contenir des mots de passe saisis par l'utilisateur ; le plugin
n'écrit jamais la saisie dans les journaux, mais coller des mots de passe en clair dans le terminal
reste déconseillé.

## Sécurité et ressources

Limite de sessions 4 par défaut (`KORE_TERMINAL_MAX_SESSIONS`, 1-16, dépassement → `E_RATELIMIT`) ;
lecture/écriture unique de 1 Mio, budget de 5s par requête ; `shell` doit être un chemin absolu vers
un exécutable existant ; l'environnement du shell enfant retire `KORE_PLUGIN_TOKEN` pour éviter que
les identifiants du protocole ne fuient dans le terminal.

Variables d'environnement : `KORE_TERMINAL_MAX_SESSIONS` (4), `KORE_TERMINAL_IDLE_SEC` (1800),
`KORE_TERMINAL_SHELL` (`/bin/bash`), `KORE_TERMINAL_REPLAY_BYTES` (65536),
`KORE_TERMINAL_HISTORY` (on), `KORE_TERMINAL_HISTORY_MAX` (200),
`KORE_TERMINAL_HISTORY_ENTRY_BYTES` (4096), `KORE_TERMINAL_HISTORY_SESSIONS` (50).

## Canal de sortie : sondage, pas d'événements

La conclusion est de sonder `terminal.read` (lecture incrémentale) avec `attach`/`cursor` pour la
reprise, et non d'utiliser des événements push : le protocole n'a pas enregistré d'événement
`terminal.output` et le noyau ne transmet pas les événements du plugin à SSE. Un front-end peut sonder
toutes les 30-80ms ou lire juste après une saisie.

## Installation et compilation

Module Go autonome qui ne dépend que de `kore/pkg/pluginapi` et de la bibliothèque standard ; aucune
dépendance tierce et aucun `go.work` versionné. Ligne de base de compilation :
`CGO_ENABLED=0 GOWORK=off GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`.
Vérification locale : `gofmt -l .`, `GOWORK=off go vet ./...`,
`GOWORK=off go test -count=1 -timeout 200s ./...`, `make dist` (empaquetage reproductible au bit près).
Artefacts de publication : `dist/terminal-<version>.tar.gz` + `.sha256`. N'ouvrez pas d'issue
publique pour une vulnérabilité — divulguez-la privément selon
[SECURITY.md](https://github.com/kore-pl/kore-plugin-terminal/blob/main/SECURITY.md).
