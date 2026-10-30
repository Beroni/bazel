---
name: bazel-post-report
description: Publishes to GitHub a review Bazel has already produced and you have already
  read on screen — the markdown whose path comes in the prompt. Posts a single review with
  inline comments, reacts 👍 to what is already on the PR instead of repeating it, and posts
  an all-clear when there were no findings. Ships inside the Bazel binary; not the same as a
  `post-report` you may have installed, which resolves the file on its own.
allowed-tools: Read Grep Glob Bash
model: inherit
effort: high
---

# Bazel Post Report

Leva ao PR um review que **já existe e já foi aprovado por um humano**. O Bazel rodou o
agente, salvou o markdown, mostrou na tela, e a pessoa clicou em publicar. Seu trabalho
começa depois disso: pegar aquele arquivo e transformá-lo em comentário de review no GitHub.

Esta skill vem embarcada no binário do Bazel e chega a você como plugin, numa pasta do Bazel
fora do clone do PR. Ela existe para que publicar funcione em qualquer máquina, sem depender de nada
instalado em `~/.claude/skills`.

## O que já foi decidido antes de você

Três coisas, e nenhuma delas é sua para refazer:

- **O review já foi lido.** Não revise o código, não procure achado novo, não melhore a
  prosa. O que está no arquivo é o que a pessoa leu e aprovou; publicar outra coisa é
  publicar algo que ninguém viu.
- **A publicação já foi confirmada.** O Bazel perguntou na página antes de te disparar.
  **Não peça confirmação** — você roda sem terminal interativo, e uma pergunta aqui ou trava
  ou é engolida. Decida com o que tem e diga no fim o que fez.
- **Os achados já foram filtrados.** Se a pessoa desmarcou algum na tela, o arquivo que
  chegou a você já está sem ele. Publique o que está no arquivo, todo ele.

## O arquivo

O caminho vem no prompt. O formato é o que o Bazel grava:

```markdown
# acme/api-core#482 — Add refresh-token rotation
- Author: @maria
- Branch: `feat/refresh`
- URL: https://github.com/acme/api-core/pull/482
- Head sha: a1b2c3d4e5f6...
- Diff: +240 −18 across 7 file(s)
- Reviewed at: 2026-09-11 14:32 (took 4m12s)
- Agent: review-fleet

---

<o relatório do agente, em markdown>
```

A primeira linha dá **repo e número do PR**. O `- Head sha:` dá o commit que foi revisado. O
que vem depois do `---` é o relatório, e é só ele que vira comentário — o cabeçalho é
metadado do Bazel, não conteúdo do review.

## Passo 1 — Três gates

Cada um **para a publicação**. Nenhum vira aviso.

- **Sem `# owner/repo#N` na primeira linha** → você não sabe onde publicar. Pare e diga.
- **Repo diferente do repo atual** (`gh repo view --json nameWithOwner --jq .nameWithOwner`)
  → você está no clone de um projeto e prestes a comentar em outro. Pare.
- **`- Head sha:` diferente do HEAD atual do PR:**

  ```bash
  gh pr view <N> --json headRefOid --jq .headRefOid
  ```

  Entrou commit depois do review. As linhas se moveram, e uma âncora `path:line` que era
  certa agora aponta para o lugar errado — pior do que não comentar, porque parece revisão e
  não é. **Não poste inline.** Caia para um comentário único no corpo (Passo 5), dizendo
  qual sha foi revisado.

  Cabeçalho **sem** `- Head sha:` é um review salvo por uma versão antiga do Bazel: trate
  como sha diferente e caia para o corpo. Nunca ancore em linha sem poder provar que o
  código é o mesmo.

## Passo 2 — Levantar o que já está no PR

Antes de montar qualquer payload. É isso que decide, achado por achado, entre postar e
reagir, e é o que deixa revisar o mesmo PR duas vezes sem dobrar o ruído.

```bash
# comentários inline, ancorados em linha
gh api repos/{owner}/{repo}/pulls/<N>/comments --paginate \
  --jq '.[] | {id, path, line: (.line // .original_line), user: .user.login, body}'

# comentários no corpo do PR — onde caem o fallback e o all-clear
gh api repos/{owner}/{repo}/issues/<N>/comments --paginate \
  --jq '.[] | {id, user: .user.login, body}'
```

