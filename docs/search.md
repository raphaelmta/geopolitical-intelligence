# Busca por nome, CNPJ e CPF

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
  (`search_state.requested_at`) e monta tabelas novas inteiras, ao lado das
  em uso (não há atualização incremental: as consultas dependem da ordem
  física por `search_rank`, um posto global). A troca é por renomeação.
- **Nada mudou, nada é reconstruído.** Antes de montar as tabelas, a
  reconstrução calcula o grau de toda entidade e compara com as tabelas em
  uso: o conjunto de entidades pesquisáveis, o tipo e o grau de cada uma.
  Se tudo for igual, ela só registra o pedido como atendido (sem trocar as
  tabelas nem mudar `generation`); o log do worker diz "search unchanged by
  the collection; rebuild skipped". A comparação é feita sobre os dados, não
  sobre um registro do que foi escrito: pega qualquer mudança, venha de onde
  vier (coleta, coleta que falhou depois da primeira passada de identidades,
  decisão de analista entre coletas, fonte que só acrescenta vínculos).
  Essa verificação não é instantânea: soma todos os vínculos abertos e cruza
  todas as entidades com as tabelas de busca, cerca de 12 % de uma
  reconstrução completa. Na base nacional, estimados 4 a 6 min por coleta
  sem mudança, em vez de cerca de 41 min (não medido). O
  nome não precisa ser comparado porque uma entidade nunca muda de nome
  (`guard_entity_update`, migração 0019).
- **O que já foi calculado é reaproveitado.** Uma reconstrução pega o nome
  normalizado e as palavras (`tokens`) das entidades que já estão nas
  tabelas em uso e só calcula os das novas. O resultado é, linha a linha, o
  de uma reconstrução do zero (o teste compara as duas depois de cada tipo
  de evento).
- **Versão da reconstrução** (`search_state.build_version`, migração 0024):
  um resumo das definições atuais de `search_norm`, `search_words` e
  `search_tokens` no catálogo, do dicionário `unaccent` (versão da extensão
  e do servidor), do idioma (locale) do banco (provedor, collate, ctype e
  versão da collation, a registrada e a que a biblioteca informa agora: o
  `upper()` de `search_norm` segue a collation padrão) e do texto dos
  comandos da reconstrução. O reaproveitamento
  e o salto só valem quando a versão guardada é igual à atual, e o salto
  também exige o mesmo limiar de palavras frequentes
  (`search_state.frequent_threshold`). Uma migração que mude a normalização
  ou a regra de palavras muda a versão sozinha, sem ninguém lembrar de
  incrementá-la: a reconstrução seguinte calcula tudo de novo. Uma versão
  guardada diferente da atual (ou nenhuma, logo depois da migração 0024)
  também deixa as tabelas sujas, então o worker as reconstrói ao iniciar,
  sem esperar uma coleta. Se a versão mudar durante uma reconstrução, as
  tabelas montadas não entram e a seguinte calcula tudo.
- Quem altera as tabelas de busca à mão (fora da troca) invalida o
  reaproveitamento: para isso, `UPDATE search_state SET build_version =
  NULL`.
- A marca é gravada mesmo se o worker estiver sendo desligado, com até 5 s
  de prazo, e nunca volta no tempo. Se ainda assim ela se perder, as tabelas
  também ficam sujas quando há mais coletas encerradas (concluídas ou com
  falha) do que no início da última reconstrução. Essa contagem não depende
  do relógio.
- Se a reconstrução falhar ou for interrompida (desligamento), as tabelas
  continuam sujas e as anteriores continuam intactas (a reconstrução só lê
  delas). O worker refaz a reconstrução ao iniciar, em segundo plano,
  e também após a próxima coleta. Enquanto isso, as tabelas anteriores
  continuam servindo.
- Só uma reconstrução roda por vez. Pedidos que chegam durante uma
  reconstrução geram exatamente mais uma.
- A troca espera as buscas em andamento com `lock_timeout` curto (2 s) e
  tenta de novo, até 10 vezes, por cerca de 1 min.
- **Tempo:** na base nacional, a reconstrução completa levou cerca de 41 min
  por coleta (2026-11, antes do salto e do reaproveitamento; ver
  `docs/performance/2026-load-optimization-design.md`, tarefa 5, para os
  tempos medidos depois). Durante esse tempo, o worker não consome outra
  mensagem de coleta.
- Outros processos (a API) podem usar a lista anterior de palavras
  frequentes por até 30 s depois de uma troca.

## Consulta

- Uma consulta com CPF é uma busca pelo trecho do CPF (ver "Busca por CPF"
  abaixo). Isso é decidido antes de qualquer outra regra.
