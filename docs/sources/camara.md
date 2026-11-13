# Câmara dos Deputados — mandatos de deputados federais

Conferido em 2026-11-12 na API de Dados Abertos da Câmara (REST v2,
`https://dadosabertos.camara.leg.br/api/v2`), com requisições reais
espaçadas (no máximo 2 por segundo, `User-Agent: geoint-dev`, nenhum outro
cabeçalho de identificação). As respostas usadas para conferência ficaram
fora do git; a amostra versionada está em `testdata/camara/` (ver o README
de lá: só nomes, partidos, UFs e datas de mandato são reais).

Legislatura atual confirmada pela API: **57ª** (`2023-02-01` a
`2027-01-31`). Padrão do conector: 55ª, 56ª e 57ª.

## Rotas usadas

Base padrão `https://dadosabertos.camara.leg.br/api/v2` (variável
`CAMARA_BASE_URL` sobrepõe). Todas com `Accept: application/json`.

| Rota | Para quê | Arquivo no raw store |
|---|---|---|
| `GET /legislaturas/{n}` | datas de início e fim da legislatura | `legislaturas_<n>.json` |
| `GET /partidos?idLegislatura={n}&itens=100&ordem=ASC&ordenarPor=sigla&pagina={k}` | nome de cada sigla (partidos com membros na legislatura, inclusive extintos) | `partidos_<n>_p<k>.json` |
| `GET /deputados?idLegislatura={n}&itens=100&ordem=ASC&ordenarPor=id&pagina={k}` | quem exerceu mandato na legislatura | `deputados_<n>_p<k>.json` |
| `GET /deputados/{id}` | nome civil e CPF | `deputado_<id>.json` |
| `GET /deputados/{id}/historico` | eventos de situação (posse, licença, troca de partido, fim) | `deputado_<id>_historico.json` |

Tudo fica em `raw/camara/<data de referência>/<arquivo>`. Cada resposta é
**conferida antes de ser guardada** (formato esperado); uma resposta 200
malformada falha a coleta sem ser guardada, e por isso nunca "envenena" a
data. Uma resposta já guardada sob a mesma data é reaproveitada (não é pedida
de novo): repetir uma coleta que falhou não refaz as requisições já feitas.

