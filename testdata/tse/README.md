# TSE test fixture

`consulta_cand_<ANO>_<UF>.csv` and `receitas_candidatos_<ANO>_<UF>.csv`
follow the real format of the TSE open data (see `docs/sources/tse.md`):
the real header (50 and 60 columns, real names and order, identical in
every year from 2018 to 2026), `;` between fields, every field in double
quotes, **CRLF** line ends, **Latin-1** encoding. Tests zip them on the fly
per year (`consulta_cand_<ANO>.zip`,
`prestacao_de_contas_eleitorais_candidatos_<ANO>.zip`) together with the
`_BRASIL.csv` aggregate (built from the UF files, as the TSE does), a
`leiame` PDF placeholder and an original-donor and an expense entry, which
the loader must all ignore.

**All personal data is synthetic.** Every person name, CPF, voter
registration number, e-mail and birth date was invented. Every CPF has
**invalid check digits**, so none can be a real person's CPF. Company and
campaign CNPJs start with `9899`, the range the Receita fixture uses (no
company in the real files), and `98990001` is the invented "ALFA
PARTICIPAÇÕES S.A." of `testdata/receita`. `SQ_CANDIDATO` and `SQ_RECEITA`
values are invented. Party acronyms and names, offices, the municipality
code of Rio Branco (`01392`) and origin codes are real.

The CSVs are marked `binary` in `.gitattributes`: git must not rewrite their
encoding or line ends. Line numbers below are physical lines, the header
being line 1 (what `record_ref` stores).

## Years

| Year | Candidate CPF | Why |
|---|---|---|
| 2022 | full (one withheld, `-4`) | general election; national `_BR` file; two rounds |
| 2024 | absent (`-4` everywhere, as published) | municipal election; `SG_UE` is a municipality code |
| 2026 | full | the same candidate as 2022, to test merging across years |

## People

| Person | Where | Purpose |
|---|---|---|
| MARIANA FICTÍCIA DOS SANTOS | candidate 2022 (CPF), 2024 (no CPF), 2026 (CPF); donor in 2024 | merges 2022 + 2026 + the 2024 donor record by fragment `123456`; the 2024 candidacy stays apart |
| JOÃO PEDRO INVENTADO | candidate 2022, CPF `-4` | empty fragment |
| CARLOS EDUARDO EXEMPLO | candidate 2022, governor, two rounds (lines 4 and 5) | one candidacy, final result from line 5, date of the 1st round |
| ANA PAULA MODELO | candidate 2022, party `PC do B` | sigla upper-cased; donor to Mariana as "outros candidatos" |
| BEATRIZ EXEMPLAR SOUZA | candidate 2022, result `#NULO` | neighbour of the malformed rows |
| HELENA MARIA INVENTADA | presidential candidate 2022 (`_BR` file) | national file is loaded |
| PAULO HENRIQUE TESTE | mayoral candidate 2024 | receives a donation from Mariana |
| ROBERTO DOADOR FICTÍCIO | donor 2022 (twice to Mariana, once to Helena, once to an unknown candidacy), 2024 | repeated donations; `NM_DOADOR_RFB` differs from `NM_DOADOR` |
| LÚCIA DOADORA EXEMPLO | donor 2022 (one receipt in two items, lines 11 and 12), 2026 | multi-item receipt summed; merges across years |
| MARCOS SEM DOCUMENTO | donor 2022, CPF `-4` | donor record without fragment |
| JOSÉ CONCEIÇÃO BRANDÃO | donor 2022 to CARLOS (line 22), CPF `321.123.456-00` (invalid check digits) | same normalized name and CPF fragment (`123456`) as the Receita partner of `testdata/receita`: identity resolution joins the two sources (the e2e path politician → donor → company) |

## Receipts (`receitas_candidatos_2022_AC.csv`)

| Line | SQ_RECEITA | Origin | Expected |
|---|---|---|---|
| 2, 3 | 50001, 50002 | pessoa física | two donated_to (1500.00, 500.50) |
| 4 | 50003 | pessoa jurídica, CNPJ 98990001… | donated_to from company 98990001 (origin absent from real 2018+ files; see the doc) |
| 5 | 50004 | pessoa jurídica, CNPJ 98990077… | new company entity |
| 6 | 50005 | partido político, `PT` | donated_to from party PT |
| 7 | 50006 | recursos próprios | `own_resources` |
| 8 | 50007 | outros candidatos (Ana) | donated_to from Ana's candidacy record |
| 9 | 50008 | financiamento coletivo | `crowdfunding` |
| 10 | -1 | `#NULO` | `other_origin` |
| 11, 12 | 50009 | pessoa física, 2 items | one donated_to, amount 350.00, items 2 |
| 13 | 50010 | — | **malformed**: amount `12a,00` |
| 14 | 50011 | pessoa física to SQ 10001699999 | `candidate_unknown` |
| 15 | 50012 | pessoa física, CPF `-4` | donor record with empty fragment |
| 16 | 50015 | — | **malformed**: 61 columns |
| 17 | 50013 | partido político without sigla | `party_unknown` |
| 18 | 50014 | outros candidatos, unknown SQ | `donor_candidate_unknown` |
| 19 | 50017 | partido político without sigla, `NR_PARTIDO_DOADOR` 45 | donated_to from PSDB (the only 2022 sigla with 45) |
| 20, 21 | 50016 | pessoa física, the two lines name different donors | one donated_to from line 20's donor (150.00, items 2); `receipt_mixed_donor` |
| 22 | 50018 | pessoa física, JOSÉ CONCEIÇÃO BRANDÃO to CARLOS | donated_to (800.00); donor fragment `123456` |

`consulta_cand_2022_AC.csv` line 7 is **malformed** (49 columns) and line 9
has a **stray quote** inside the name.

So a full load reads 37 records (12 candidacies + 25 receipts), skips 4,
and writes 6 parties, 18 person records (9 candidacies, 9 donors) and 24
relationships (9 candidate_for, 15 donated_to).

## How it was made

Generated by a small script from the structure of the files published on
2026-11-11 (header, quoting, sentinels such as `#NULO` and `-4`, date and
decimal formats, origin codes). No row was copied from the real files.
