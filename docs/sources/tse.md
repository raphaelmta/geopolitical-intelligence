# TSE — Candidaturas e prestação de contas eleitorais

Conferido em 2026-11-11, baixando os arquivos do Portal de Dados Abertos do
TSE (CDN `https://cdn.tse.jus.br/estatistica/sead/odsele/`) e lendo seus
cabeçalhos e valores (só contagens e formatos; nenhum dado pessoal foi
copiado para o repositório). Os `leiame.pdf` que vêm dentro dos zips foram
lidos também.

## Onde ficam os arquivos

Os nomes são determinísticos (não é preciso listar diretório):

| Conjunto | URL (relativa à base) |
|---|---|
| Candidaturas | `consulta_cand/consulta_cand_<ANO>.zip` |
| Prestação de contas de candidatos | `prestacao_contas/prestacao_de_contas_eleitorais_candidatos_<ANO>.zip` |

Base padrão: `https://cdn.tse.jus.br/estatistica/sead/odsele/` (variável
`TSE_BASE_URL` sobrepõe). O CDN responde `HEAD` com `Content-Length`,
`ETag` e `Accept-Ranges: bytes`; o conector usa o tamanho para conferir o
download e `Range`/`If-Range` para retomar. Nenhum cabeçalho de
identificação é enviado além de um `User-Agent` genérico.

Tamanhos em 2026-11-11 (`Last-Modified` entre parênteses):

| Ano | `consulta_cand_<ANO>.zip` | `prestacao_de_contas_eleitorais_candidatos_<ANO>.zip` |
|---|---|---|
| 2018 | 4,7 MB (2024-11-13) | 311 MB (2026-11-08) |
| 2020 | 85 MB (2026-01-06) | 1,37 GB (2026-11-07) |
| 2022 | 4,4 MB (2026-11-11) | 522 MB (2026-11-08) |
| 2024 | 65 MB (2026-11-11) | 1,25 GB (2026-11-11) |
| 2026 | 3,3 MB (2026-11-11) | 437 MB (2026-11-11) |

**2026:** os dois arquivos já existem, mas são republicados quase todo dia
(eleição de outubro de 2026; prestações de contas finais ainda chegando).
Não são definitivos: cada versão publicada é guardada (a chave do raw store
vem dos validadores do arquivo no servidor; veja "Coleta").

Baixados para conferência (em `data/raw/tse/`, fora do git): os cinco
`consulta_cand_<ANO>.zip` (162 MB ao todo), o zip de prestação de contas de
2026 inteiro (437 MB) e, por requisições `Range` sobre o diretório central
dos zips, só as entradas `receitas_candidatos_<ANO>_AC.csv` de 2018, 2020,
2022 e 2024 e o `leiame_receitas-candidatos.pdf`.

## Entradas de cada zip e o que é carregado

Cada zip tem **um CSV por UF**, mais um `..._BR.csv` (abrangência nacional:
Presidente/Vice nas eleições gerais) e um `..._BRASIL.csv`, além de PDFs
`leiame`.

- `consulta_cand_<ANO>.zip`: `consulta_cand_<ANO>_<UF>.csv`,
  `consulta_cand_<ANO>_BR.csv` (só 2018, 2022, 2026), `consulta_cand_<ANO>_BRASIL.csv`, `leiame.pdf`.
- `prestacao_de_contas_eleitorais_candidatos_<ANO>.zip`:
  `receitas_candidatos_<ANO>_<UF>.csv`, `receitas_candidatos_<ANO>_BR.csv`
  (anos gerais), `receitas_candidatos_<ANO>_BRASIL.csv`, e também
  `receitas_candidatos_doador_originario_*`, `despesas_contratadas_*`,
  `despesas_pagas_*` e os `leiame_*.pdf`.

**O `_BRASIL.csv` é o agregado das UFs + BR**, conferido:

- candidaturas, todos os anos: o conjunto de `SQ_CANDIDATO` do BRASIL é
  igual ao da união das UFs (2018: 29.227; 2020: 558.572; 2022: 29.270;
  2024: 463.731; 2026: 20.989);