`/partidos` sem parâmetros só lista os 21 partidos ativos; com
`idLegislatura` lista os partidos com membros naquela legislatura, inclusive
extintos (56ª: 36 siglas, com `DEM`, `PROS`, `PSL`, `S.PART.` "Sem
Partido"). Por isso é pedido por legislatura.

**Deputado indisponível:** se a API responde 4xx (exceto 429) ao registro
ou ao histórico de **um** deputado, a coleta continua: em vez da resposta é
guardado um marcador `deputado_<id>[_historico].unavailable.json`
(`{"status": 404}`, nada da API) e a carga pula esse deputado
(`skipped_detail_unavailable`). Isso não grava nada errado: a Câmara não é
fonte snapshot, então um deputado ausente de uma coleta não fecha nenhum
vínculo; ele volta na próxima coleta em que a API responder. 4xx numa
**lista** (deputados, partidos) ou na legislatura falha a coleta.

## Formato e paginação

- Toda resposta é JSON `{"dados": ..., "links": [...]}`; `dados` é lista nas
  rotas de lista e objeto nas de item.
- **Paginação** por `pagina` e `itens` (documentado: padrão 15 itens,
  limite de 100 por requisição). Cada página traz em `links` os
  `rel` `self`, `first`, `last`, `previous` (a partir da 2ª) e `next`
  (exceto na última); os mesmos links vêm no cabeçalho `Link`, e o total no
  cabeçalho `X-Total-Count`. O conector pede páginas de 100 itens em ordem de
  `id` e **segue enquanto a página trouxer `rel: next`** (limite de 200
  páginas por legislatura).
- A lista de uma legislatura tem **um item por deputado e partido/UF**, não
  por deputado: 57ª = 877 itens de 648 deputados distintos; 56ª = 1.071
  itens. Inclui suplentes que exerceram o mandato.
- `historico` traz os eventos **de todas as legislaturas** do deputado, sem
  paginação (só `rel: self`; uma resposta paginada falha a coleta).
- Datas: `AAAA-MM-DD` em `legislaturas`; `dataHora` = `AAAA-MM-DDTHH:MM` no
  histórico. A data do evento é a parte da data.

## Limite de requisições

A documentação da API (`/api/v2/api-docs`, versão 0.4.359) não fixa limite
de requisições. Observado: toda resposta,
mesmo 200, traz `Retry-After: 30` e `Cache-Control: public, max-age=1800`.
O conector faz no máximo **2 requisições por segundo**, uma de cada vez;
429 e 5xx são repetidos até 6 vezes com espera exponencial (2 s, dobrando
até 2 min) ou o `Retry-After` da resposta de erro (limitado a 2 min).
Outros limites: 16 MiB por resposta, 12.000 requisições e 6 horas por
coleta; redirecionamento só para o mesmo host **e o mesmo esquema** (nunca
de https para http).

Volume estimado (não medido numa coleta completa): cerca de 2.000
deputados distintos nas três legislaturas, duas requisições cada, mais ~35
páginas de lista: ~4.000 requisições, ~35 minutos.

## Campos lidos

- `legislaturas/{n}`: `id`, `dataInicio`, `dataFim`.
- `partidos`: `sigla`, `nome`.
- Lista de deputados: só `id` (quem buscar e em quais legislaturas).
- `deputados/{id}`: `id`, `nomeCivil`, `cpf`.
- `historico`, por evento: `idLegislatura`, `dataHora`, `situacao`,
  `condicaoEleitoral`, `descricaoStatus`, `siglaPartido`, `siglaUf`.

### CPF

**A Câmara publica o CPF completo** (11 dígitos) em `deputados/{id}`,
campo `cpf`. No parser só se extrai o fragmento de 6 dígitos (dígitos 4 a 9,
`identity.CPFFragment`) e o resto é descartado na hora: nunca chega a
`person_record`, `record_ref`, `attrs`, logs ou mensagens de erro.

**Raw store × banco:** a resposta da API é guardada **como veio** no raw
store (é o documento público de origem, mantido para que qualquer
afirmação possa ser reconferida); por isso o arquivo `deputado_<id>.json` do
raw store contém o CPF completo e outros dados pessoais publicados pela
Câmara. O raw store é privado (object store interno), **nunca é servido
pela API** da plataforma; a rota de proveniência devolve só nome, chave e
checksum do arquivo. O **banco guarda só o fragmento**.

### Eventos do histórico

Valores observados (históricos de todos os 648 deputados da 57ª, todas as
legislaturas de cada um):

- `situacao`: `Exercício`, `Licença`, `Afastado`, `FIM_MANDATO`,
  `SUPLENCIA`, `CONVOCADO`, `VACANCIA`, `SUSPENSO`, e `null`.
- `condicaoEleitoral`: `Titular`, `Suplente`, `EFETIVADO` (suplente que
  virou titular), `ELEITO`.
- `descricaoStatus`: `Entrada - Posse de Eleito Titular - …`,
  `Entrada - Posse de Suplente - Posse como Suplente | Posse como Titular`,
  `Entrada - Reassunção`, `Alteração de partido`,
  `Saída - Afastamento com prazo determinado - <causa>`,
  `Saída - Afastamento sem prazo determinado - <causa>`,
  `Saída - Afastamento definitivo - Término da Legislatura | Renúncia |
  Falecimento | Perda de Mandato por Ato da Mesa`, `DIVERSO - …`
  (convocação, efetivação, recontagem), textos livres (`Of. 104/2022`,
  vazio).
- Eventos com `situacao` `null` são notas "Nome no início da legislatura /
  Partido no início da legislatura" (inclusive datadas do primeiro dia da
  legislatura **seguinte**): não são mudança de situação e são ignorados.
- O fim da legislatura vem às 23:59 do último dia (`2023-01-31T23:59`) nos
  históricos recentes e às 00:00 em alguns antigos; as demais mudanças
  valem a partir do próprio dia.

## Datas de fim

**A data de fim gravada é o primeiro dia em que o vínculo já não vale**
(`valid_to_before` exclusivo, convenção de toda a plataforma; um vínculo
vale no dia D quando `valid_from <= D < valid_to_before`). Na Câmara:

- fim da legislatura (`FIM_MANDATO` ou `… Término da Legislatura`, datado
  do último dia da legislatura ou do seguinte, a qualquer hora): fim gravado
  = dia seguinte ao `dataFim` da legislatura (`2023-02-01` na 56ª), sem
  depender da hora do evento — nunca há dia vazio antes da legislatura
  seguinte;
- outro evento às 23:59: vale a partir do dia seguinte;
- qualquer outro evento: vale no próprio dia (renúncia em
  `2026-05-19T15:44` → fim `2026-05-19`; troca de partido em `2026-04-08`:
  o partido antigo termina em `2026-04-08` e o novo começa em `2026-04-08`);
