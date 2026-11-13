# Senado Federal — mandatos de senadores

Conferido em 2026-11-12 na API de Dados Abertos Legislativos do Senado
(`https://legis.senado.leg.br/dadosabertos`, documentação OpenAPI em
`/dadosabertos/v3/api-docs`), com requisições reais espaçadas (no máximo 2
por segundo, `User-Agent: geoint-dev`, nenhum outro cabeçalho de
identificação). As respostas usadas para conferência ficaram fora do git; a
amostra versionada está em `testdata/senado/` (ver o README de lá).

Legislatura atual: **57ª** (`2023-02-01` a `2027-01-31`, como dão os
próprios mandatos e a API da Câmara). Padrão do conector: 55ª, 56ª e 57ª.

## Rotas usadas

Base padrão `https://legis.senado.leg.br/dadosabertos` (variável
`SENADO_BASE_URL` sobrepõe). Formato escolhido com `Accept:
application/json` (a API também aceita o sufixo `.json`; sem um dos dois
responde XML).

| Rota | Para quê | Arquivo no raw store |
|---|---|---|
| `GET /senador/lista/legislatura/{n}` | senadores da legislatura, seus mandatos e **exercícios** | `senadores_<n>.json` |
| `GET /senador/{código}/filiacoes` | histórico de filiação partidária | `senador_<código>_filiacoes.json` |

As filiações só são pedidas para quem tem ao menos um exercício em algum
mandato listado (suplente que nunca assumiu não é pedido). Tudo fica em
`raw/senado/<data de referência>/<arquivo>`. Cada resposta é **conferida
antes de ser guardada**: uma resposta 200 malformada falha a coleta sem ser
guardada e não "envenena" a data; resposta já guardada sob a mesma data é
reaproveitada.

**Filiações indisponíveis:** 4xx (exceto 429) nas filiações de **um**
senador não falha a coleta: é guardado o marcador
`senador_<código>_filiacoes.unavailable.json` (`{"status": 404}`) e a carga
pula esse senador (`skipped_detail_unavailable`). Isso não grava nada
errado: o Senado não é fonte snapshot, nada é fechado por ausência. 4xx numa
lista de legislatura falha a coleta.

Não usadas: `/senador/{código}` (detalhe: **não tem CPF**; tem data de
nascimento, endereço, telefones), `/senador/{código}/mandatos` (os mesmos
mandatos da lista), `/senador/{código}/licencas`, `/senador/afastados`.

## Formato e paginação

- **Não há paginação**: cada rota responde inteira (57ª: 245 senadores,
  257 KB).
- JSON derivado de um esquema XML: objeto raiz com o nome do conjunto
  (`ListaParlamentarLegislatura`, `FiliacaoParlamentar`), `Metadados`,
  números como texto (`"CodigoParlamentar": "5322"`). Nas amostras as
  listas vêm sempre como array; o conector aceita também um objeto único no
  lugar de uma lista de um.
- Datas `AAAA-MM-DD`.

## Limite de requisições (documentado)

A documentação diz: mais de **10 requisições por segundo** podem receber
**429**; em momentos de muita demanda, **503**; evitar rotinas em horários
redondos. O conector faz no máximo 2 por segundo, uma de cada vez; 429 e
5xx são repetidos até 6 vezes com espera exponencial (2 s, dobrando até
2 min) ou o `Retry-After` (limitado a 2 min). Limites: 16 MiB por
resposta, 3.000 requisições e 2 horas por coleta; redirecionamento só para
o mesmo host e o mesmo esquema (nunca de https para http). Volume: 3 listas + filiações de algumas centenas de
senadores (~5 minutos).

Serviços marcados como depreciados respondem com cabeçalhos `Deprecation`
e `Sunset`; as duas rotas usadas não estavam depreciadas em 2026-11-12.

## Campos lidos

- `IdentificacaoParlamentar`: `CodigoParlamentar`,
  `NomeCompletoParlamentar` (nome civil).
- `Mandatos.Mandato[]`: `UfParlamentar`, `PrimeiraLegislaturaDoMandato` e
  `SegundaLegislaturaDoMandato` (`NumeroLegislatura`, `DataInicio`,
  `DataFim`), `DescricaoParticipacao` (titular ou suplente),
  `Exercicios.Exercicio[]` (`DataInicio`, `DataFim`,
  `SiglaCausaAfastamento`).