- receitas 2026: 257.555 linhas no BRASIL e 257.555 na soma das UFs + BR;
- receitas, todos os anos: o tamanho descompactado do BRASIL é exatamente a
  soma das UFs + BR menos um cabeçalho por arquivo a mais (1.068 bytes cada).

Por isso **só as entradas de duas letras são lidas**
(`^consulta_cand_<ANO>_[A-Z]{2}\.csv$` e
`^receitas_candidatos_<ANO>_[A-Z]{2}\.csv$`); `_BRASIL.csv` nunca é lido,
nem os doadores originários, as despesas e os PDFs. Uma linha nunca é
carregada duas vezes. Entradas são lidas do zip guardado (sem extrair para
disco), com limites de tamanho descompactado por entrada e no total.

## Formato dos CSV

Igual em todos os anos de 2018 a 2026 (o TSE republicou os anos antigos no
leiaute atual) e igual ao que o `leiame.pdf` descreve:

- **com cabeçalho** (linha 1);
- separador `;`;
- **todos** os campos entre aspas duplas, inclusive números (nenhuma linha
  sem aspas nas amostras);
- fim de linha **CRLF**;
- codificação **Latin-1** (ISO-8859-1; o leiame diz "Latin 1"; os
  arquivos não são UTF-8 válido);
- nenhum campo com quebra de linha ou aspas internas nas amostras (o leitor
  aceita as duas coisas, como o da Receita);
- datas `DD/MM/AAAA`; valores com vírgula decimal e sem separador de milhar
  (`1500,00`).

As colunas são lidas **pelo nome do cabeçalho**, com um mapa de nomes por
ano (`internal/connector/tse/parse.go`; hoje todos os anos usam os mesmos
nomes). Coluna obrigatória ausente faz o arquivo falhar com erro que cita a
coluna; colunas a mais são ignoradas. `record_ref` é
`<nome da entrada>:<linha física>`, com o cabeçalho na linha 1 (a primeira
linha de dados é a 2).

### Valores sentinela (leiame)

- `#NULO` (ou `#NULO#` em anos antigos): campo em branco na base; numérico
  correspondente `-1`;
- `#NE`/`#NE#`: informação não registrada naquele ano; numérico `-3`;
- `-4`: dado pessoal **não divulgável** (campos numéricos); `NÃO DIVULGÁVEL`
  nos textuais (só aparece em e-mail, nome social, gênero e instrução: o
  nome civil sempre vem).

Todos viram vazio na carga.

### Candidaturas (`consulta_cand_<ANO>_<UF>.csv`, 50 colunas)

Colunas usadas: `SQ_CANDIDATO`, `NR_TURNO`, `DT_ELEICAO`, `CD_CARGO`,
`DS_CARGO`, `SG_UF`, `SG_UE`, `NM_UE`, `NM_CANDIDATO`, `NM_URNA_CANDIDATO`,
`NR_CPF_CANDIDATO`, `SG_PARTIDO`, `NM_PARTIDO`, `DS_SIT_TOT_TURNO`.

- **Uma linha por candidatura e turno**: quem foi ao 2º turno tem duas
  linhas com o mesmo `SQ_CANDIDATO` (`NR_TURNO` 1 com `DS_SIT_TOT_TURNO` =
  `2º TURNO`, e `NR_TURNO` 2 com o resultado final). A carga deduplica por
  (ano, `SQ_CANDIDATO`): resultado da linha do maior turno; `valid_from` =
  menor `DT_ELEICAO` (a data do 1º turno da eleição daquela candidatura).
- O arquivo de um ano também traz **eleições suplementares** posteriores
  (`CD_TIPO_ELEICAO` = 1; p.ex. o de 2024 tem eleições de 2025 e 2026), cada
  uma com sua `DT_ELEICAO`; elas entram com a própria data.
- `SG_UE` é a UF (ou `BR`) nas eleições gerais e o código TSE do município
  (5 dígitos) nas municipais. `city` = esse código quando é numérico.
- `DS_SIT_TOT_TURNO` vem `#NULO` (2022+) ou `#NULO#` (2018) para quem não
  chegou à votação.

**CPF do candidato, por ano** (`NR_CPF_CANDIDATO`):

