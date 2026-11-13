# Senado test fixture

Responses of the Senado Federal open-data API (Dados Abertos Legislativos,
JSON via `Accept: application/json`) recorded on 2026-11-12, trimmed to
ten senators of the 56th and 57th legislatures and scrubbed of personal
data. The tests serve them with `httptest` at the routes the connector
calls (see `docs/sources/senado.md`); the file names are the names the
connector stores them under in the raw store.

## What is real

Senators are public figures and these are official records of their public
office, so the following are **real**, as the API returned them: codes,
parliamentary and full names, parties and party names, states, mandates
(codes, legislatures and dates, titular and substitutes), exercise periods
and causes, affiliation dates, and the `Metadados` blocks.

## What was changed

- **Removed** from `IdentificacaoParlamentar`: e-mail, photo URL, official
  and personal page URLs, sex. (The Senado publishes no CPF.)
- **Lists**: `senadores_56.json` and `senadores_57.json` keep only the
  senators below, in the API's order. The API has no pagination.
- `senador_<código>_filiacoes.json` are unchanged.
- Pretty-printed (indented) JSON; field order unchanged.

Malformed cases (a senator without code, an exercise with an impossible
date) and the "affiliation not published yet" case are made by the tests
in memory, not stored here.

## Senators

Expected `holds_mandate` with legislatures 56 and 57 and a reference date
after 2026-11-12. Ends are exclusive (the API's last day + 1); `ex` =
exercicio, `lic` = licenciado, `sup` = suplente_exercicio.

| Código | Covers | Expected |
|---|---|---|
| 751 | titular, exercise ended by death; listed in the 57th with no exercise in it | 56 PSD ex 2019-02-01..2020-10-22 |
| 5322 | 8-year mandate cut at the start of the 56th; party change; leave (LCS) and return in the 57th; ends without party (`S/Partido`) | 56 PODE ex 2019-02-01..2021-04-08; 56 PL ex 2021-04-08..2023-02-01; 57 PL ex 2023-02-01..2025-12-10; 57 PL lic 2025-12-10..2026-04-10; 57 PL ex 2026-04-10..2026-09-10; 57 (no party) ex from 2026-09-10 (open) |
| 4642 | titular, leave (LCS), return, mid-term party change | 57 UNIÃO ex 2023-02-01..2024-06-20; 57 UNIÃO lic 2024-06-20..2024-10-18; 57 UNIÃO ex 2024-10-18..2026-03-30; 57 PL ex from 2026-03-30 (open) |
| 6366 | 1st substitute of 4642, in office during the leave | 57 UNIÃO sup 2024-06-20..2024-10-18 |
| 5016 | minister: repeated leaves (AFO), on leave now | 57 PT: ex 2023-02-01..02-04, lic ..2023-12-13, ex ..12-14, lic ..2025-02-01, ex ..02-03, lic ..2026-04-28, ex ..05-07, lic from 2026-05-07 (open) |
| 6369 | 1st substitute of 5016 | 57 PSD sup 2023-02-06..2023-12-13, 2023-12-14..2025-02-02, 2025-02-03..2026-04-29, from 2026-05-06 (open) |
| 4605 | titular on leave as minister (AFO), back on 2024-02-01 only to resign (REN) on 2024-02-20 | 57 PSB ex 2023-02-01..2023-02-03; lic ..2024-02-01; ex ..2024-02-21 (nothing open) |
| 6358 | 4605's 1st substitute, effective after the resignation but still "1º Suplente"; party changes | 57 sup PSB 2023-02-02..2024-02-01; PSB 2024-02-21..2024-04-24; PDT ..2026-03-04; PSB ..2026-07-31; PSB from 2026-10-06 (open) |
| 6359 | 4605's 2nd substitute | 57 PSB sup 2026-08-05..2026-10-07 |
| 5918 | 2nd substitute never in office (no `Exercicios`) | none; no person record, affiliations not fetched |

So a full load reads 13 list items (3 + 10), skips none, and writes 9
person records and 33 `holds_mandate` claims (5 open, 28 closed).