- Filiações: `Partido.SiglaPartido`, `Partido.NomePartido`,
  `DataFiliacao`, `DataDesfiliacao`.

### CPF

**O Senado não publica CPF** em nenhuma das rotas usadas (nem no detalhe do
senador). As fichas ficam sem fragmento (`cpf_fragment` vazio); a
identidade não junta senadores a outras fontes por CPF.

**Raw store × banco:** as respostas são guardadas **como vieram** no raw
store (documento público de origem, para reconferência). A lista traz
e-mail, URL de foto e de página e sexo; ficam só no raw store, que é
privado e **nunca é servido pela API**. O banco guarda só código, nome,
partido, UF e datas de mandato.

## Como o mandato é representado

- Um mandato de senador dura **duas legislaturas** (8 anos):
  `PrimeiraLegislaturaDoMandato` e `SegundaLegislaturaDoMandato`. A lista da
  legislatura *n* traz, de cada senador, os mandatos que cobrem *n* —
  titulares, **1º e 2º suplentes** (`DescricaoParticipacao`), com os nomes
  do titular e dos suplentes.
- `Exercicios.Exercicio[]` são os **períodos de exercício efetivo**:
  `DataInicio` e, quando terminou, `DataFim` com `SiglaCausaAfastamento` /
  `DescricaoCausaAfastamento`. Causas vistas: Retorno do titular, Licença
  com convocação de suplente (LCS), Afastamento do exercício, Licença
  Particular, Licença saúde, Renúncia, Falecimento, Término do mandato,
  Cassação de registro/diploma. Exercício vigente: sem `DataFim`.
- Suplente que **nunca assumiu** não tem `Exercicios` (lista da 57ª: 119
  dos 246 mandatos listados não têm exercício). Suplente que assumiu tem os seus
  períodos, como o titular.
- O partido da lista (`SiglaPartidoParlamentar`) é **só o atual**. O
  histórico está em `filiacoes`: cada filiação com `DataFiliacao` e, se
  terminou, `DataDesfiliacao` (em geral a véspera da próxima filiação; às
  vezes o mesmo dia). `S/Partido` = sem partido.

## Datas de fim

**A data de fim gravada é o primeiro dia em que o vínculo já não vale**
(`valid_to_before` exclusivo, convenção de toda a plataforma; um vínculo
vale no dia D quando `valid_from <= D < valid_to_before`). O Senado publica
o **último dia** (inclusivo) em `Exercicio.DataFim`, `DataDesfiliacao` e
`DataFim` da legislatura; o conector grava **último dia + 1**. Uma troca no
mesmo dia (a filiação antiga termina em D e a nova começa em D) grava D.
Assim, o grafo no último dia no assento mostra o vínculo e no dia seguinte
não; trechos consecutivos não têm dia vazio nem duplicado.

## O que `holds_mandate` significa e o que o conector grava

`holds_mandate` significa **ocupar o assento**; `attrs.status`:

| `status` | Quando |
|---|---|
| `exercicio` | titular (`DescricaoParticipacao` `Titular`) em exercício |
| `licenciado` | titular entre um exercício encerrado por causa de licença (tabela) e o exercício seguinte — ou desde então, se ainda não voltou e um suplente em exercício o confirma (linha aberta) |
| `suplente_exercicio` | 1º ou 2º suplente em exercício |

Causas (`SiglaCausaAfastamento`, exatamente como publicadas):

| Sigla | Descrição | Efeito |
|---|---|---|
| `LCS` | Licença com convocação de suplente (sup 120 dias) | titular: `licenciado` |
| `AFO` | Afastamento do exercício (ministro, secretário…) | titular: `licenciado` |
| `LP` | Licença Particular | titular: `licenciado` |
| `LS` | Licença saúde | titular: `licenciado` |
| `LSP` | Licença Saúde-Particular | titular: `licenciado` |
| `REN` | Renúncia | encerra a ocupação |
| `FAL` | Falecimento | encerra a ocupação |
| `CAS` | Cassação de registro/diploma pela Justiça Eleitoral | encerra a ocupação |
| `TER` | Término do mandato | encerra a ocupação |
| `RET` | Retorno do titular | encerra o exercício do suplente |

