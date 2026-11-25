# kore-plugin-terminal — Plugin de terminal web

## O que faz

Este é o plugin de terminal web do kore (id `terminal`). Cada sessão é um **PTY real com um shell de
login**, não um wrapper de execução de comandos: escrever significa enviar teclas e ler significa obter
a saída do terminal (incluindo sequências de escape), pronto para um front-end como o xterm.js. Desde
a versão 0.2.0, adiciona **permissões por método**, **histórico de comandos persistido por sessão** e
**reconexão** (cursores de leitura retomáveis). Aspectos puramente visuais como tamanho da fonte, tela
cheia, temas e painéis divididos ficam fora deste repositório.

- Protocolo: `kore.plugin.v1`, `lifecycle=resident` (o terminal deve permanecer ativo), nível de profundidade `production`.
- Permissões: `system:read` + `system:admin` + `proc:write`, com `method_capabilities` por método.
- Orçamento de recursos: 12 MiB de memória / 25% de CPU / 64 pids.

## Implementação do PTY (sem bibliotecas de terceiros)

`internal/terminal/pty_linux.go` usa apenas a biblioteca padrão e segue o fluxo fixo do Linux para
abrir um pseudo-terminal: abrir `/dev/ptmx`, desbloquear o escravo (TIOCSPTLCK), obter o número do
escravo (TIOCGPTN) e abrir `/dev/pts/<n>`, iniciar um shell de login com `Setsid` + `Setctty` (de modo
que o id do grupo de processos seja igual ao pid, permitindo `kill(-pgid)` para encerrar toda a árvore
ao fechar) e redimensionar com `TIOCSWINSZ`. O fd mestre é não bloqueante e as leituras/escritas usam
`select` para que uma única colagem não trave o loop. Escopo de plataforma: compila em `linux/amd64`,
`linux/arm64`, `linux/riscv64` e `linux/loong64`; outras plataformas retornam explicitamente
`E_UNSUPPORTED`.

## Métodos do protocolo (kore.plugin.v1)

| Método | Entrada | Descrição |
| --- | --- | --- |
| `kore.ping` | `{}` | Vitalidade e métricas de execução |
| `kore.shutdown` | `{}` | Salvar o histórico, fechar todas as sessões e sair graciosamente |
| `terminal.create` | `{client_id?,cols?,rows?,shell?}` | Padrões 80x24, `/bin/bash`, `default` |
| `terminal.write` | `{session_id,client_id?,data}` | `data` é base64; a entrada é registrada no histórico |
| `terminal.resize` | `{session_id,client_id?,cols,rows}` | Retorna o valor após `TIOCSWINSZ` |
| `terminal.read` | `{session_id,client_id?,cursor?,max_bytes?}` | Leitura incremental; com `cursor`, retoma a partir dele |
| `terminal.attach` | `{session_id,client_id?,cursor?}` | Reconecta uma sessão viva e verifica se o cursor é retomável |
| `terminal.close` | `{session_id,client_id?}` | `kill(-pgid)` + fechar fd + remover do mapa |
| `terminal.list` | `{client_id?}` | Sem: visão administrativa (todas); com: filtrar por proprietário |
| `terminal.history` | `{session_id?,client_id?,limit?}` | Consultar o histórico de comandos (mais recente primeiro) |
| `terminal.history.clear` | `{session_id?,client_id?}` | Limpar o histórico de quem chama |

**Codificação de dados**: `terminal.write.data`, `terminal.read.data` e `terminal.history.entries[].data`
usam todos base64 padrão (com preenchimento `=`) — a saída do terminal pode conter dados binários
arbitrários e sequências UTF-8 multibyte divididas entre quadros; colocá-los diretamente em JSON os
substituiria silenciosamente por U+FFFD. O front-end decodifica uma vez e entrega os bytes a
`term.write(Uint8Array)`. Base64 inválido retorna `E_INVALID`.

## Propriedade da sessão e rejeição de acessos não autorizados

`terminal.create` pode trazer um `client_id` (letras/dígitos/`-_.:@`, <=64 bytes, padrão `default`);
a propriedade é imutável após a criação. `write/read/resize/close/attach/history` devem trazer o mesmo
`client_id`, sob pena de retornar `E_UNAUTHORIZED` sem tocar na sessão do proprietário. **Modelo de
confiança**: `client_id` é um rótulo lógico de propriedade dentro do plugin, não uma credencial de
autenticação; a verdadeira fronteira de autenticação está na autorização `method_capabilities` do
núcleo.