- Somente dígitos, depois de retirar `. / -` e espaços:
  - 8 ou 14 dígitos: busca exata pela raiz do CNPJ;
  - 3 a 7 dígitos: prefixo da raiz, em ordem de chave (um número de 6
    dígitos é um prefixo de CNPJ, nunca um trecho de CPF);
  - 11 dígitos: CPF (busca pelo trecho);
  - outros tamanhos: nada.
- CNPJ alfanumérico (8 caracteres, ou 12 mais 2 dígitos, misturando letras e
  dígitos): primeiro a busca exata pela raiz. Se não achar, a consulta vira
  busca por nome.
- Limites: no máximo 200 caracteres (`ErrQueryTooLong`) e no máximo 10
  palavras usadas.

## Busca por CPF

A plataforma nunca guarda um CPF inteiro. `person_record.cpf_fragment` guarda
só os **6 dígitos do meio** (posições 4 a 9 dos 11): é o que a Receita publica
dos sócios (`***NNNNNN**`), e os conectores das fontes que publicam o CPF
inteiro (TSE, CVM, Câmara) derivam o trecho na leitura. Por isso a busca por
CPF **casa só o trecho** e devolve **candidatos**: pessoas que têm ao menos um
registro com esse trecho. Nunca confirma que o CPF digitado é daquela pessoa.

- Formas aceitas:
  - uma consulta feita só de dígitos e de `. / -` e espaços, em qualquer
    agrupamento, com 11 dígitos: `12345678909`, `123.456.789-09`,
    `1234 5678 909`. **Todo número de 11 dígitos é lido como CPF**: um
    telefone, um PIS ou um RENAVAM vira uma busca por CPF (quase sempre
    recusada como `invalid_cpf`; cerca de 1 em 100 passa na verificação e
    busca o trecho). Antes desta mudança, eles davam uma resposta vazia;
  - dentro de uma consulta com palavras, separadas delas (não coladas a
    letras ou dígitos): 11 dígitos seguidos, ou `ddd?ddd?ddd?dd` com cada `?`
    no máximo um entre `. - /` e espaço, como na máscara
    (`maria 123.456.789-09`). Assim, `maria 11998765432` também é lida como
    CPF, e a consulta inteira é recusada se o número não passar na
    verificação;
  - a forma mascarada das fontes: `***456789**`, `***.456.789-**`;
  - dígitos, asteriscos e pontuação de largura total ou outras formas de
    compatibilidade Unicode (`１２３.４５６.７８９-０９`) contam como os ASCII
    (normalização NFKC antes da detecção, na API e na Bancada).

  Um CPF colado a uma palavra (`MARIA12345678909`) não é tirado da consulta:
  esse termo segue as regras de palavras e é descartado, como antes.
- CPF inteiro: os dois dígitos verificadores são conferidos (módulo 11).
  Dígitos errados, ou todos iguais (`000.000.000-00`, `111…`), são recusados
  com `ErrInvalidCPF` (400 `invalid_cpf`): pega erros de digitação. A forma
  mascarada não tem verificação.
- Dois CPFs diferentes na mesma consulta: `ErrMultipleCPFs` (400
  `multiple_cpfs`). O mesmo CPF repetido, ou dado inteiro e mascarado com o
  mesmo trecho (`123.456.789-09 ***456789**`), conta como um. Dois CPFs
  inteiros diferentes com o mesmo trecho são dois CPFs.
- **Só o CPF**: as pessoas com algum registro desse trecho, na mesma ordem
  dos níveis de nome (grau desc., nome normalizado, id), com o mesmo limite
  (padrão 20, máximo 50). O grau é contado na hora, a partir dos vínculos
  abertos, com a mesma definição da reconstrução; entre duas reconstruções
  ele pode diferir do das tabelas de busca.
- **CPF e palavras**, em qualquer ordem (`maria silva 123.456.789-09`): o CPF
  sai da consulta **antes** das regras de palavras, e o resto passa por elas
  sem mudança (palavras vazias, números longos, prefixo só na última). O
  resultado é a interseção: entre as pessoas do trecho, as que têm todas
  essas palavras no nome. Se nenhuma palavra sobrar (só palavras vazias, por
  exemplo), vale só o trecho.
- **Empresas pelas palavras** (sem tipo): quando a consulta tem palavras
  suficientes (3 caracteres úteis), depois das pessoas candidatas vêm as
  empresas que essas palavras acham, pela busca por nome normal (nome exato e
  palavras), até o limite. O CPF não é usado para elas e elas não trazem
  `match`. Assim, colar de volta um nome exibido de MEI com o CPF mascarado
  (`JOAO DA SILVA ***456789** ME`) ainda acha a empresa. Como as empresas
  saem só das palavras, a lista delas é a mesma com qualquer CPF válido: o
  CPF dentro do nome de uma empresa continua sem ser confirmado.