**Licença sem retorno.** Uma licença do titular é limitada quando ele volta
(há um exercício seguinte). Depois do **último** exercício do titular no
mandato, a API não traz nenhuma data que encerre a licença: a renúncia, a
morte ou a cassação de um titular licenciado só aparecem quando ele reassume
e o exercício termina com `REN`/`FAL`/`CAS` (caso real: o titular licenciado
como ministro (AFO) voltou em 2024-02-01 e renunciou em 2024-02-20 para
assumir no STF; a suplente continuou como "1º Suplente"). Conferido em
2026-11-12: o mandato do titular (`/senador/{código}/mandatos`) não tem data
de fim própria nem situação; a participação do suplente não muda para
titular; `/senador/{código}/licencas` só lista licenças oficiais (e estava
vazia para esse titular); `/senador/afastados` (redireciona para
`/dados/AfastamentoAtual.json`) e `/senador/lista/atual` são fotografias do
momento, sem datas de saída definitiva. **O único sinal definitivo com data
é a causa do fim de um exercício do titular.** Regra: **uma licença sem
retorno só aparece como vínculo atual quando há suplente em exercício que a
confirme** — um exercício de um suplente do mesmo mandato (`CodigoMandato`)
iniciado no dia em que começou o último exercício do titular ou depois, e
ainda sem fim: então a linha `licenciado` fica aberta. Se esses exercícios
de suplente já terminaram, a licença termina no fim do último deles (o
melhor limite exato que a API dá; `leave_bounded_by_substitute`) **somente**
quando esse fim tem causa terminal (`RET`, retorno do titular, ou uma causa
definitiva: `REN`, `FAL`, `CAS`, `TER`) ou ficou mais de 30 dias antes da
data da coleta. Num intervalo menor, sem causa terminal, a licença continua
**aberta** (`leave_open_within_substitute_gap`): suplentes costumam se
suceder com poucos dias de intervalo (5 dias, por exemplo), e fechar a
licença no fim do primeiro deixaria uma linha `licenciado` fechada ao lado
da nova aberta quando o seguinte assumisse. Passados os 30 dias sem novo
suplente, a coleta seguinte a fecha no fim do último exercício. Se não há
nenhum, a licença **não é gravada** (`leave_without_return`). A mesma regra
vale na segunda legislatura de um mandato de 8 anos (nada é reaberto).
Limitação: um titular que sai em definitivo durante a licença, sem
reassumir, continua aparecendo como `licenciado` enquanto o suplente estiver
em exercício.

Causa fora da tabela (ou exercício com `DataFim` sem causa) **nunca é
adivinhada**: o trecho termina na data publicada, nada é aberto, e conta em
`unknown_leave_cause`. O exercício do suplente termina com qualquer causa.
**Suplente efetivado:** a API não distingue o suplente que assume
definitivamente (renúncia ou morte do titular): a participação continua
"1º Suplente", e os trechos ficam `suplente_exercicio`.

Para cada mandato da lista da legislatura *n*:

1. exercícios em ordem de início: cada um é um trecho `exercicio` (titular)
   ou `suplente_exercicio`; depois de um exercício do titular encerrado por
   licença, um trecho `licenciado` do dia seguinte ao último dia até o
   início do exercício seguinte (sem exercício seguinte: a regra da
   "licença sem retorno" acima);
2. recorta ao período da legislatura *n* (`DataInicio`/`DataFim` da
   legislatura no próprio mandato): trecho que segue além é cortado no dia
   seguinte ao último dia dela (a legislatura seguinte tem sua própria
   linha); trecho sem fim numa legislatura que já terminou na data de
   referência termina ali (`closed_at_legislature_end`);
3. corta pelo histórico de filiações: um trecho por partido, do maior dos
   inícios ao menor dos fins. Filiação que termina depois do início da
   seguinte, ou sem `DataDesfiliacao` e não é a última, termina no início
   da seguinte. Parte que nenhuma filiação cobre **não é carregada**
   (`exercises_partly_without_affiliation`).

Gravado (`claims.Store`, linha a linha, transações curtas):

