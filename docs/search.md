# Busca por nome e CNPJ

Implementação: `internal/search`. Desenho medido: bloco `SEARCH_STRATEGY` em
`docs/spikes/2026-11-receita-path-queries.md`. Este arquivo registra só o que
o produto acrescenta ou muda em relação ao spike e o que o operador precisa
saber.

## Tabelas

- `search_company`: empresas.
- `search_person`: pessoas que ainda têm registro de pessoa. As entidades
  esvaziadas por fusões de identidade ficam de fora.
- `search_other`: estrangeiros (`E`), partidos (`A`) e órgãos públicos
  (`O`).

A ordem é `grau DESC, nome normalizado (C), id`. O grau é o número de
relações abertas, somadas todas as fontes.

## Reconstrução

- Ao fim de toda coleta bem-sucedida, o worker marca as tabelas como sujas
  (`search_state.requested_at`) e as reconstrói por inteiro. A troca é por
  renomeação.
- A marca é gravada mesmo se o worker estiver sendo desligado, com até 5 s
  de prazo, e nunca volta no tempo. Se ainda assim ela se perder, as tabelas
  também ficam sujas quando há mais coletas encerradas (concluídas ou com
  falha) do que no início da última reconstrução. Essa contagem não depende
  do relógio.
- Se a reconstrução falhar ou for interrompida (desligamento), as tabelas
  continuam sujas. O worker refaz a reconstrução ao iniciar, em segundo plano,
  e também após a próxima coleta. Enquanto isso, as tabelas anteriores
  continuam servindo.
- Só uma reconstrução roda por vez. Pedidos que chegam durante uma
  reconstrução geram exatamente mais uma.
- A troca espera as buscas em andamento com `lock_timeout` curto (2 s) e
  tenta de novo, até 10 vezes, por cerca de 1 min.
- **Tempo:** no volume nacional, a reconstrução deve passar dos 819 s do
  spike, porque agora também calcula a normalização e o grau (a medir na
  Tarefa 20). Durante esse tempo, o worker não consome outra mensagem de
  coleta.
- Outros processos (a API) podem usar a lista anterior de palavras
  frequentes por até 30 s depois de uma troca.

## Consulta

- Somente dígitos, depois de retirar `. / -` e espaços:
  - 8 ou 14 dígitos: busca exata pela raiz do CNPJ;
  - 3 a 7 dígitos: prefixo da raiz, em ordem de chave;
  - 11 dígitos (CPF): nada;
  - outros tamanhos: nada.
- CNPJ alfanumérico (8 caracteres, ou 12 mais 2 dígitos, misturando letras e
  dígitos): primeiro a busca exata pela raiz. Se não achar, a consulta vira
  busca por nome.
- Limites: no máximo 200 caracteres (`ErrQueryTooLong`) e no máximo 10
  palavras usadas.

## Perdas aceitas, além das do spike

- Consultas com menos de 3 caracteres úteis (`OI`, `3M`) ou só com palavras
  vazias (`LTDA`) são recusadas (`ErrQueryTooShort`). Esses nomes só são
  achados pelo CNPJ.
- A regra de palavras é uma só, definida em SQL (`search_words`, migração
  0008) e usada tanto no índice quanto na consulta. Ficam fora:
  - as palavras vazias;
  - toda palavra com 5 ou mais dígitos seguidos (`MARIA12345678901`);
  - quando o nome (ou a consulta) tem **9 ou mais dígitos no total**, contados
    em qualquer posição, toda palavra que contém algum dígito
    (`SILVA123.456.789-01`, `123 456 789 DV 01`, `1 2 3 4 5 6 7 8 9 0 1`,
    `12.345.678/0001-90`).

  Garantia: **um nome que contém 9 ou mais dígitos no total não é pesquisável
  por nenhum dos seus termos com dígitos**. Como um CPF tem 11 dígitos e um
  CNPJ 14, escritos de qualquer forma, nenhuma consulta, com os dígitos certos
  ou errados, casa um nome desses pelos dígitos. Esse nome só é achado pelas
  palavras sem dígitos, e nunca digitando-o por inteiro.

  O custo: os dígitos desses nomes, e números longos em qualquer nome, não
  são buscáveis. Números curtos em outros nomes continuam buscáveis
  (`POSTO 15`, `C6 BANK`, `99 TAXIS`, `RUA 7 123`).
- Quando uma palavra descartada fica no fim da consulta, a palavra anterior
  casa só inteira. A decisão depende só da forma das palavras, nunca de os
  dígitos estarem certos.
- Uma consulta com 9 ou mais dígitos no total, ou com um CPF no formato que a
  máscara reconhece, não usa o nível de nome exato. Assim, o nível exato
  também nunca confirma dígitos.
- Nos nomes exibidos, os CPFs aparecem mascarados, com os separadores
  preservados:
  - 11 dígitos seguidos, mesmo colados a letras, viram `***NNNNNN**`;
  - `ddd?ddd?ddd?dd`, em que cada `?` é no máximo um entre `. - /` e espaço,
    vira, por exemplo, `***.456.789-**`.

  Um CPF escrito de outra forma (`1 2 3 4 5 6 7 8 9 0 1`) aparece como a fonte
  o publicou. Ele continua sem ser pesquisável pelos dígitos.