- **Tipo**: com `company`, `party`, `public_body` ou `foreign` a resposta é
  vazia (só pessoas têm trecho de CPF; para achar uma empresa pelo nome,
  busque sem o CPF ou sem tipo); com `person`, só pessoas. Um CPF inválido é
  recusado qualquer que seja o tipo.
- **Quantas pessoas dividem um trecho**: 6 dígitos são 1 milhão de valores
  para cerca de 18,7 milhões de pessoas e 29 milhões de registros na base
  nacional. Medido na base nacional (2026-11), registros por trecho: mediana
  24, percentil 99 de 109 — **tipicamente dezenas de pessoas por trecho**.
  Mas 8 trechos passam de 1.000 registros, 1 passa de 10.000, e o maior tem
  465.539 (um valor de preenchimento das fontes). Daí o aviso na tela, a
  sugestão de acrescentar o nome e o limite abaixo.
- **Limite do trecho** (`DefaultFragmentCap` = 2.000 registros,
  `Service.SetFragmentCap`): antes de listar, uma sondagem limitada conta os
  registros do trecho até 2.001 (lê no máximo isso do índice). Acima do
  limite:
  - só o CPF: nada é listado; a resposta traz `cpf_fragment_too_common:
    true` e a Bancada pede o nome ("Esse trecho de CPF é genérico demais nas
    fontes…");
  - CPF e palavras (3 caracteres úteis ou mais): a busca parte do **nome**
    (o nível de palavras de `search_person`, na ordem de `search_rank`) e
    mantém só quem tem um registro com o trecho, conferido no mesmo índice
    (`entity_id` está nele). Essa ordem é a das tabelas de busca, não o grau
    contado na hora.
- **Desempenho**: abaixo do limite, a consulta começa pelo trecho, pelo
  índice parcial `person_record_cpf_fragment_entity (cpf_fragment,
  entity_id)` (migração 0022), e calcula grau, nome e palavras só nesse
  conjunto pequeno (no máximo 2.000 registros). Nunca roda a busca por nome
  inteira para filtrar depois. Testado com 320 pessoas num mesmo trecho, e
  com um limite baixo para os dois caminhos acima dele.
- **O que não é guardado nem devolvido**: a resposta não repete o CPF nem o
  trecho (os nomes continuam mascarados); cada resultado traz `match:
  "cpf_fragment"` e a resposta, `cpf_fragment_search: true`. A auditoria
  guarda só o tipo, a contagem e `cpf_fragment_search: true`, nunca o texto
  buscado (e `cpf_fragment_too_common: true` quando for o caso). O CPF não
  aparece em logs (o log de acesso da API registra só a rota; o do servidor
  web registra só o caminho, sem `?q=`), mensagens de erro, métricas ou
  parâmetros de tarefas. Na Bancada, uma busca com CPF fica só na memória da
  página: nunca no endereço (`?q=`), no histórico ou no armazenamento do
  navegador. Um endereço que chega com um CPF em `?q=` (link antigo) tem o
  `q` retirado na hora, e a rota guardada para a volta do login também sai
  sem ele.
- **Regra para o operador**: a Bancada manda a busca à API num GET, com o
  texto em `?q=`. Nada na frente de `/api/v1/search` (proxy reverso,
  balanceador, WAF, APM) pode registrar a query string. Antes de qualquer
  implantação hospedada ou para terceiros, a busca precisa passar a POST, com
  o texto no corpo (README, "Antes de abrir para clientes").
- A proteção dos nomes continua: um nome (de empresa ou pessoa) que contém um
  CPF nunca é achado por esse CPF, porque a busca por CPF só olha o trecho dos
  registros de pessoa, nunca o nome.

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
- Uma consulta com 9 ou mais dígitos no total não usa o nível de nome exato.
  Assim, o nível exato também nunca confirma dígitos. (Uma consulta com um
  CPF separado das palavras é uma busca por CPF, acima.)
- Nos nomes exibidos, os CPFs aparecem mascarados, com os separadores
  preservados:
  - 11 dígitos seguidos, mesmo colados a letras, viram `***NNNNNN**`;
  - `ddd?ddd?ddd?dd`, em que cada `?` é no máximo um entre `. - /` e espaço,
    vira, por exemplo, `***.456.789-**`.

  Um CPF escrito de outra forma (`1 2 3 4 5 6 7 8 9 0 1`) aparece como a fonte
  o publicou. Ele continua sem ser pesquisável pelos dígitos.
