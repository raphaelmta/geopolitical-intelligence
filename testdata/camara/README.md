# Câmara test fixture

Responses of the Câmara dos Deputados open-data API (REST v2) recorded on
2026-11-12, trimmed to six deputies of the 56th and 57th legislatures and
scrubbed of personal data. The tests serve them with `httptest` at the
routes the connector calls (see `docs/sources/camara.md`); the file names
are the names the connector stores them under in the raw store.

## What is real

Federal deputies are public figures and these are official records of their
public office, so the following are **real**, as the API returned them:
deputy ids, civil and parliamentary names, party siglas, states,
legislature numbers and dates, and every history event (date and time,
situation, electoral condition, description, party, state). The legislature
files (`legislaturas_56.json`, `legislaturas_57.json`) are unchanged; the
parties lists keep real siglas and names.

## What was changed

- **CPF**: replaced by an invented value `900` + a chosen fragment + two
  digits, with **invalid check digits** (`90011111100`, `90022222201`,
  `90033333300`, `90044444401`, `90055555500`, `90066666600`): fragments
  `111111` … `666666`. No real CPF is in the repository.
- **Set to `null` or emptied**: e-mail (list items, history events, last
  status, office), photo URL, birth date, death date, birth state and
  municipality, education, sex, website, social networks (`[]`), and every
  office field (name, building, room, floor, phone, e-mail).
- **Lists**: `deputados_<n>_p<k>.json` keep only the items of the six
  deputies (in `id` order, as the API returns with `ordenarPor=id`); the
  57th is split in two pages (4 + 3 items) and the `links` were rewritten to
  describe two pages (`next` on page 1, `previous` on page 2), so that the
  pagination is exercised. The 56th is one page without `next`.
  `partidos_<n>_p1.json` keep only the parties these deputies had in the
  56th and 57th (one page, links rewritten).
- Pretty-printed (indented) JSON; field order unchanged.

Malformed cases (an item without `id`, an event with an impossible date, a
history that is not a list) are added by the tests in memory, not stored
here.

## Deputies

Expected `holds_mandate` with legislatures 56 and 57 and a reference date
after 2026-11-12. Ends are exclusive (the first day the stretch no longer
holds); `ex` = exercicio, `lic` = licenciado, `sup` = suplente_exercicio.

| id | Covers | Expected |
|---|---|---|
| 204379 | mid-term party change; term end at 23:59; current | 56 PROS ex 2019-02-01..2022-04-11; 56 MDB ex 2022-04-11..2023-02-01; 57 MDB ex from 2023-02-01 (open) |
| 74159 | resignation (`VACANCIA`, Renúncia); older legislatures ignored | 56 PT ex 2019-02-01..2023-02-01; 57 PT ex 2023-02-01..2026-05-19 |
| 100689 | substitute in office, then automatic leave | 57 REPUBLICANOS sup 2023-12-12..2024-04-11 |
| 204511 | leave (Afastamento Conjunto Consecutivo), party change during the leave, return; party change in the 57th | 56 DEM ex 2019-02-01..2021-11-11; 56 DEM lic 2021-11-11..2022-02-23; 56 UNIÃO lic 2022-02-23..2022-03-13; 56 UNIÃO ex 2022-03-13..2023-02-01; 57 UNIÃO ex 2023-02-01..2026-04-08; 57 PODE ex from 2026-04-08 (open) |
| 204503 | minister (Ministro de Estado): on leave at the end of the 56th without end event; leave, return, leave again (current) | 56 PT ex 2019-02-01..2023-01-01; 56 PT lic 2023-01-01..2023-02-01; 57 PT ex 2023-02-01..2023-02-02; 57 PT lic 2023-02-02..2025-01-31; 57 PT ex 2025-01-31..2025-02-03; 57 PT lic from 2025-02-03 (open) |
| 74283 | titular in the 56th; in the 57th the substitute in office during 204503's leaves | 56 PT ex 2019-02-01..2023-02-01; 57 PT sup 2023-02-03..2025-01-31; 57 PT sup 2025-02-03..2026-03-31 |

So a full load reads 14 list items, skips none, and writes 6 person
records, 7 named parties and 21 `holds_mandate` claims (3 open, 18 closed).