| Ano | Publicação |
|---|---|
| 2018 | completo (11 dígitos); 108 linhas `-4` |
| 2020 | completo; 284 linhas `-4` |
| 2022 | completo; 29 linhas `-4` |
| 2024 | **ausente**: `-4` em todas as 463.935 linhas (Res. TSE 23.609/2019, art. 33 §2º, incluído pela Res. 23.729/2024, citada no leiame) |
| 2026 | completo; 3 linhas `-4` (2026-11-11) |

### Receitas (`receitas_candidatos_<ANO>_<UF>.csv`, 60 colunas)

Colunas usadas: `SQ_CANDIDATO`, `CD_ORIGEM_RECEITA`, `DS_ORIGEM_RECEITA`,
`NR_CPF_CNPJ_DOADOR`, `NM_DOADOR`, `NM_DOADOR_RFB`, `SG_PARTIDO_DOADOR`,
`NM_PARTIDO_DOADOR`, `SQ_CANDIDATO_DOADOR`, `SQ_RECEITA`, `DT_RECEITA`,
`VR_RECEITA`.

- `SQ_CANDIDATO` é o mesmo da candidatura (conferido em 2022/AC: todas as
  receitas acham a candidatura).
- **`SQ_RECEITA` não é único por linha**: uma receita pode vir em vários
  itens (recursos estimáveis com descrições e valores diferentes; só
  `DS_RECEITA`, `VR_RECEITA` e `DS_NATUREZA_RECURSO_ESTIMAVEL` mudam). Em
  2026 havia 6.708 receitas com mais de uma linha. A carga agrega os itens:
  um `donated_to` por receita, `amount` = soma exata dos itens, `items` =
  número de linhas, `record_ref` = a primeira linha.
- `SQ_RECEITA` = `-1` nas linhas de origem `#NULO` (sem doador, sem data):
  não são carregadas.
- `NM_DOADOR_RFB` é o nome no cadastro da Receita; difere de `NM_DOADOR`
  em cerca de metade das linhas (grafia, abreviação). Usa-se o da RFB, e
  `NM_DOADOR` quando o da RFB vem vazio/sentinela.
- `NR_CPF_CANDIDATO` nas receitas: completo em 2018–2022, `-4` em 2024 e
  2026. **Não é lido** (a identidade do candidato vem da candidatura).

**Documento do doador** (`NR_CPF_CNPJ_DOADOR`), todos os anos conferidos:
CPF **completo** (11 dígitos) para pessoa física, recursos próprios e
doações pela internet; CNPJ completo (14 dígitos) para partido, outro
candidato (CNPJ da campanha), financiamento coletivo (CNPJ da plataforma);
`-4` quando não há doador (rendimentos, origem não identificada, `#NULO`).

Origens (`CD_ORIGEM_RECEITA`) e o que a carga faz:

| Código | Origem | Carga |
|---|---|---|
| 10010200 | Recursos de pessoas físicas | ficha `donor:<ano>:<SQ_CANDIDATO>` → `donated_to` |
| 10010400 | Doações pela Internet | idem (CPF do doador) |
| 10010300 | Recursos de pessoas jurídicas | entidade `company` por `CNPJBase` → `donated_to`. **Não aparece em 2018–2026** (doação de empresa proibida desde 2015, ADI 4650); tratado para correções e anos antigos |
| 10020000 | Recursos de partido político | entidade `party` pela sigla `SG_PARTIDO_DOADOR` → `donated_to`; sem sigla: `party_unknown` |
| 10040000 | Recursos de outros candidatos | ficha da candidatura `SQ_CANDIDATO_DOADOR` do mesmo ano → `donated_to`. Não carregado, contado por recibo: sem `SQ_CANDIDATO_DOADOR` publicado, `donor_candidate_unpublished`; doador = a própria candidatura, `donor_candidate_self` (nunca um laço); candidatura doadora ausente do arquivo de candidaturas do ano, `donor_candidate_not_in_file` |
| 10010100 | Recursos próprios | não carregado (seria laço do candidato com ele mesmo): `own_resources` |
| 10020500 | Recursos de Financiamento Coletivo | não carregado: o CNPJ é da plataforma; os doadores reais estão em `receitas_candidatos_doador_originario_*`, que não é lido: `crowdfunding` |
| 10030xxx, -1 | rendimentos, comercialização de bens, fundo partidário/FEFC repassado, origem não identificada, `#NULO` | não carregado: `other_origin` |

