# CVM — Companhias abertas: posição acionária do Formulário de Referência

Conferido em 2026-11-12, baixando os arquivos do Portal de Dados Abertos da
CVM (`https://dados.cvm.gov.br`) e os dicionários de dados publicados ao lado
(`META/meta_fre_cia_aberta.zip`, `META/meta_cad_cia_aberta.txt`), e lendo
cabeçalhos, valores e contagens (nenhum dado pessoal foi copiado para o
repositório).

## Onde ficam os arquivos

| Conjunto | URL | Usado? |
|---|---|---|
| Formulário de Referência (FRE), um zip por ano | `https://dados.cvm.gov.br/dados/CIA_ABERTA/DOC/FRE/DADOS/fre_cia_aberta_<ANO>.zip` | sim |
| Informação Cadastral | `https://dados.cvm.gov.br/dados/CIA_ABERTA/CAD/DADOS/cad_cia_aberta.csv` | não (ver abaixo) |

Base padrão: `https://dados.cvm.gov.br/dados/CIA_ABERTA/DOC/FRE/DADOS/`
(variável `CVM_BASE_URL` sobrepõe). Os nomes são determinísticos; não é
preciso listar o diretório. O portal (atrás do Cloudflare) responde `HEAD`
com `Content-Length`, `ETag` e `Accept-Ranges: bytes`; o conector confere o
tamanho e retoma com `Range`/`If-Range`. Um ano não publicado responde 404.
Só um `User-Agent` genérico é enviado.

A CVM **regera os mesmos nomes de arquivo toda semana** (em 2026-11 os zips
de 2022 a 2026 tinham `Last-Modified` 2026-11-08; o cadastro, 2026-11-11).
Não há edição datada: o conector não resolve edição (não implementa
`EditionResolver`) e usa a data de referência do pedido como data da coleta;
a chave do raw store vem dos validadores do arquivo no servidor (HEAD:
ETag, Last-Modified, tamanho), `raw/cvm/<ANO>/<id dos validadores>/fre_cia_aberta_<ANO>.zip`:
um zip não regerado desde a última coleta não é baixado nem guardado de novo
(a coleta nova lê o mesmo objeto e o registra no seu `collection_file`); um
regerado ganha chave nova e o antigo fica. Sem validadores, cada data de
referência guarda sua cópia em `raw/cvm/<data>/fre_cia_aberta_<ANO>.zip`.

Tamanhos em 2026-11-12: `fre_cia_aberta_2025.zip` 8,5 MB,
`fre_cia_aberta_2026.zip` 8,2 MB (anos 2019–2024 entre 8 e 10 MB);
`cad_cia_aberta.csv` 1,5 MB.

### Anos coletados

O FRE de um ano é entregue ao longo desse ano (a entrega anual vai até
maio). Por isso o conector coleta o ano da data de referência **e o ano
anterior** (`DefaultYearsBack = 1`): a companhia que ainda não entregou o
FRE do ano corrente continua coberta pelo do ano anterior
(`companies_from_previous_year` conta quantas).

Zip do **ano de referência** ausente (404, típico do começo de janeiro): o
`Fetch` devolve só o do ano anterior e a carga decide:

- se alguma coleta CVM anterior **concluída** já registrou a entrada
  `fre_cia_aberta_posicao_acionaria_<ANO>.csv` daquele ano em
  `collection_file`, a ausência é falha de publicação, não ano novo: a
  coleta **falha** antes de gravar qualquer coisa (carregar reverteria toda
  companhia ao formulário do ano anterior e `CloseUnseen` fecharia vínculos
  de forma irreversível);
- senão, carrega só o ano anterior e grava a estatística
  `year_not_published = <ANO>`.

404 no ano anterior falha a coleta sempre.

## Entradas do zip e o que é carregado

`fre_cia_aberta_<ANO>.zip` tem 34 entradas CSV (2026), uma por quadro do
formulário: `fre_cia_aberta_<ANO>.csv` (índice dos documentos entregues,
**todas** as versões), `..._posicao_acionaria_<ANO>.csv`,
`..._posicao_acionaria_classe_acao_<ANO>.csv`,
`..._distribuicao_capital_<ANO>.csv`, `..._capital_social_<ANO>.csv`,
`..._administrador_membro_conselho_fiscal_<ANO>.csv` etc.

