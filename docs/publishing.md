# Antes de publicar o repositório

O histórico local **não pode ser enviado a nenhum servidor como está** (GitHub,
GitLab, um espelho, um colaborador). Este documento diz por quê e como preparar
uma versão publicável. Nada aqui foi executado: são passos para o dono do
repositório, na ordem.

## Por quê

1. **Dados pessoais em commits antigos.** Os commits `278b75d` e `6927c07`
   contêm, em `docs/sources/receita.md` e em
   `docs/spikes/2026-11-receita-path-queries.md`, sequências de 11 dígitos com
   cara de CPF real e nomes de pessoas. O commit `741147d` as removeu dos
   arquivos, mas elas continuam no histórico, e todos os commits seguintes
   descendem deles. Apagar só os arquivos não basta: quem clona recebe o
   histórico inteiro.
2. **Identidade provisória.** Todos os commits foram feitos com o autor
   provisório `Rafael <rafael@localhost.invalid>`. Publicar com ele não expõe
   nada, mas o autor certo deve aparecer.
3. **Rascunhos de revisão fora do git.** A pasta `.superpowers/` (ignorada pelo
   git) guarda, entre outros, `review-2aae866..6927c07.diff` e
   `review-6927c07..741147d.diff`, que contêm os mesmos dados removidos.
   Apague-os antes de compartilhar a pasta de qualquer forma (cópia, backup
   enviado a alguém, disco emprestado).

Os commits também trazem linhas `Co-Authored-By` de assistente de IA. Não são
sensíveis; decida se ficam na mensagem do commit publicado.

## Como: um único commit com o estado final (recomendado)

O jeito mais simples e mais seguro é publicar o estado final como um único
commit, sem nenhum dos commits intermediários. Num terminal, na pasta do
repositório, com tudo commitado (`git status` sem mudanças):

```sh
# 1. Sua identidade (só neste repositório).
git config user.name  "Seu Nome"
git config user.email "voce@exemplo.org"

# 2. Um ramo novo, SEM histórico, com a árvore final do stage 1.
git switch --orphan publicacao
git read-tree -u --reset stage-1-foundation
git commit -m "Plataforma de inteligência geopolítica: stage 1"

# 3. Conferências (cada uma deve voltar vazia ou como indicado).
git log --format='%an <%ae> | %cn <%ce>' publicacao      # só a sua identidade, 1 linha
git rev-list --count publicacao                           # 1
git grep -nE '(^|[^0-9])[0-9]{11}([^0-9]|$)' publicacao -- . ':!web/package-lock.json'
#    (o único resultado aceitável é um marcador só de zeros; revise qualquer outro)
git grep -n 'localhost.invalid' publicacao                 # vazio
```

Depois disso, envie **apenas** o ramo `publicacao` (por exemplo
`git push origin publicacao:main`, num repositório remoto novo e vazio). Nunca
envie `stage-1-foundation`, `main` antiga nem `--all`/`--mirror`: eles carregam
o histórico antigo.

Se preferir manter o histórico de `main` (`2aae866`, que não contém os dados),
troque o passo 2 por:

```sh
git switch -c publicacao main
git merge --squash stage-1-foundation
git commit -m "Plataforma de inteligência geopolítica: stage 1"
```

e confira também `git log --format='%an <%ae>' main` (se `main` tiver o autor
provisório, use o caminho do ramo sem histórico acima).

## Depois de publicar

- Apague os rascunhos de revisão (item 3) e, se quiser, o ramo antigo local
  (`git branch -D stage-1-foundation`) só depois de conferir que o ramo
  publicado tem tudo (`git diff stage-1-foundation publicacao` vazio).
- O `.env`, `data/`, `.superpowers/` e as pastas de backup nunca vão para o
  git (veja `.gitignore`); confira com `git status --ignored` antes do
  primeiro envio.
- Antes de abrir o uso a clientes, veja também "Antes de abrir para clientes"
  no [README](../README.md).