Em 2026 (todas as UFs): 118.491 PF, 475 internet, 84.425 partido, 31.540
outros candidatos, 17.159 próprios, 3.286 financiamento coletivo, 1.773
`#NULO`, 252 outras.

## Modelo gravado

- Partido: entidade `party`, `natural_key` = sigla em maiúsculas e sem
  espaços nas pontas (`PC do B` → `PC DO B`); nome = `NM_PARTIDO` da primeira
  linha vista (nunca reescrito). Partidos dos doadores (`SG_PARTIDO_DOADOR`,
  `NM_PARTIDO_DOADOR`) também criam a entidade. Doador de origem partidária
  sem sigla mas com `NR_PARTIDO_DOADOR`: o número vira a sigla que as
  candidaturas do mesmo ano dão a esse número (`NR_PARTIDO`/`SG_PARTIDO`),
  quando só uma sigla o usa (`party_number_mapped`); senão `party_unknown`.
- Candidato: `person_record` com `context_key` = `cand:<ANO>:<SQ_CANDIDATO>`,
  nome = `NM_CANDIDATO` (o nome de urna não entra na identidade), fragmento
  do CPF quando publicado. **`SQ_CANDIDATO` identifica a candidatura: uma
  ficha por candidatura, e vale a primeira publicação.** Se uma publicação
  posterior muda o nome ou o CPF (p.ex. de `-4` para completo), a ficha
  existente é reutilizada com o nome e o fragmento da primeira vez (as
  colunas da ficha são imutáveis); `candidate_for` e as doações recebidas não
  se duplicam. Consequência: uma candidatura carregada sem CPF continua sem
  fragmento e não é juntada a outras fichas pela identidade.
- `candidate_for` (ficha do candidato → partido), `fact`, `attrs`: `year`,
  `office` (`DS_CARGO`), `office_code`, `uf`, `city` (código TSE do
  município ou vazio), `city_name`, `result` (`DS_SIT_TOT_TURNO` final ou
  vazio), `ballot_name` e `role` = `<ano>:<CD_CARGO>:<SG_UF>:<código do
  município ou vazio>`; `valid_from` = data da eleição (1º turno).
- Doador pessoa física: `person_record` `donor:<ANO>:<SQ_CANDIDATO>`. A
  chave única de `person_record` é (fonte, `context_key`, `name_norm`,
  `cpf_fragment`): dois doadores diferentes do mesmo candidato no mesmo ano
  são duas fichas; o mesmo doador doando várias vezes é uma ficha. Dois
  homônimos sem CPF publicado doando ao mesmo candidato no mesmo ano viram
  uma ficha só (não há como separá-los).
- Doador empresa: entidade `company` com `natural_key` = `CNPJBase` (a mesma
  chave da Receita: junta com as empresas já carregadas); se não existir,
  é criada com o nome do arquivo.
- `donated_to` (doador → ficha do candidato), `fact`, um por receita
  (ano, `SQ_RECEITA`, candidatura). `attrs`: `year`; `amount` = **soma de
  todas as linhas da receita** (texto com ponto decimal, soma exata em
  `numeric`, nunca float; numa receita de uma linha, o valor exatamente
  como publicado); `date` (`AAAA-MM-DD`); `origin` (`DS_ORIGEM_RECEITA`);
  `origin_code`; `receipt_id` (`SQ_RECEITA`); `items` = número de linhas da
  receita; `role` = `SQ_RECEITA`. `valid_from` = `DT_RECEITA` (menor data
  entre as linhas); `record_ref` = **a primeira linha** da receita (por
  arquivo e linha). Doações repetidas do mesmo doador ao mesmo candidato têm
  `SQ_RECEITA` diferentes: linhas diferentes.
- Doador, origem e nome do doador vêm da primeira linha da receita. Receita
  cujas linhas discordam do doador conta em `receipt_mixed_donor` (só o
  doador da primeira linha entra).