Só é lida a entrada **`fre_cia_aberta_posicao_acionaria_<ANO>.csv`**
(item 15.1/15.2 do FRE), por nome exato, direto do zip guardado (sem
extrair). As demais são ignoradas sem ser lidas. Limites: zip de até 1 GiB
e entrada de até 1 GiB descomprimida (tamanho declarado conferido antes e
bytes lidos contados durante a leitura).

`fre_cia_aberta_posicao_acionaria_<ANO>.csv` traz **uma única versão por
companhia** (a mais recente entregue no ano: 656 companhias e 656
documentos em 2026; 700 em 2025), ao passo que o índice
`fre_cia_aberta_<ANO>.csv` lista todas as versões. O conector, mesmo assim,
aplica a regra "último formulário" (abaixo) para não depender disso.

## Formato do CSV

- separador `;`, **sem aspas** — aspas são texto comum (há nomes reais como
  `CPFL Comercialização Brasil S.A. ("CPFL Brasil")`); um `;` dentro de um
  campo não ocorre (todas as 31.572 linhas de 2026 têm 29 campos);
- primeira linha é o cabeçalho; finais de linha **CRLF**; codificação
  **Latin-1** (ISO-8859-1); um BOM UTF-8 (bytes `EF BB BF`) no início, se
  aparecer, é removido antes da decodificação;
- números com **ponto** decimal e 6 casas (`51.370000`) — não a vírgula
  brasileira; datas `AAAA-MM-DD`;
- CNPJ e CPF com pontuação (`00.000.000/0001-91`, `000.000.000-00`).

Colunas (29, mesma ordem em 2025 e 2026): `CNPJ_Companhia`,
`Data_Referencia`, `Versao`, `ID_Documento`, `Nome_Companhia`,
`ID_Acionista`, `Acionista`, `Tipo_Pessoa_Acionista`, `CPF_CNPJ_Acionista`,
`ID_Acionista_Relacionado`, `Acionista_Relacionado`,
`Tipo_Pessoa_Acionista_Relacionado`, `CPF_CNPJ_Acionista_Relacionado`,
`Quantidade_Acao_Ordinaria_Circulacao`, `Percentual_Acao_Ordinaria_Circulacao`,
`Quantidade_Acao_Preferencial_Circulacao`,
`Percentual_Acao_Preferencial_Circulacao`,
`Quantidade_Total_Acoes_Circulacao`, `Percentual_Total_Acoes_Circulacao`,
`Nacionalidade`, `Sigla_UF`, `Residente_Exterior`, `Representante_Legal`,
`Tipo_Pessoa_Representante_Legal`, `CPF_CNPJ_Representante_legal`,
`Data_Composicao_Capital_Social`, `Data_Ultima_Alteracao`,
`Acionista_Controlador`, `Participante_Acordo_Acionistas`.

As colunas são lidas **pelo nome do cabeçalho**; a falta de qualquer uma das
usadas (tabela abaixo) falha o arquivo com o nome da coluna.

| Coluna | Uso |
|---|---|
| `CNPJ_Companhia` | companhia aberta: `CNPJBase` → entidade `company` |
| `Nome_Companhia` | nome da companhia, se a entidade ainda não existir |
| `Data_Referencia`, `Versao`, `ID_Documento` | identificam o formulário (regra do último formulário) |
| `ID_Acionista` | identifica a linha no formulário (pai das linhas indiretas; desempate de duplicadas) |
| `ID_Acionista_Relacionado` | preenchido = linha indireta: o `ID_Acionista` da linha do acionista que ela descreve |
| `Acionista` | nome do acionista |
| `Tipo_Pessoa_Acionista` | `PF`, `PJ` ou vazio (vazio só nas linhas agregadas) |
| `CPF_CNPJ_Acionista` | CNPJ (PJ) ou CPF **completo** (PF) — ver Privacidade |
| `Percentual_Total_Acoes_Circulacao` | **percentual no capital total**: `attrs.percent` |
| `Percentual_Acao_Ordinaria_Circulacao` | `attrs.percent_on` |
| `Percentual_Acao_Preferencial_Circulacao` | `attrs.percent_pn` |
| `Residente_Exterior`, `Nacionalidade` | só para decidir "estrangeiro sem documento" |
| `Acionista_Controlador` | `S`/`N`: `S` cria `controls` |