- trecho sem evento de fim numa legislatura já terminada na data de
  referência: fim = dia seguinte ao `dataFim` da legislatura
  (`closed_at_legislature_end`).

Assim, consultar o grafo no último dia no assento mostra o vínculo; no dia
seguinte, não; mandatos consecutivos não têm dia vazio nem dia duplicado.

## O que `holds_mandate` significa e o que o conector grava

`holds_mandate` significa **ocupar o assento**: em exercício ou licenciado.
`attrs.status` diz qual:

| `status` | Quando |
|---|---|
| `exercicio` | titular (ou `EFETIVADO`) em exercício |
| `licenciado` | titular em afastamento temporário com causa da tabela abaixo; o assento continua dele |
| `suplente_exercicio` | suplente (`condicaoEleitoral` `Suplente`) em exercício |

Por deputado e legislatura, os eventos com situação, em ordem de
`dataHora`:

- um evento em `Exercício` cuja descrição começa com `Entrada` (posse ou
  reassunção) abre `exercicio` (ou `suplente_exercicio` se a condição é
  `Suplente`);
- saída **definitiva** (`situacao` `FIM_MANDATO` ou `VACANCIA`, ou
  descrição `Saída - Afastamento definitivo - …`: término, renúncia,
  falecimento, perda de mandato) encerra a ocupação;
- afastamento **temporário** do titular (`Saída - Afastamento com prazo
  determinado - <causa>` ou `… sem prazo determinado - <causa>`) com causa
  da tabela: encerra `exercicio` e abre `licenciado` no mesmo dia, até um
  evento em exercício (`Entrada - Reassunção`);
- causa **fora da tabela**, ou qualquer outro evento fora de exercício
  com o titular em exercício: **nunca é adivinhada** — o trecho termina
  nesse dia, nada é aberto, e conta em `unknown_leave_cause`;
- saída do suplente (em geral `Afastamento de Suplente (automático)`,
  quando o titular volta) encerra `suplente_exercicio`; uma saída do
  suplente com outra descrição também encerra, e conta em
  `substitute_exit_other` (não é causa de licença de titular);
- `Entrada - Posse de Suplente - Posse como Titular` abre `exercicio`
  qualquer que seja a condição (nos 648 históricos da 57ª, as 22
  ocorrências vinham com condição `EFETIVADO`);
- evento em `Exercício` com outro status, partido ou UF encerra o trecho e
  abre o seguinte no mesmo dia (troca de partido = duas linhas; suplente
  efetivado, condição `EFETIVADO`, passa a `exercicio`);
- durante a licença, eventos fora de exercício (nova licença, troca de
  partido) mantêm `licenciado`, dividindo por partido/UF;
- um evento em `Exercício` sem trecho aberto e sem `Entrada` não abre nada
  (`exercise_without_entry`).

Causas de afastamento temporário do titular (texto exato depois do prefixo,
até um eventual ` / `), todas observadas:

| Causa | Observação |
|---|---|
| `Ministro de Estado` | |
| `Secretário de Estado` | |
| `Secretário de Prefeitura de Capital` | |
| `Afastamento Conjunto Consecutivo` | licenças somadas |
| `Licença para Tratamento de Saúde` | |
| `Licença para Tratar de Interesse Particular` | |
| `Suspensão` | mandato suspenso; o assento continua dele |
| `Afastamento por Decisão Judicial` | |
| `Afastamento por Decisão da Presidência sem Convocação de Suplente` | |