- entidade `public_body` `natural_key` = `senado`, nome "Senado Federal";
- entidade `party` com `natural_key` = sigla em maiúsculas, espaços
  normalizados (**a mesma chave do TSE e da Câmara**), nome = `NomePartido`
  da filiação (um nome existente não é reescrito). Sigla sem nome: nenhuma
  entidade (`party_without_name`); a sigla nunca é usada como nome. As
  entidades de partido **não são ligadas** por este conector (o partido é
  atributo do mandato): existem para que os partidos de todas as fontes
  sejam uma entidade só. `S/Partido`: nenhuma entidade, `party` vazio;
- `person_record` com `context_key` = `sen:<CodigoParlamentar>`, nome =
  `NomeCompletoParlamentar`, sem fragmento de CPF, `record_ref` =
  `senadores_<n>.json:/ListaParlamentarLegislatura/Parlamentares/Parlamentar/<i>/IdentificacaoParlamentar`.
  **Vale a primeira vez**: a ficha existente com o mesmo `context_key` é
  reutilizada mesmo que o nome mude depois. Senador sem exercício não
  ganha ficha (`never_in_office`);
- `holds_mandate` (ficha → Senado), `fact`, `attrs`: `legislature`, `uf`,
  `party`, `status`, `role` = `<legislatura>:<partido>:<UF>:<status>`;
  `valid_from` = início do trecho; `record_ref` = `senadores_<n>.json:` +
  ponteiro JSON para o **`Exercicio`** que deu origem ao trecho (para
  `licenciado`, o exercício encerrado pela licença). **O partido vem de
  outro arquivo**, `senador_<código>_filiacoes.json` da mesma coleta (também
  em `collection_file`), que o `record_ref` não cita;
- trecho com fim declarado é gravado **já fechado** (`closed_collection_id`
  = a coleta, `valid_to_after` = `valid_to_before` = a data de fim
  exclusiva); trecho vigente fica aberto. Uma coleta posterior que traz o
  fim, a licença ou a troca de partido **fecha a mesma linha** nessa
  coleta; nada é duplicado; recarga igual só avança
  `last_seen_collection_id`.

Senado **não é fonte snapshot** no catálogo: nada é fechado por ausência.

## Linhas malformadas

`rows_read` conta os senadores das listas. Pulados e contados por motivo
(`skipped_<motivo>`, somados em `rows_skipped`), o senador inteiro naquela
lista, nunca adivinhando:

- `no_code` (sem `CodigoParlamentar` numérico), `no_name` (sem nome
  completo), `bad_item` (item ilegível);
- `bad_uf`, `no_legislature` (mandato sem a legislatura da lista),
  `bad_date` (data ilegível, impossível ou fim antes do início, no
  mandato, no exercício ou na filiação), `no_party` (filiação sem sigla),
  `detail_unavailable` (filiações respondidas com 4xx, ver acima).

Resposta fora do formato (sem `ListaParlamentarLegislatura.Parlamentares.
Parlamentar` ou `FiliacaoParlamentar.Parlamentar`, JSON inválido) **falha a
coleta** citando o arquivo; filiações de um senador em exercício ausentes
dos arquivos também.

## O que não é carregado

E-mail, URLs de foto e de página, página pessoal, sexo, forma de
tratamento, código público, data de nascimento, endereço, telefones (estes
três nem são pedidos), a relação titular–suplente, a participação (titular,
1º/2º suplente; só decide o status), a descrição da causa do afastamento
(só a sigla decide o status), o código do mandato e do exercício. Comissões, lideranças, votações, discursos e autorias também
não.

## Limitações conhecidas

- Siglas diferentes entre fontes não se juntam; fusões e trocas de nome
  são entidades distintas.
- Exercício removido ou corrigido pelo Senado depois de carregado não apaga
  nada (fonte não snapshot); **um fim corrigido para outra data grava uma
  segunda linha fechada** ao lado da antiga.
- Suplente efetivado continua `suplente_exercicio` (a API não o distingue).
- Causas novas encerram o trecho sem `licenciado` até a tabela ser
  atualizada (`unknown_leave_cause`).
- Períodos sem filiação registrada não são carregados.
- Sem CPF, a identidade só pode ligar senadores a fichas de outras fontes
  por revisão manual.
- Cada resposta carregada guarda em `collection_file` a URL da requisição
  que a trouxe (`source_url`, com os parâmetros de consulta, sem
  credenciais: a API é pública).