Apesar do sufixo `_Circulacao` no nome, os percentuais são a participação
no capital (a soma das linhas diretas de cada companhia dá 100 % em 655 das
656 companhias de 2026; a exceção repete o mesmo documento três vezes, ver
Limitações).

Não são lidos: quantidades de ações, `Sigla_UF`, representante legal (nome,
tipo, CPF/CNPJ), datas de composição e de última alteração, participação em
acordo de acionistas, e as colunas `Acionista_Relacionado`,
`Tipo_Pessoa_Acionista_Relacionado` e `CPF_CNPJ_Acionista_Relacionado` (o
acionista relacionado é identificado pela linha-pai, via
`ID_Acionista_Relacionado`; o CPF dessa coluna nunca é lido).

## Linhas diretas, indiretas e agregadas

Cada formulário lista primeiro os acionistas **diretos** da companhia
(`ID_Acionista_Relacionado` vazio) e depois, para cada acionista
controlador, a composição do capital **dele** (`ID_Acionista_Relacionado`
= o `ID_Acionista` daquela linha-pai, no mesmo documento; o
`CPF_CNPJ_Acionista_Relacionado` repete o documento da linha-pai), descendo
a cadeia até pessoas físicas — até 15 níveis nos arquivos de 2026. Em 2026:
4.512 linhas diretas e 27.060 indiretas. Os percentuais das linhas
indiretas são relativos ao capital da **holding** descrita, não da
companhia aberta.

### Regra das linhas indiretas

1. **Holding** = o acionista da linha-pai. Só pessoa jurídica com CNPJ
   descreve holding; linhas sob pai pessoa física, sem documento ou
   malformado são puladas (`indirect_parent_not_company`). Pai inexistente
   no formulário: `indirect_parent_missing`. Os ancestrais do pai são
   seguidos até 20 níveis (`indirect_too_deep`) com proteção contra ciclo
   (`indirect_cycle`).
2. **Uma descrição por holding**, escolhida na carga inteira:
   - dentro de um formulário, a mesma holding sob duas linhas-pai é
     descrita pelo grupo **completo** (soma 100, com tolerância de ±0,01)
     antes de um incompleto (por exemplo todo zerado, "não informado"), e
     entre iguais pela de menor `ID_Acionista`
     (`indirect_duplicate_description` para as linhas das outras; se uma
     outra difere, `indirect_duplicate_conflict`) — nos arquivos reais 929 holdings
     aparecem assim e 243 diferem;
   - holding que é ela mesma companhia aberta com formulário atual: valem
     as linhas **diretas** do próprio formulário; descrições feitas por
     outros são ignoradas (`indirect_superseded_by_own_form`);
   - senão, vale a descrição do formulário mais recente (data de
     referência, versão, documento, depois o menor CNPJ base de quem
     descreve); as outras são ignoradas (`indirect_superseded`), e cada uma
     que difere (acionistas, percentuais ou controle) conta
     `indirect_conflict`.
   A escolha não depende da ordem dos arquivos nem das linhas, e garante
   que a chave natural (`shareholder_of`, acionista, holding, papel) é
   afirmada por uma só fonte — sem fechar/abrir em vaivém entre
   formulários.
3. Grava `shareholder_of` acionista → holding (entidade `company` pela
   base do CNPJ, criada com o nome do arquivo se não existir) e `controls`
   quando `Acionista_Controlador = S`, com `attrs.described_by` = base do
   CNPJ da companhia aberta cujo formulário forneceu a descrição.
