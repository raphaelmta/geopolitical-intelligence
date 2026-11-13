# CVM test fixture

`fre_cia_aberta_posicao_acionaria_<ANO>.csv` follow the real format of the
shareholding entry of the CVM's Formulário de Referência zips (see
`docs/sources/cvm.md`): the real header (29 columns, real names and order,
identical in 2025 and 2026), `;` between fields, **no quoting**, **CRLF**
line ends, **Latin-1** encoding, decimals with a dot and 6 places, dates
`AAAA-MM-DD`. Tests zip each one on the fly as `fre_cia_aberta_<ANO>.zip`,
together with other FRE entries (the document index, the share-class
position, the capital distribution, the board members) that the loader must
ignore.

**Everything is synthetic.** Every company, person, CNPJ, CPF, document id
and shareholder id was invented. Every CPF has **invalid check digits**, so
none can be a real person's CPF. Every CNPJ base starts with `9899`, the
range the Receita fixture uses (no company in the real files); `98990001`
is the invented "ALFA PARTICIPAÇÕES S.A." of `testdata/receita`, so a test
can check that the CVM finds the Receita's entity. Only the format, the
aggregate labels (`Outros`, `Ações Tesouraria`,
`Outros (registered shareholders)`), the all-zero CNPJ used for foreign
holders and the country names are real.

The CSVs are marked `binary` in `.gitattributes`: git must not rewrite their
encoding or line ends. Line numbers below are physical lines, the header
being line 1 (what `record_ref` stores).

## Companies and forms

| Base | Company | Forms in the fixture | Loaded form |
|---|---|---|---|
| 98990001 | ALFA PARTICIPAÇÕES S.A. | 2025-12-31 v3 (2025 file), 2026-12-31 v2 (2026 file) | 2026 v2: declared controllers |
| 98991001 | BETA ENERGIA S.A. | 2026-12-31 v1 and v2 (2026 file) | v2: no controller |
| 98991002 | GAMA SANEAMENTO S.A. | 2025-06-30 v4 and 2025-12-31 v1 (2025 file); not in 2026 | 2025-12-31 v1 (later date beats higher version); covered by the previous year |

## `fre_cia_aberta_posicao_acionaria_2026.csv`

| Line | Holder | Expected |
|---|---|---|
| 2 | ALFA HOLDING S.A. (PJ 98991101) in Alfa, 51.370000 %, controller `S` | `shareholder_of` percent `51.37` + `controls` |
| 3 | ANA FICTÍCIA CONTROLADORA (PF, CPF 987.654.321-01) in Alfa, 10 %, controller `S` | person record `cvm:98990001`, fragment `654321`; `shareholder_of` + `controls` |
| 4 | FICTITIOUS CAPITAL LLC (PJ, CNPJ `00.000.000/0000-00`, resident abroad) in Alfa, 8 % | `foreign` `cvm:98990001:FICTITIOUS CAPITAL LLC` |
| 5, 6 | `Ações Tesouraria`, `Outros` (24.5 %) | aggregates, not loaded |
| 7, 8 | EDUARDO INVENTADO INDIRETO (PF, CPF 123.123.123-00, 30 %) and `Outros` (0), holders of ALFA HOLDING (parent 7000001) | indirect: person record `cvm:98991101`, `shareholder_of` 30 into the holding |
| 9, 10 | Beta version 1 (Delta 40 %) | superseded |
| 11 | DELTA PARTICIPAÇÕES LTDA (PJ 98991102) in Beta, 30 % | `shareholder_of` |
| 12 | BRUNO EXEMPLO TESTE (PF, CPF 321.654.987-00) in Beta, 20 % | person record, fragment `654987` |
| 13 | CARLA SEM DOCUMENTO (PF, no CPF) in Beta, 5 % | person record, empty fragment |
| 14 | INVENTADA HOLDING, S.L. (PJ, no document, nationality Espanha, `Residente_Exterior = N`) | `foreign` `cvm:98991001:INVENTADA HOLDING S L` |
| 15 | DELTA again (same values, another `ID_Acionista`) | `duplicate_shareholder` |
| 16 | 28 fields | `malformed_columns` |
| 17 | percent `12,5x` | `malformed_percent` |
| 18 | percent `150.000000` | `malformed_percent` |
| 19 | shareholder CNPJ `12.345/0001` | `malformed_cnpj` |
| 20 | company CNPJ `XX` | `malformed_cnpj` |
| 21, 22 | `Ações Tesouraria`, `Outros` (31.5 %) | aggregates |
| 23 | GAMA SANEAMENTO S.A. (listed, PJ 98991002) in Alfa, 5 % | direct `shareholder_of` |
| 24 | ALFA CONTROLE LTDA (PJ 98991108) holds 70 % of ALFA HOLDING, controller `S` | indirect; with lines 7, 8 the group sums to 100: percent `70` + `controls`, `described_by` 98990001 |
| 25, 26 | FERNANDA FICTÍCIA NETA (PF, CPF 741.852.963-00, 100 %, controller) and `Outros` 0, holders of ALFA CONTROLE (parent 7000008, itself indirect) | nested level: record `cvm:98991108`, percent `100` + `controls` |
| 27, 28 | OMEGA 99 % and `Outros` 1 %, holders of GAMA (parent 7000023) | Gama is listed with its own form: `indirect_superseded_by_own_form` |
| 29 | OMEGA PARTICIPAÇÕES LTDA in Beta, 10 % | direct |
| 30–32 | HELIO ZERO FICTÍCIO (PF, CPF 456.456.456-01, controller), GLOBAL ZERO FUND LP (foreign, `00.000.000/0000-00`, resident abroad), `Outros`, all 0 %, holders of DELTA (parent 7000111) | all-zero group: no percent attribute; `controls` Helio → Delta; `foreign` `cvm:98991102:GLOBAL ZERO FUND LP` |
| 33, 34 | OMEGA FUNDADOR LTDA (PJ 98991109) 90 % (not controller) and `Outros` 10 %, holders of OMEGA (parent 7000123) | Beta's description of Omega wins over Gama's older one: percent `90`, `described_by` 98991001 |
| 35 | FILHA DE PESSOA LTDA under CARLA (an individual) | `indirect_parent_not_company` |

## `fre_cia_aberta_posicao_acionaria_2025.csv`

| Line | Holder | Expected |
|---|---|---|
| 2, 3 | Alfa 2025 form (holding 50.1 %) | superseded by the 2026 form |
| 4 | OMEGA PARTICIPAÇÕES LTDA (PJ 98991103) in Gama, 75 %, controller `N` | `shareholder_of` 75; documented control by percent alone |
| 5 | ANA FICTÍCIA CONTROLADORA in Gama, 5 % | a second person record, context `cvm:98991002` |
| 6 | `Outros (registered shareholders)` (PJ, no document) | aggregate |
| 7 | `Ações Tesouraria` | aggregate |
| 8 | Gama 2025-06-30 v4 (Omega 70 %) | superseded (older reference date) |
| 9, 10 | OMEGA FUNDADOR 80 % and `Outros` 20 %, holders of OMEGA (parent 6000101) | Gama's description of Omega: `indirect_superseded` by Beta's newer form, and an `indirect_conflict` (80 ≠ 90) |
| 11 | ANA again in Gama, 6 %, higher `ID_Acionista` (6000107) | `duplicate_shareholder` + `duplicate_conflict`: line 5 (id 6000102, 5 %) is kept |