`.line` vem `null` no comentário que ficou desatualizado (a linha saiu do diff): sem o
`// .original_line` você perde exatamente os comentários das rodadas anteriores, que são os
que mais duplicam.

## Passo 3 — Classificar cada achado

Todo achado do relatório cai em **exatamente um** destes destinos. Classifique todos antes
de montar qualquer payload:

| Situação | Destino |
|---|---|
| Já está no PR | 👍 no comentário existente. Nada é postado. |
| Sem `path`/`line`, sha diferente, ou linha fora do diff | corpo do review |
| Resto | comentário inline |

### De onde saem `path` e `line`

Um relatório da frota traz os metadados prontos, uma linha por achado, logo abaixo do `###`:

```markdown
### Transfer debits any account by id
<!-- finding severity=critical class=authz lenses=senior-code-reviewer+exploit-digger path=src/api/transfer.ts line=88 side=RIGHT ref=CWE-639 -->
```

Use esses valores verbatim quando existirem — `path`, `line`, `side` (`RIGHT` por padrão,
`LEFT` para linha removida), e `start_line` quando o achado cobre uma faixa.

Um agente que não segue esse formato deixa a âncora na prosa (`src/api/transfer.ts:88`).
Aí vale ler o caminho e a linha dali — mas **confira que o arquivo e a linha existem no
diff** antes de ancorar:

```bash
gh pr diff <N> --patch | grep -n "^+++ b/src/api/transfer.ts"
```

Na dúvida, corpo. Um achado no corpo do review é lido; um achado ancorado na linha errada
manda o autor olhar código que não tem nada a ver.

### Duplicata leva 👍 e mais nada

**Achado que já está no PR não é postado de novo. Ponto.** Não em outras palavras, não
"complementando", não com o texto de outra lente. Ele sai do payload e vira uma reação.

**Mesmo achado** = mesmo arquivo **e** linhas a ≤5 de distância **e** mesma causa raiz. Vale
igual se o comentário é de outra pessoa ou de uma rodada anterior. Redação diferente
descrevendo o mesmo bug **é** duplicata — é o caso que mais engana, porque o texto novo
parece achado novo. O que não é duplicata é o mesmo arquivo com dois bugs distintos.

```bash
# duplicata de um comentário inline de review
gh api -X POST repos/{owner}/{repo}/pulls/comments/<id>/reactions -f content=+1

# duplicata de um comentário no corpo do PR
gh api -X POST repos/{owner}/{repo}/issues/comments/<id>/reactions -f content=+1
```

São **endpoints diferentes**, e um `id` devolve 404 no outro — o tipo você já sabe, veio de
qual das duas listas do Passo 2. Reagir é idempotente: três rodadas deixam uma 👍, não três.

Filtrar duplicata não é filtrar severidade. Um `critical` que já está no PR continua contando
no veredito que vai escrito no corpo; ele só não vira um segundo comentário.

### O que nunca vira comentário

Uma seção `## Needs human verification` **nunca** vira achado. Ou entra como seção de dúvidas
no corpo, ou fica fora. Ela existe justamente porque o agente não conseguiu confirmar —
postar como achado seria afirmar o que o relatório diz não saber.

## Passo 4 — Tudo que vai para o PR é em inglês

Obrigatório, e este é o último ponto onde dá para pegar: **corpo do review, comentários
inline, cabeçalhos, all-clear — tudo em inglês.** Independente do idioma do relatório, do
repo ou desta instrução.

O relatório normalmente já nasce em inglês. Mas um agente que espelhou o português do prompt
pode ter deixado prosa em pt-BR — **varra o payload antes de mandar e traduza o que sobrou.**
Não é preferência de estilo: o comentário é lido pelo time, fica no histórico do repositório
e não se apaga sem rastro.

## Passo 5 — Publicar

### Um review único com os inline (preferido)

Agrupa tudo numa notificação só, em vez de N:

```bash
cat > /tmp/review.json <<'JSON'
{
  "body": "## Automated review — Bazel\n\n<veredito e resumo>\n\nReviewed at `a1b2c3d`.",
  "event": "COMMENT",
  "comments": [
    {
      "path": "src/api/transfer.ts",
      "line": 88,
      "side": "RIGHT",
      "body": "**critical · authz** (senior-code-reviewer + exploit-digger)\n\nTransfer reads `accountId` from the body without checking ownership.\n\n**Trigger:** `POST /api/transfer` with another user's `accountId` debits their account.\n\n**Fix:** resolve the account from the session, or check `account.ownerId === session.userId` before debiting.\n\nCWE-639 / OWASP A01"
    }
  ]
}
JSON

gh api -X POST repos/{owner}/{repo}/pulls/<N>/reviews --input /tmp/review.json
```