4. `attrs.percent` (e `percent_on`/`percent_pn`) **só** quando os
   percentuais totais do grupo (todas as linhas daquela holding na
   descrição escolhida, agregadas incluídas) somam 100 ± 0,01, em aritmética
   decimal exata. Senão, nenhum percentual: grupo todo zerado significa "não
   informado", e gravar `"0"` seria falso. Nos arquivos reais, 4.721 de
   7.072 grupos somam 100, 2.348 são todo zero, 3 outros valores.
5. Identidade do acionista como nas linhas diretas, com a holding no lugar
   da companhia aberta: ficha de pessoa com `context_key` =
   `cvm:<base da holding>`, estrangeiro `cvm:<base da holding>:<nome>`;
   agregadas puladas; a própria holding como acionista: `self_holding`.

Linhas agregadas — não são acionistas; contadas e puladas quando não têm
documento (vazio ou só zeros), comparando o nome normalizado:

| Rótulo (nome normalizado) | Estatística | Observado |
|---|---|---|
| `OUTROS`, ou começando por `OUTROS ` (ex.: `Outros (registered shareholders)`) | `aggregate_outros` | `Outros` com tipo vazio em toda companhia; poucas linhas `Outros` com tipo `PJ` e sem documento (as `Outros` com tipo `PF` e CPF são todas indiretas) |
| `ACOES TESOURARIA`, `ACOES EM TESOURARIA` | `aggregate_tesouraria` | `Ações Tesouraria` com tipo vazio em toda companhia |
| `TOTAL` | `aggregate_total` | não observado; reservado |

## Último formulário por companhia

Para cada companhia (base do CNPJ) vale **só o último formulário
entregue** entre todos os anos lidos: a maior `Data_Referencia`; empate, a
maior `Versao`; depois o maior `ID_Documento`; depois o zip mais recente.
Linhas de formulários e versões anteriores são contadas em
`superseded_rows` e ignoradas.

`Data_Referencia` do FRE é uma convenção do formulário, não a data da
posição: quase todo FRE 2026 tem `2026-12-31` (também há `2026-03-31`,
`2026-06-30` etc.). Por isso ela é usada para ordenar formulários e é gravada
em `attrs.reference_date`, mas **não** como data de validade dos vínculos.

A fonte é catalogada como snapshot: depois de cada carga completa,
`CloseUnseen` fecha todo vínculo da CVM não reafirmado. Como cada carga
reafirma o conjunto completo de acionistas do último formulário de cada
companhia, uma carga semanal normal reafirma praticamente tudo (carga real
de 2026-11-12 repetida: 9.866 vínculos vistos de novo, 0 substituídos, 0 fechados).

## Modelo gravado

| Dado | Gravação |
|---|---|
| Companhia aberta | entidade `company`, `natural_key` = base do CNPJ (`identity.CNPJBase`, a mesma chave da Receita); criada com `Nome_Companhia` se não existir (o nome já existente prevalece). Nenhuma `company_version` é escrita (são da Receita). |
| Acionista PJ com CNPJ | entidade `company` pela base do CNPJ (criada se não existir) |
| Acionista PF | `person_record` com `context_key` = `cvm:<cnpj_basico da companhia aberta>`, nome e fragmento do CPF; a mesma pessoa em duas companhias são duas fichas, que a resolução de identidade pode unir por nome + fragmento |
| Estrangeiro sem documento | entidade `foreign`, `natural_key` = `cvm:<cnpj_basico da companhia aberta>:<nome normalizado>` (como os sócios estrangeiros da Receita, identificados pelo nome dentro de uma empresa). "Sem documento" = vazio ou `00.000.000/0000-00` (o preenchimento que a CVM usa para estrangeiros); "estrangeiro" = `Residente_Exterior = S` ou `Nacionalidade` preenchida e diferente de `Brasil` (há estrangeiros com `Residente_Exterior = N`, ex.: holding espanhola). Vale para PJ e para PF sem CPF. |
| PJ sem CNPJ e não estrangeira, linha sem nome nem documento, tipo desconhecido | não carregada: `unidentified_shareholder` |
| A própria companhia (ou holding) como acionista | não carregada: `self_holding` |
| Mesmo acionista duas vezes no grupo | vale a linha de menor `ID_Acionista` (independe da ordem): `duplicate_shareholder`; se os valores diferem, também `duplicate_conflict` |

