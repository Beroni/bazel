# bazel

Interface web que junta os pull requests abertos dos repositórios que você
escolher — os seus e os dos seus colegas — e dispara agentes de IA para
revisá-los.


Você marca os PRs, escolhe o agente, e acompanha cada lente trabalhando no
próprio terminal. Terminado, lê o review na tela e decide se ele vai para o PR.

Subir o `bazel` dispara a animação de um ovo-bomba do Bazelgeuse que racha,
esquenta e detona no logo. Desliga com `--no-splash` ou `BAZEL_NO_SPLASH=1`.

## Requisitos

- Go 1.25+ (para compilar)
- [`gh`](https://cli.github.com) autenticado (`gh auth login`)
- `git`
- Um agente de IA no PATH que aceite um prompt no stdin — por padrão o
  [`claude`](https://claude.com/claude-code), rodando a skill
  [`review-fleet`](#como-o-review-roda) em modo
  [stream](#o-log-ao-vivo), que é o que alimenta o log da página

## Instalação

```sh
make install            # compila e joga em ~/.local/bin
make install PREFIX=/usr/local/bin
```

Ou pelo Go:

```sh
go install github.com/beroni/bazel@latest
```

> O binário se chama `bazel`, igual à ferramenta de build do Google. Se você usa
> as duas, renomeie uma delas na instalação (`-o ~/.local/bin/bz`).

## Uso

```sh
bazel          # sobe em 127.0.0.1:7777
bazel --open   # e já abre o navegador
```

Não há subcomando: **o binário é o servidor**. Repositórios, agentes e reviews
se gerenciam na página. Na primeira execução o `~/.bazel/config.yaml` é criado
sozinho — daí é só abrir "config" no topo e adicionar um `owner/repo`.

| Flag | Efeito |
| --- | --- |
| `--addr <host:porta>` | onde escutar (padrão `127.0.0.1:7777`) |
| `--jobs <n>` | reviews simultâneos (padrão 2) |
| `--open` | abre o navegador |
| `--keep` | preserva os clones temporários dos PRs |
| `--no-splash` | pula a animação de abertura |
| `--version` | versão |

## A página

Um review leva minutos e nenhuma requisição HTTP segura isso: cada um vira um
**job no servidor**. O navegador recebe o id na hora e o resultado chega depois
por [SSE](https://developer.mozilla.org/docs/Web/API/Server-sent_events). Na
prática: dá para fechar a aba no meio de um review, voltar depois e ele está
lá, pronto.

Pela página você:

- **marca PRs** e escolhe no seletor **qual agente** roda sobre eles;
- acompanha a fila, cancela, e vê o **log ao vivo** — cada agente no seu
  próprio terminal, ver [O log ao vivo](#o-log-ao-vivo);
- **lê o review renderizado** e decide se ele vai para o PR, com comentários
  inline ou como comentário simples — ver
  [Publicar o review no PR](#publicar-o-review-no-pr);
- relê os reviews antigos salvos em disco;
- adiciona e remove repositórios monitorados, e vê em **config** os agentes
  configurados e as [skills instaladas](#agentes-e-skills) na máquina.

`--jobs` é o que separa "revisando dois PRs" de "derrubando o notebook": cada
review clona um repositório e sobe um processo de agente.

> **É single-user, e a porta é local de propósito.** O servidor usa o `gh` já
> autenticado da máquina — quem chega nele manda clonar repositório e rodar
> agente com `Bash` liberado. Por isso ele escuta em loopback, recusa `Host` que
> não seja local (bloqueia DNS rebinding) e recusa `POST` vindo de outra origem
> (bloqueia uma aba qualquer disparando review no seu nome). Não coloque isso
> atrás de um IP público sem autenticação na frente.

## Como o review roda

Cada PR passa por estes passos:

1. `gh pr view` traz os metadados (título, autor, branch, base, corpo).
2. O repositório é clonado numa **pasta temporária** (`gh repo clone` com
   `--filter=blob:none` — histórico inteiro, sem baixar os blobs) e o PR entra
   em checkout com `gh pr checkout`.
3. Cada agente da escolha roda **com o clone como diretório de trabalho**,
   recebendo o prompt pelo stdin. Uma pipeline roda seus agentes um de cada vez
   sobre esse mesmo clone — clona uma vez só. O agente padrão dispara a skill
   [`review-fleet`](https://github.com/beroni/skills) — três lentes em paralelo
   (`senior-code-reviewer`, `exploit-digger`, `lazy-senior-dev`) sobre o mesmo
   escopo, deduplicadas em um veredito só.
4. O clone é apagado no fim. `--keep` preserva, e o caminho aparece no rodapé
   do review.

Um passo que falha não derruba o review: vira uma seção com o erro e o resto
continua. Só quando nenhum agente devolve nada é que o review falha inteiro.

Como o agente navega no código clonado, o diff **não** vai colado no prompt —
ele só é baixado se o seu template usar `{{diff}}`.

Para voltar ao modo antigo (diff no prompt, sem clone), coloque
`agent.checkout: false` e um `{{diff}}` no prompt.

> O `claude -p` nega toda permissão que não estiver liberada, e em silêncio.
> Por isso os args padrão trazem `--allowedTools Read,Grep,Glob,Bash,Agent` —
> sem `Agent` a frota não consegue disparar as lentes, sem `Bash` nenhuma delas
> resolve o escopo. O agente roda em um clone descartável e as instruções da
> skill são read-only: ela reporta, não corrige.

## O que já foi revisado

Terminado um review, o PR fica **marcado na lista**. E quando ele recebe commit
novo depois, o ✓ vira aviso:

```
#482  ✓ revisado 2h · publicado
#479  ⟳ mudou desde o review
```

O `⟳` é o que importa: o que está no GitHub agora **não é mais o que o agente
leu**, e o review que você publicou pode estar falando de código que já não
existe. O PR aberto ganha uma tarja explicando isso.

O Bazel guarda o commit do topo do PR (`headRefOid`) na hora do review, e é a
comparação com o de agora que acende o aviso — não a data, que muda por
qualquer comentário. PR consultado sem esse commit não ganha aviso: melhor não
avisar do que avisar errado.

O índice fica em `<reviews_dir>/revisados.json`, com o agente que rodou, o
arquivo do review e se ele foi publicado. É gravado por escrita atômica
(arquivo temporário e rename) e guarda os últimos 500 PRs; corrompido ou
ausente, ele é simplesmente ignorado — isso é enfeite de lista, não pode
derrubar a listagem.

## O log ao vivo

`claude -p` não escreve nada até o relatório sair — para a interface web
mostrar o agente trabalhando, ele precisa narrar o que faz. É o que
`--output-format stream-json --verbose` liga, e vem nos args padrão:

```yaml
agent:
  command: claude
  args: [-p, --output-format, stream-json, --verbose, --allowedTools, "Read,Grep,Glob,Bash,Agent"]
```

Nesse modo o stdout é JSONL de eventos: o Bazel traduz cada um numa linha
legível (a ferramenta chamada e o argumento que diz o que ela faz) e tira o
relatório final do evento de resultado.

**Cada linha é assinada por quem a escreveu.** A frota sobe as três lentes
dentro do mesmo processo, em paralelo, e sem isso o log chega embaralhado e
anônimo — o Bazel liga cada chamada de `Task` ao sub-agente que ela criou e
carimba nas linhas que saem de dentro dela:

```
review-fleet          | → Agent(senior-code-reviewer): review de precisão
review-fleet          | → Agent(exploit-digger): varredura adversarial
exploit-digger        | → Grep exec.Command
senior-code-reviewer  | → Read internal/agent/agent.go
lazy-senior-dev       | 40 linhas que o stdlib já faz
```

O que liga uma linha ao seu autor é a chamada que a criou: o Bazel guarda o id
de cada chamada de sub-agente (a ferramenta se chama `Agent` numa versão do
Claude Code e `Task` noutra — o que ele procura é um `subagent_type` na
entrada) e carimba tudo que vem com aquele `parent_tool_use_id`. Duas lentes do
mesmo tipo em paralelo viram `exploit-digger` e `exploit-digger 2`, não uma só.

**Cada agente tem o próprio terminal**, com o próprio nome,
a própria contagem de linhas e o próprio scroll — a frota vira quatro quadros
lado a lado, o dela e o de cada lente:

```
┌─ review-fleet ──────────────┐  ┌─ exploit-digger ────────────┐
│ · sessão iniciada           │  │ → Grep exec.Command         │
│ → Agent(senior-code-...)    │  │ nenhuma brecha explorável   │
│ → Agent(exploit-digger)     │  └─────────────────────────────┘
└─────────────────────────────┘  ┌─ lazy-senior-dev ───────────┐
┌─ senior-code-reviewer ──────┐  │ 40 linhas que o stdlib já   │
│ → Read internal/agent/...   │  │ faz                         │
└─────────────────────────────┘  └─────────────────────────────┘
```

Clicar em "log dos agentes" abre e fecha o painel; num review já terminado ele
começa fechado. Evento que ele não conhece é ignorado, e linha
que não é JSON passa direto — um agente que não fala esse formato aparece no
log com a saída crua, do jeito que a escreve.

O log é uma janela das últimas **500 linhas** por review, em memória. Ele não
vai pelo SSE: a página guarda onde parou e busca só o que falta, uma vez por
segundo. O stderr do agente entra junto, em outra cor.

> Se o seu `agent.args` estiver customizado, o Bazel não mexe nele —
> acrescente `--output-format stream-json --verbose` para ganhar o log
> traduzido.

## Saída do review

Todo review vai para três lugares, nessa ordem:

1. **Terminal** — renderizado com markdown formatado.
2. **Arquivo** — `<BAZEL_HOME>/reviews/<repo>-<numero>-<data>.md`, com o
   cabeçalho do PR e o agente que rodou (numa pipeline, o tempo de cada passo).
3. **PR no GitHub** — só se você pedir, e só depois de ler: "publicar review
   inline" roda o `post_agent`, "colar como comentário" cola o markdown.
   Sempre pede confirmação antes. Ver
   [Publicar o review no PR](#publicar-o-review-no-pr).

## Configuração

`~/.bazel/config.yaml` (ou `$BAZEL_HOME/config.yaml`):

```yaml
repos:
  - acme/api-core
  - acme/web-app

authors:
  - beroni

include_drafts: false

# Vazio = <BAZEL_HOME>/reviews
reviews_dir: ""

# Só é usado se o prompt tiver {{diff}}.
max_diff_bytes: 400000

# Vazio = ~/.claude/skills
skills_dir: ""

agent:
  command: claude
  args:
    - -p
    - --allowedTools
    - Read,Grep,Glob,Bash,Agent
  # Clona o repo numa pasta temporária e faz checkout do PR antes de rodar.
  checkout: true
  timeout_seconds: 1800
  prompt: |-
    {{task}}
    ...

# As lentes que o seletor oferece. A primeira é a padrão.
agents:
  - name: review-fleet
    description: as três lentes em paralelo, dedup e veredito único
    task: /review-fleet {{number}}
  # ...

# Sequências rodadas sobre o mesmo clone.
pipelines:
  - name: frota-em-série
    steps: [senior-code-reviewer, exploit-digger, lazy-senior-dev]
```

### Agentes e pipelines

`agents:` é o que o seletor da página oferece, ao lado do botão **revisar**. A
primeira entrada é a padrão — a que roda quando ninguém escolhe.

```yaml
agents:
  - name: review-fleet
    description: as três lentes em paralelo, dedup e veredito único
    task: /review-fleet {{number}}
  - name: exploit-digger
    description: "recall adversarial: caça bug e brecha classe por classe"
    task: /exploit-digger {{number}}
  # Este publica sozinho: `posts` é o que faz a interface avisar antes.
  - name: review-fleet-post
    description: as três lentes e publica o review no PR
    task: /review-fleet {{number}} --post
    posts: true
  # Uma lente pode rodar em outro modelo, ou em outro executável.
  - name: rapidinha
    task: /senior-code-reviewer {{number}}
    args: ["-p", "--model", "claude-haiku-4-5-20251001", "--allowedTools", "Read,Grep,Glob,Bash"]
    timeout_seconds: 600

pipelines:
  - name: frota-em-série
    description: as três lentes uma de cada vez, cada uma no próprio processo
    steps: [senior-code-reviewer, exploit-digger, lazy-senior-dev]
```

Um agente só declara **o que muda**: o `task` entra no `{{task}}` do molde de
`agent.prompt`, e `command`, `args`, `checkout` e `timeout_seconds` herdam do
bloco `agent` quando não vêm preenchidos. `prompt` substitui o molde inteiro.

### Publicar o review no PR

Há três caminhos para o PR, do mais controlado ao mais direto.

**1. Ler e então publicar** (o padrão). Rode `review-fleet`, leia o resultado
na tela e só então clique em **publicar review inline**. Isso sobe o
`post_agent` — a skill `post-report` — sobre um
clone do PR, com o arquivo do review que você acabou de ler no prompt e a
instrução de **não refazer o review**: publica o que está no arquivo, com os
comentários inline nas linhas certas, 👍 no que já está apontado e um "tudo
certo" quando não há achado. Vira um job como qualquer outro, com passos e log.

**2. Colar como comentário** ("ou colar como comentário"). É o Bazel
escrevendo, sem agente: o markdown do review vira um comentário só, na hora.
Não tem inline, mas também não gasta um agente.

**3. Publicar direto**, sem passar pela sua leitura: escolha
`review-fleet-post` no seletor antes de revisar. É a frota rodando com
`--post` — revisa e publica na mesma rodada. Rápido, e você lê o que foi
publicado depois.

Os agentes 1 e 3 têm molde de prompt próprio: o padrão proíbe escrever no
GitHub, e o deles troca a proibição pela autorização explícita. Qualquer agente
seu pode fazer o mesmo com `posts: true`, que é o que faz a interface marcar
com `⇧` e perguntar antes de disparar — publicar é escrita no PR de outra
pessoa. E se você mandar comentar por cima de um review que o agente já
publicou, o Bazel avisa antes.

Quem publica no caminho 1 é o `post_agent`, configurável como qualquer outro:

```yaml
post_agent:
  name: post-report
  task: /post-report {{review_file}}
  posts: true
  # Clona por padrão: comentário inline precisa do diff para achar a linha.
```

Uma **pipeline** encadeia agentes pelo nome, na ordem dada, sobre o mesmo clone;
o relatório sai com uma seção por passo. Passo apontando para agente que não
existe é ignorado.

### Agentes e skills

Em **config**, na página, cada agente aparece com a skill que a sua `task`
invoca e se ela está instalada:

```
review-fleet          ✓ /review-fleet          padrão
review-fleet-post ⇧   ✓ /review-fleet
frota-em-série        ✓ /senior-code-reviewer  ✓ /exploit-digger  ✓ /lazy-senior-dev
post-report           ✗ /post-report           usado ao publicar
```

O `✗` é o aviso que importa: o agente chama uma skill que **não está na sua
máquina** e só vai falhar na hora de rodar. Abaixo vem a lista do que está
instalado de verdade, lida de `~/.claude/skills` (mude com `skills_dir` no
`config.yaml`) — as skills costumam ser symlinks para o repositório onde você
as versiona, e o Bazel segue os links.

> Config escrito antes dos agentes nomeados continua valendo: se o
> `agent.prompt` nunca foi tocado, o Bazel entrega as lentes padrão e a primeira
> reproduz exatamente o que aquele config já fazia. Prompt customizado fica
> intocado, e o seletor mostra só ele — as `task` padrão são comandos de skill do
> Claude Code e não fazem sentido colados no molde de outro agente.

### Trocando de agente

Qualquer executável que leia o prompt no **stdin** e escreva markdown no
**stdout** serve. Com `checkout: true` ele roda dentro do clone do PR, e o que
ele escreve vai para o [log ao vivo](#o-log-ao-vivo) linha a linha.

```yaml
# Claude Code com um modelo específico
agent:
  command: claude
  args: ["-p", "--model", "claude-opus-5", "--allowedTools", "Read,Grep,Glob,Bash,Agent"]

# Codex CLI
agent:
  command: codex
  args: ["exec", "-"]
```

### Placeholders do prompt

`{{task}}`, `{{repo}}`, `{{number}}`, `{{title}}`, `{{author}}`, `{{url}}`,
`{{branch}}`, `{{base}}`, `{{body}}`, `{{workdir}}`, `{{diff}}`.

`{{task}}` é a instrução do agente escolhido — é só ela que muda de uma lente
para a outra. Molde sem `{{task}}` recebe a instrução na primeira linha.

O `post_agent` ganha mais dois: `{{review_file}}`, o caminho do markdown que
você leu, e `{{review}}`, o texto dele.

`{{diff}}` é o único que custa uma chamada extra ao GitHub — se ele não estiver
no template, o diff nem é baixado.

## Variáveis de ambiente

| Variável | Efeito |
| --- | --- |
| `BAZEL_HOME` | diretório de configuração (padrão `~/.bazel`) |
| `BAZEL_NO_SPLASH` | desliga a animação de abertura |
| `NO_COLOR` / `CI` | também desligam a animação |

## Desenvolvimento

```sh
make          # compila em ./bazel
make run      # sobe a interface web e abre o navegador
make check    # fmt + vet + test, antes de commitar
make help     # todos os alvos
```

O front (`internal/server/static/`) vai embutido no binário com `go:embed` —
não tem build step, nem CDN: a página abre offline.

Os pacotes: `server` (HTTP, fila de jobs, SSE), `agent` (roda os agentes e
traduz o stream), `config`, `gh` (fala com o `gh`), `workspace` (o clone
descartável), `store` (reviews salvos e o índice de revisados), `skills`
(descobre as skills instaladas) e `splash` (o ovo).