## Reconexão e cursores de leitura

Cada sessão mantém um cursor de bytes absoluto monotonicamente crescente `offset` (bytes entregues),
avançado apenas por `read`. `cursor == offset` é leitura incremental; `cursor < offset` dentro da janela
de reprodução devolve os bytes `[cursor, offset)` (`replay=true`); antes do limite inferior da janela
retorna `E_CONFLICT`, depois retorna `E_INVALID`. A janela `KORE_TERMINAL_REPLAY_BYTES` tem como padrão
64 KiB (máx. 4 MiB) e guarda apenas os bytes entregues mais recentemente; ler o mesmo cursor duas vezes
é idempotente. A sessão vive até fechamento explícito, saída do shell ou recolhimento por inatividade
(padrão 1800s, disparado de forma preguiçosa pelas chamadas, sem temporizador); após o recolhimento não
pode ser retomada — `attach` retorna `E_NOTFOUND`.

## Histórico de comandos

Apenas a entrada de `terminal.write` é registrada, agrupada por sessão e armazenada em
`<KORE_STATE_DIR>/history/session-<id>.json` (diretório 0700, arquivo 0600, escrita atômica). O número
de entradas por sessão (padrão 200), os bytes por entrada (padrão 4096) e as sessões mantidas (padrão
50) são configuráveis; o excedente é removido do mais antigo e entradas grandes demais são truncadas e
marcadas com `truncated`. A persistência é best-effort: falhas não rejeitam a entrada e o erro é
devolvido honestamente por `terminal.history.storage_error`. Com `KORE_TERMINAL_HISTORY=off` ou sem
`KORE_STATE_DIR`, o histórico é desativado com um WARN. **Privacidade**: o histórico pode conter senhas
digitadas pelo usuário; o plugin nunca escreve a entrada em logs, mas colar senhas em texto simples no
terminal continua desaconselhado.

## Segurança e recursos

Limite de sessões padrão 4 (`KORE_TERMINAL_MAX_SESSIONS`, 1-16, exceder → `E_RATELIMIT`); leitura/escrita
única de 1 MiB, orçamento de 5s por requisição; `shell` deve ser um caminho absoluto para um executável
existente; o ambiente do shell filho remove `KORE_PLUGIN_TOKEN` para que credenciais do protocolo não
vazem para o terminal.

Variáveis de ambiente: `KORE_TERMINAL_MAX_SESSIONS` (4), `KORE_TERMINAL_IDLE_SEC` (1800),
`KORE_TERMINAL_SHELL` (`/bin/bash`), `KORE_TERMINAL_REPLAY_BYTES` (65536),
`KORE_TERMINAL_HISTORY` (on), `KORE_TERMINAL_HISTORY_MAX` (200),
`KORE_TERMINAL_HISTORY_ENTRY_BYTES` (4096), `KORE_TERMINAL_HISTORY_SESSIONS` (50).

## Canal de saída: sondagem, não eventos

A conclusão é sondar `terminal.read` (leitura incremental) com `attach`/`cursor` para recuperação, e não
usar eventos push: o protocolo não tem um evento `terminal.output` registrado e o núcleo não encaminha
eventos de plugins para SSE. Um front-end pode sondar a cada 30-80ms ou ler logo após a entrada.

## Instalação e compilação

Módulo Go autônomo que depende apenas de `kore/pkg/pluginapi` e da biblioteca padrão; sem dependências
de terceiros e sem `go.work` versionado. Base de compilação:
`CGO_ENABLED=0 GOWORK=off GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`.
Verificação local: `gofmt -l .`, `GOWORK=off go vet ./...`,
`GOWORK=off go test -count=1 -timeout 200s ./...`, `make dist` (empacotamento reproduzível bit a bit).
Artefatos de lançamento: `dist/terminal-<versão>.tar.gz` + `.sha256`. Não abra uma issue pública para
vulnerabilidades — divulgue-a de forma privada conforme
[SECURITY.md](https://github.com/kore-pl/kore-plugin-terminal/blob/main/SECURITY.md).