Vínculos (todos `kind = fact`, `valid_from` NULL — o formulário não diz
desde quando):

| Vínculo | Direção | `attrs` |
|---|---|---|
| `shareholder_of` | acionista → companhia aberta ou holding | `role` = `"shareholder"`, `percent` (capital total), `percent_on`, `percent_pn`, `reference_date`, `form_version` (número); linhas indiretas: + `described_by`, e percentuais só com grupo completo |
| `controls` | acionista → companhia aberta ou holding, só com `Acionista_Controlador = S` | `role` = `"controller"`, `reference_date`, `form_version` (+ `described_by` nas indiretas) |

Percentuais: texto decimal canônico com ponto, dígitos exatos (nunca
`float`): `51.370000` → `"51.37"`, `100.000000` → `"100"`,
`0.000000` → `"0"`. O arquivo arredonda a 6 casas: `"0"` significa abaixo
de 0,0000005 %, não exatamente zero. Aceita-se também vírgula decimal. Valor vazio,
negativo, acima de 100 ou com outro caractere torna a linha malformada.
`internal/graph` (modo `documented`) lê `attrs.percent` e segue
`shareholder_of` acima de 50 % e todo `controls`; sem `percent`, só o
`controls` sobe a cadeia.

`record_ref` = `<entrada>:<linha física>` (cabeçalho = linha 1), por exemplo
`fre_cia_aberta_posicao_acionaria_2026.csv:2`; a entrada é registrada em
`collection_file` com a chave e o SHA-256 do zip guardado.

### Mudança de percentual ou de controle

`attrs` de um vínculo é imutável. Quando o último formulário traz, para a
mesma chave natural (tipo, acionista, companhia, papel, `valid_from` NULL),
`percent`, `percent_on` ou `percent_pn` diferentes do vínculo aberto, o
carregador, na mesma transação curta da companhia:

1. fecha o vínculo aberto nesta coleta: `closed_collection_id` = coleta
   atual, `valid_to_after` = data de referência da coleta que o viu por
   último, `valid_to_before` = data de observação da coleta atual (o dia em
   que ela começou, UTC) + 1 dia, exclusivo (os mesmos limites que
   `CloseUnseen` usa);
2. insere o novo vínculo aberto com os novos `attrs`.

Sem mudança, o vínculo só é marcado como visto de novo
(`last_seen_collection_id`); `reference_date`/`form_version`/`described_by`
continuam os do formulário que o declarou primeiro. Um grupo indireto que
deixa de somar 100 perde o `percent`, o que também é mudança (fecha e
reabre). Só vínculos `fact` cuja primeira coleta
é da CVM são fechados assim. Quem deixa de ser controlador simplesmente não
tem o `controls` reafirmado, e `CloseUnseen` o fecha.

## Linhas malformadas

Contadas por motivo em `rows_skipped` e puladas, nunca deslocadas; o log
traz arquivo, linha e motivo, nunca o conteúdo:

| Motivo | Quando |
|---|---|
| `malformed_columns` | número de campos diferente do cabeçalho |
| `malformed_line` | linha acima de 64 KiB |
| `malformed_cnpj` | CNPJ da companhia ou do acionista PJ ilegível |
| `malformed_date`, `malformed_version`, `malformed_document` | `Data_Referencia`, `Versao`, `ID_Documento` ilegíveis |
| `malformed_shareholder_id` | `ID_Acionista` ou `ID_Acionista_Relacionado` ilegível |
| `malformed_percent` | percentual total (ou ON/PN preenchido) inválido ou fora de 0–100 |
| `duplicate_shareholder_id` | o mesmo `ID_Acionista` em duas linhas do mesmo formulário: vale a de menor número de linha, as outras são puladas (independe da ordem de leitura) |