- **Vale a primeira publicação da receita**: se uma publicação posterior
  traz a mesma receita (mesma ficha do candidato e mesmo `SQ_RECEITA`) com
  outro doador ou outra data, nada é gravado (nem ficha de doador nova, nem
  `donated_to`) e a linha já aberta fica intacta, sem avançar
  `last_seen`; conta em `receipt_conflict`.
- Receita cujo candidato não está na carga do mesmo ano:
  `candidate_unknown`.
- `result` de `candidate_for` é o da primeira carga (`attrs` é imutável):
  uma candidatura carregada entre os turnos fica com `2º TURNO`, mesmo que
  depois o TSE publique o resultado final.

## Privacidade

- **Nenhum CPF completo é guardado.** De `NR_CPF_CANDIDATO` e
  `NR_CPF_CNPJ_DOADOR` (pessoa física) só se extrai, no parser, o fragmento
  de 6 dígitos (dígitos 4 a 9, `identity.CPFFragment`); o resto é descartado
  na hora e nunca chega a staging, logs, `record_ref`, `attrs` ou mensagens
  de erro. CPF ausente, `-4`, mascarado sem esses 6 dígitos ou
  `000000` → fragmento vazio (a ficha é criada; nada é juntado por nome).
  Um CPF com 10 dígitos (zero à esquerda perdido) também dá fragmento vazio.
- **Não são carregados**: título de eleitor (`NR_TITULO_ELEITORAL_CANDIDATO`),
  e-mail, data e UF de nascimento, gênero, cor/raça, grau de instrução,
  estado civil, ocupação, nome social, CPF do vice, município/UF do doador,
  CNAE do doador, descrição da receita. Também não se carregam despesas.
- Doações de pessoas físicas são públicas por lei, mas ligam pessoas
  privadas à política: entram como `fact` com proveniência exata da linha;
  nada é inferido aqui.
- Logs citam arquivo e linha, nunca conteúdo de linha.

## Coleta

- `New(baseURL, years)`; anos padrão 2018, 2020, 2022, 2024, 2026, todos os
  cargos. Para cada ano, os dois zips; faltar um (404) falha a coleta.
- Só escopo completo (`Scope.Full`): o pedido de coleta não tem como
  expressar "só um ano". Recortes (`cnpjs`/`depth`) não existem para o TSE:
  a API e `geoint collect` os recusam (`SupportsSlice` falso no catálogo) e
  a carga recusa qualquer outro escopo (`ErrScope`).
- Data de referência: a do pedido. Antes de baixar, um HEAD lê os
  validadores de cada zip (ETag, Last-Modified, tamanho; a CDN manda os
  três), e a chave no raw store vem deles:
  `raw/tse/<ANO>/<id dos validadores>/<arquivo>` (id = 16 primeiros dígitos
  hexadecimais do SHA-256 de ETag, Last-Modified e tamanho). Um arquivo que
  o TSE não regerou desde a última coleta não é baixado nem guardado de novo:
  a coleta nova lê o mesmo objeto e o registra no seu `collection_file`
  (mesma chave e mesmo SHA-256). Um arquivo regerado ganha chave nova; o
  antigo fica (nada é apagado). Risco, o de qualquer cache HTTP: um arquivo
  trocado sem mudar ETag, data nem tamanho não seria percebido. Sem
  validadores (servidor que não manda ETag nem Last-Modified), a chave volta
  a ser `raw/tse/<AAAA-MM-DD>/<arquivo>` (uma cópia por data de referência),
  e um arquivo já guardado ali com outro tamanho é erro.
- Quanto isso economiza depende do TSE: em 2026-11-12, 8 dos 10 zips
  (≈ 4,0 dos 4,1 GB) tinham `Last-Modified` na última semana, inclusive as
  prestações de contas de 2018 a 2022; só `consulta_cand_2018` e
  `consulta_cand_2020` estavam parados. Enquanto o TSE regerar os arquivos
  toda semana, a coleta semanal ainda guarda cerca de 4 GB por semana.
- TSE **não é fonte snapshot** no catálogo: nada é fechado. Recarregar um
  ano (nova coleta) não duplica nada: as linhas já vistas só avançam
  `last_seen_collection_id`.