**Licença sem retorno.** Na Câmara a saída definitiva de um titular
licenciado é um evento do próprio histórico dele (`VACANCIA` renúncia,
falecimento, perda de mandato, ou o fim da legislatura), que encerra a
linha `licenciado` na data do evento (teste
`TestDefinitiveExitDuringLeaveEndsIt`); sem esse evento, a licença aberta
termina no fim da legislatura. Diferente do Senado, aqui não é preciso
suplente para confirmar: o histórico do titular é completo. (No Senado,
"uma licença sem retorno só aparece como vínculo atual quando há suplente
em exercício que a confirme"; ver docs/sources/senado.md.)

Um ministro licenciado hoje tem, portanto, uma linha `licenciado` **aberta**;
o suplente que exerce no lugar dele tem a sua `suplente_exercicio` no mesmo
período (a API da Câmara não liga suplente a titular).

Gravado (`claims.Store`, linha a linha, transações curtas):

- entidade `public_body` `natural_key` = `camara`, nome "Câmara dos
  Deputados";
- entidade `party` com `natural_key` = sigla em maiúsculas, espaços
  normalizados — **a mesma chave do conector do TSE** — e nome = `nome` da
  lista `/partidos` (a da legislatura mais recente em que aparece). **Sigla
  sem nome encontrado não vira entidade** (o partido fica só no atributo;
  conta uma vez por sigla em `party_without_name`); a sigla nunca é usada
  como nome. As entidades de partido **não são ligadas** por este conector
  (o partido é atributo do mandato): existem para que os partidos de todas
  as fontes sejam uma entidade só. `S.PART.` = sem partido: `party` vazio,
  nenhuma entidade;
- `person_record` com `context_key` = `dep:<id>` (uma ficha por deputado,
  estável entre legislaturas), nome = `nomeCivil`, fragmento do CPF,
  `record_ref` = `deputado_<id>.json:/dados`. **Vale a primeira vez**: uma
  ficha existente com o mesmo `context_key` é reutilizada mesmo que o nome ou
  o CPF mudem numa resposta posterior;
- `holds_mandate` (ficha → Câmara), `fact`, `attrs`: `legislature`, `uf`,
  `party`, `status` e `role` = `<legislatura>:<partido>:<UF>:<status>`;
  `valid_from` = início do trecho; `record_ref` =
  `deputado_<id>_historico.json:/dados/<i>` (ponteiro JSON para o evento que
  abriu o trecho);
- **trecho encerrado na fonte** é gravado **já fechado**:
  `closed_collection_id` = a coleta, `valid_to_after` = `valid_to_before` =
  a data de fim (exclusiva; `claims.Store.AssertEndedRelationship`).
  "Aberto" na plataforma significa vigente;
- trecho vigente: linha aberta, `valid_to` nulo. Se uma coleta posterior
  mostra o fim (ou uma troca de partido, ou a licença), **a mesma linha é
  fechada** nessa coleta, na data de fim, e a nova é aberta; nada é
  duplicado. Recarregar a mesma informação só avança
  `last_seen_collection_id`.

Câmara **não é fonte snapshot** no catálogo: nada é fechado por ausência.

## Linhas malformadas

`rows_read` conta os itens das listas de deputados. São pulados e contados
por motivo (`skipped_<motivo>`, também somados em `rows_skipped`):

- `no_id`: item de lista sem `id` numérico;
- `bad_date`, `bad_uf`, `no_party`: evento com situação na legislatura com
  `dataHora` ilegível ou impossível, UF que não é de duas letras, ou sem
  `siglaPartido` — a legislatura inteira daquele deputado é pulada (os
  itens de lista dele nela contam como pulados): o conector nunca adivinha
  em volta de um evento ruim;
- `bad_event`: evento ilegível ou sem `idLegislatura` — o deputado inteiro;
- `no_name`: deputado sem `nomeCivil`;
- `detail_unavailable`: a API respondeu 4xx ao registro ou ao histórico do
  deputado (ver acima).

Uma resposta fora do formato (sem `dados`, `dados` do tipo errado, JSON
inválido, histórico paginado, legislatura sem datas, deputado com outro
`id`) **falha a coleta** com erro que cita o arquivo. Arquivo esperado e
ausente (p.ex. histórico de um deputado listado) também falha a coleta.

## O que não é carregado

E-mail, telefone e dados do gabinete, data e município/UF de nascimento,
data de falecimento, escolaridade, sexo, site, redes sociais, URL de foto,
nome eleitoral/parlamentar, a descrição dos eventos (só a causa decide o
status), o `uriPartido`. Nenhum desses campos chega ao banco nem aos logs (só ficam na
resposta original do raw store). Despesas, votações, proposições, frentes e
órgãos também não.

## Limitações conhecidas

- A sigla do partido é a da Câmara; siglas grafadas de outra forma pelo
  TSE não se juntam (p.ex. `PCdoB` → `PCDOB` aqui; o TSE já usou `PC do B` →
  `PC DO B`). Fusões e trocas de nome (DEM/PSL → UNIÃO) são entidades
  distintas.
- Um evento removido ou corrigido pela Câmara depois de carregado não
  apaga nada: a linha antiga continua (fonte não snapshot). **Um fim
  corrigido para outra data grava uma segunda linha fechada** ao lado da
  antiga (mesmo início, outro fim).
- Causas de afastamento novas (fora da tabela) encerram o trecho sem
  `licenciado` até a tabela ser atualizada (`unknown_leave_cause`).
- Legislaturas fora das configuradas não são lidas, nem eventos delas.
- Cada resposta carregada guarda em `collection_file` a URL da requisição
  que a trouxe (`source_url`, com os parâmetros de consulta, sem
  credenciais: a API é pública).