Linhas que não identificam o formulário (CNPJ da companhia, data, versão,
documento) são malformadas sempre; as demais só contam como malformadas se
pertencerem ao último formulário da companhia (as de formulários antigos são
`superseded_rows`). Estas também contam em `latest_form_lines_skipped`:
o vínculo que viria delas não é reafirmado e `CloseUnseen` o fecha.

## Privacidade

- **A CVM publica o CPF completo** dos acionistas pessoas físicas (9.890
  linhas em 2026, formato `000.000.000-00`; nenhum mascarado). O parser
  reduz o CPF ao fragmento de 6 dígitos (`identity.CPFFragment`) e descarta
  o resto na hora; o CPF nunca vai para `record_ref`, `attrs`, logs ou
  erros. CPF ausente ou ilegível (7 PF com 14 dígitos em 2026) → fragmento
  vazio.
- Nenhum outro dado pessoal é carregado: nem UF, nem nacionalidade (só
  decide "estrangeiro"), nem representante legal, nem datas da posição, nem
  acordo de acionistas.
- Fixtures (`testdata/cvm`) são sintéticas; ver o README de lá.

## Informação Cadastral (`cad_cia_aberta.csv`) — não carregada

Conferido: 2.679 linhas, Latin-1, CRLF, `;`, 47 colunas (`CNPJ_CIA`,
`DENOM_SOCIAL`, `SIT`, `CONTROLE_ACIONARIO`, endereço, responsável com
e-mail etc.), todas as companhias já registradas, inclusive canceladas. Não
é carregado: os nomes das companhias vêm do próprio FRE, situação e
endereço já vêm da Receita, e o arquivo traz dados pessoais do responsável
pelo relacionamento com investidores.

## Carga de teste (arquivos reais de 2026-11-12)

FRE 2025 + 2026, com a cadeia indireta: 63.079 linhas lidas, 0
malformadas, 30.249 de formulários substituídos; 708 companhias (52 só com
o FRE do ano anterior), 2.028 holdings descritas; 6.466 `shareholder_of`
(3.328 diretos, 3.138 indiretos, dos quais 3.108 com percentual) e 3.400
`controls`; 3.556 fichas de pessoa, 218 entidades `foreign`. Linhas
indiretas puladas: 3.663 sob pai que não é PJ com CNPJ, 8.027 descrições
repetidas no mesmo formulário (350 repetições divergentes), 2.806 de
holdings que são companhias abertas, 6.366 de descrições substituídas (419
conflitantes). Primeira carga 36 s; repetição 9 s com 0 inseridos, 0
substituídos e 0 fechados.

## Limitações conhecidas

- Holding descrita de forma inconsistente (por formulários diferentes, ou
  em dois pontos do mesmo formulário) mostra só a visão escolhida pela
  regra — a do formulário mais recente; as outras ficam contadas, não
  gravadas.
- Percentual de linha indireta ausente quando o grupo não soma 100 (em
  geral, não informado); o vínculo existe, mas só `controls` leva controle.
- Holdings estrangeiras sem CNPJ não são descritas: as linhas sob elas
  ficam de fora (`indirect_parent_not_company`).
- O mesmo documento pode aparecer repetido dentro da posição acionária (uma
  companhia em 2026 tem cada acionista três vezes, com `ID_Acionista`
  diferentes e os mesmos valores): fica a linha de menor `ID_Acionista`.
- Estrangeiros sem documento são uma entidade por companhia (aberta ou
  holding) que detêm (o mesmo fundo em duas companhias são duas entidades
  `foreign`).
- `reference_date`/`form_version`/`described_by` de um vínculo inalterado
  continuam os do primeiro formulário que o declarou.
- Uma linha malformada do último formulário some daquela coleta, e
  `CloseUnseen` fecha o vínculo correspondente.
- Se o zip do ano de referência sumir depois de já ter sido carregado, a
  coleta falha até ele voltar (de propósito).
- Cada arquivo carregado guarda em `collection_file` a URL do zip
  (`source_url`). Uma coleta **falha** anterior que registrou o arquivo do
  ano de referência não torna esse ano obrigatório: só uma coleta concluída
  (`done`) torna.