- **`event: COMMENT`, sempre.** Nunca `REQUEST_CHANGES`: isso bloqueia o merge em nome do
  usuário, e a decisão é dele — o veredito vai escrito no corpo e ele age. Nunca `APPROVE`,
  nem quando o relatório diz `approve`.
- Cada comentário abre com uma linha de cabeçalho (severidade · classe, e de quais lentes
  veio), depois o que é em 1–2 linhas, o gatilho quando houver, e o **Fix** concreto. Duas
  lentes no mesmo achado é sinal forte: deixe visível.
- Se a API recusar **um** comentário por linha inválida, ela recusa o **review inteiro**.
  Tire o comentário problemático, mande o resto, e diga qual caiu. Não desista de tudo por
  causa de um, e não engula a perda.
- **Já existe um review seu no PR?** Não abra outro com os mesmos achados. Os que forem
  novos vão num review novo; o resto vira 👍.

### Fallback: comentário único no corpo

Quando ancorar não vale a pena (achados espalhados, linhas fora do diff) ou não é seguro (o
sha não bate):

```bash
gh pr comment <N> --body-file /tmp/report.md
```

É o caminho mais seguro e o que se prefere na dúvida. Com sha diferente, o corpo **tem** que
dizer qual commit foi revisado:

```markdown
> Reviewed at `a1b2c3d`; the PR has moved since, so findings are not line-anchored.
```

### Zero achado — poste o tudo-certo

Agente calado é indistinguível de agente que não rodou. Sem nenhum achado, poste dizendo
isso, com a cobertura junto quando o relatório trouxer uma seção `## Coverage`:

```bash
gh pr comment <N> --body-file /tmp/all-clear.md
```

```markdown
## Automated review — Bazel · no findings

Reviewed at `a1b2c3d` by `review-fleet`.

**Swept clean:**
- `injection` — every query in src/db goes through prepared statements
- `authz` — no new endpoint; the two touched routes keep their session guard

<!-- bazel: all-clear @ a1b2c3d -->
```

"Nenhum problema encontrado" sozinho não diz se o diff foi varrido ou ignorado — quando o
relatório não traz cobertura, diga qual agente rodou e sobre qual sha, que é o que o Bazel
sabe de verdade.

Procure o marcador nos comentários do corpo antes de postar:

- **Mesmo sha** já tem all-clear → nada mudou; **👍 nele**, não poste outro.
- **Sha diferente** → entrou commit novo e o tudo-certo antigo não cobre esse código. Poste
  um novo, e **não apague nem edite o antigo**: ele era verdade sobre aquele sha.

### Todos os achados eram duplicata

Só as 👍, nenhum review. Não mande review vazio e **não poste o tudo-certo** — all-clear é
para zero achado, e aqui há achados, só que já ditos.

## Passo 6 — Dizer o que aconteceu

Reação silenciosa parece achado perdido. Feche o stdout com a contagem real, em markdown:

```
4 findings: 3 posted inline, 1 already on the PR (👍 on @fulano's comment).
The review body carried the verdict and 2 items for human verification.
```

É isso que volta para o card do Bazel e é o que a pessoa vê ao lado do review que ela leu.
Se algum comentário foi recusado pela API, diga qual e por quê.

## Nunca

- Nunca `APPROVE` nem `REQUEST_CHANGES` em nome do usuário.
- Nunca repostar achado que já está no PR — nem reescrito, nem complementado.
- Nunca postar em qualquer idioma que não inglês.
- Nunca resolver nem fechar thread de review existente. A 👍 é a única escrita que se faz em
  comentário de outra pessoa.
- Nunca editar ou apagar um all-clear de sha anterior.
- Nunca ancorar em linha quando o sha não bate.
- Nunca revisar o código você mesmo para ter o que postar, e nunca acrescentar achado que
  não estava no arquivo. Você publica o que já foi lido.
- Nunca editar arquivo, commitar, dar push ou abrir PR.
- Nunca perguntar. Você roda sem ninguém do outro lado; o Bazel já confirmou.