- **Frescor**: `PublishEvery` 7 dias, `StaleAfter` 30 dias. As contas da
  eleição em curso são republicadas pelo TSE todos os dias enquanto são
  entregues e corrigidas; uma carga do ano corrente fica vencida em um mês
  sem nova coleta, e o agendador recarrega a fonte toda semana (minutos por
  ano). Correções do TSE nunca substituem o que já foi carregado: chegam como
  acréscimos (a primeira publicação de cada recibo vale).
- **Arquivo de origem**: cada CSV carregado guarda em `collection_file` a
  URL do zip no CDN (`source_url`).
- **Identidades antes dos vínculos**: depois de gravar as fichas de
  candidaturas e de doadores e antes do primeiro vínculo, a carga roda
  `ANALYZE person_record` e a primeira passada da resolução de identidades
  só para as fichas desta coleta (`identity.Resolver.ResolveRecords`, como
  na Receita: veja `receita.md`). Os vínculos já nascem apontando para a
  entidade final de cada ficha; o `Resolve` do fim da coleta não reaponta
  nada.
- **Estatísticas**: depois das fases em lote a carga roda `ANALYZE` em
  `entity`, `person_record` e `relationship`.

## Carga de teste (arquivos reais de 2026-11-11 e 2026-11-12)

Num projeto compose descartável, um ano por vez, carga e recarga idêntica
(nova coleta):

- **2026** (eleição geral): 278.572 linhas lidas (21.017 candidaturas,
  257.555 receitas), nenhuma malformada; 30 partidos, 121.970 fichas,
  236.474 relações (20.989 `candidate_for`, 215.485 `donated_to`); não
  carregadas: 17.159 recursos próprios, 3.286 financiamento coletivo, 2.172
  outras origens, 2 doadores-candidatos não identificados. Carga 68 s;
  recarga 51 s (nada inserido; a maior parte é avançar `last_seen` de
  236.474 linhas). `Resolve`: 6.628 fichas juntadas em 3,9 s.
- **2024** (eleição municipal; zip de receitas com 1,34 GB, baixado em
  3 min 46 s): 2.505.084 linhas (463.935 candidaturas, 2.041.149 receitas),
  nenhuma malformada; 31 partidos, 1.023.285 fichas, 2.001.313 relações;
  não carregadas: 264.504 recursos próprios, 69.691 outras origens, 4.347
  financiamento coletivo, 26.767 doadores-candidatos sem candidatura no
  arquivo do ano, 2 partidos sem sigla nem número. Carga 9 min 47 s (5 min
  29 s inserindo relações); recarga 6 min 32 s (3 min 27 s avançando
  `last_seen`). `Resolve`: 77.915 fichas juntadas em 64 s.

A busca de conflito de receita usa o índice parcial
`relationship_open_receipt` (migração 0016) com `dst_record_id` e
`attrs ->> 'role'` como condições de índice; roda uma vez por carga.

## Limitações conhecidas

- Partido que mudou de nome ou de sigla entre eleições (PSL+DEM → UNIÃO;
  `PC do B` → `PCDOB`; PATRIOTA+PTB → PRD) vira entidades distintas.
- Receita removida pelo TSE numa publicação posterior **continua aberta** (a
  fonte não é snapshot). Receita corrigida (outro doador ou outra data no
  mesmo `SQ_RECEITA`) fica como na primeira publicação e conta em
  `receipt_conflict`. Um `attrs` mudado (valor, itens, resultado da eleição)
  **não** é regravado: `attrs` é imutável.
- Nome ou CPF corrigido pelo TSE para o mesmo `SQ_CANDIDATO` não muda a
  ficha: vale a primeira publicação. Uma candidatura carregada antes de o
  CPF ser publicado fica sem fragmento.
- Candidatos de 2024 (sem CPF publicado) nunca se juntam automaticamente a
  outras fichas; ficam para revisão manual.
- Doações via financiamento coletivo e os doadores originários de
  repasses não são carregados.
- Receitas da prestação de contas **de partidos e comitês** (outro conjunto
  do TSE) não são carregadas.
