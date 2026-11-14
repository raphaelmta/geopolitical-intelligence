# Load optimisation: measured diagnosis and design (phase 1)

Status: **in progress** — sections marked *pending* wait for a benchmark run.
The working log of every run is in the dispatch report
(`.superpowers/sdd/2026-11-11-fundacao-mapa-de-conexoes/perf-p1-report.md`,
not in the repository); this document holds the conclusions.

The first national load of the Receita registry took 26 h 20 min against a
12 h target ([`2026-national-load.md`](2026-national-load.md)). This document
says where the time goes, measured on the load benchmark
([`benchmark.md`](benchmark.md)), which hypotheses hold, and what to build, as
an ordered list of tasks. Nothing here is implemented yet: prototypes were
throwaway patches and hand-written SQL in the benchmark database only.

## 1. How it was measured

- Harness: `make bench-load` (commit cf37d1e). The real collection path
  (`geoint collect` -> worker: fetch, parse, load, CloseUnseen, Resolve, search
  rebuild) in the throwaway project `geoint-bench`, twice: first load, then a
  simulated next edition (98 % of the claims re-seen, 1-2 % new, 1-2 % gone).
- Sample: every company with head office in GO, 3.65 % of the country: 2.55 M
  companies, 1.03 M person records, 1.89 M relationships, 6.2 M rows read.
  **National = sample x 27.4** in rows.
- Two PostgreSQL profiles. `roomy` (shared_buffers 4 GB, everything in
  memory) shows CPU, trigger and WAL costs. `pressure` (shared_buffers 320 MB,
  container limited to 1100 MB, about one fifth of the database, as 8 GB +
  page cache were to the 96 GB national base) turns random access into real
  IO and reproduces the national behaviour.
- The machine is shared with the national stack (idle during the runs, 8 GB
  of shared buffers resident). **All numbers are relative**: compare runs of
  the same profile, never a benchmark second with a national hour.
- Docker root: btrfs, copy-on-write, `compress=zstd:1` (as in the national
  run), unless a run says No_COW.

## 2. Baseline profile (current code, d8e2cf7)

Seconds. `r` = roomy, `p` = pressure.

| phase | first load r | first load p | national (h) | re-load r | re-load p |
|---|---|---|---|---|---|
| parse + staging | 6 | 8 | 0.05 | 6 | 8 |
| companies | 208 | 223 | 1.93 | 153 | 161 |
| persons | 92 | 95 | 0.70 | 7 | 9 |
| relationships | 273 | 461 | 8.40 | 173 | 475 |
| CloseUnseen | 0 | 0 | 0 | 5 | 19 |
| Resolve | 96 | 798 | 14.11 | 5 | 38 |
| search rebuild | 87 | 84 | 0.69 | 83 | 87 |
| **collection** | **770** | **1681** | **26.3** | **442** | **813** |

`pressure` reproduces the national shape: load 47 % / Resolve 47 % / search
5 % (national 44 / 54 / 3); the cost of 64 relationship chunks grows from 23 s
to 45.5 s (x2.0; national x2.5); `IO/DataFileRead` is 34 % of the loader's
wait samples. Scaled by rows it predicts 12.8 h where 26.3 h were measured:
the national base is about twice as deep into cache pressure as the profile.
Projections below therefore use ratios per phase, not absolute seconds.

### 2.1 Top time consumers and the evidence for each root cause

**(1) Resolve re-points relationships row by row — 77 % of Resolve.**
`UPDATE relationship SET src_entity_id = ...` (35 batches, 583 k rows): 573 s
of the 798 s under pressure (46 s in memory), plus 45 s for the dst side.
It read 2.96 M blocks (435 s of read time) to change 583 k rows: every
re-pointed row is a non-HOT update (an indexed column changes; 0 HOT updates
of 584 k), so it gets a new heap tuple and a new entry in **each of the eight
indexes** of `relationship` at a random position, and pays the `guard_update`
row trigger. Block reads of the Resolve phase: relationship heap 1.86 M,
relationship indexes 1.52 M, person_record heap 1.20 M and indexes 0.84 M.
The merge itself (`UPDATE person_record SET entity_id`, 486 k rows, non-HOT)
is another 101 s (26 s in memory). Together 90 % of Resolve is the cost of
writing every person twice: first one entity per record, relationships
pointing at it, then moving 30 % of the records (312,720 of 1,034,365;
nationally 9.57 M of 27.9 M) and their relationships. The 312,720 entities
left empty are the same waste seen from the other side (national: 10.7 M).

**(2) Relationship insert — foreign-key checks and hash-ordered chunks.**
`INSERT INTO relationship` (1.89 M rows): 242 s in memory (128 us per row, 50
buffer hits and 8.5 WAL records per row), 375 s under pressure. Block reads of
the relationships phase under pressure, by relation:

| relation | share of the 2.87 M blocks read | cause |
|---|---|---|
| `entity` heap | 34 % | FK checks on `src_entity_id` / `dst_entity_id`: a `FOR KEY SHARE` row lock on the referenced tuple — a random heap read and a dirtied page |
| `person_record` heap | 17 % | FK check on `src_record_id`, and the chunk's read of the record's current entity |
| `entity_pkey` | 13 % | the FK checks' index probes |
| `relationship_natural_key` | 11 % | unique expression index, random insert position |
| `relationship_dst_entity_id_id`, `_src_entity_id_id`, `_src_record_id` | 17 % | random insert positions |
| `relationship` heap | 7 % | re-reads of evicted pages |
| `relationship_pkey`, `relationship_first_collection_id` | 0 % | append-only |

**The `DataFileRead` of the national relationship phase is mostly not index
pages of `relationship`: 64 % is the foreign-key checks** reading (and
row-locking, hence dirtying) `entity` and `person_record` at random. That part
is constant per chunk. The *growth* over the phase is relationship's four
random-insert indexes: 1 % of the reads in the first quarter of the phase,
41 % in the last, as they outgrow memory. Both have one cause: the 1,024
chunks are **hash buckets** of `cnpj_basico`, so each chunk touches every
table and index uniformly at random. Entity ids and person-record ids are
handed out in `cnpj_basico` order by the earlier phases; chunks cut as
*ranges* of company would turn all of these accesses into short, mostly
sequential ranges.

**(3) `company_version insert` and `person_record insert` are CPU-bound.**
177 s and 86 s in memory, 184 s and 88 s under pressure: no IO wait, no WAL
wait (WAL waits are 2 % of all samples). 69 us and 83 us per row, 22 and 32
buffer hits per row, 5 and 8 WAL records per row. Shares by cause: *pending
(run `nofk-roomy`)*.

**(4) The monthly re-load rewrites every re-seen row.** The two "seen" updates
are 58 % of the re-load in memory (255 of 442 s) and 55 % under pressure (449
of 813 s). `UPDATE company_version SET last_seen_collection_id` (2.52 M rows):
0.14 % HOT — the column is not indexed, but every row of a page is updated by
the same statement, so there is never room for the new version in the page —
6 WAL records per row, 1.7 GB of WAL. `UPDATE relationship ... last_seen`
(1.86 M rows): 6 % HOT, 1.85 GB of WAL, 1.76 M blocks read under pressure.
The database grows 21 % per re-load (3.56 -> 4.31 GB) until vacuum reclaims
the old versions. The update guard `guard_update` (two `to_jsonb(row)` and a
jsonb subtraction per updated row) is 19 us per call: 86 s of those 255 s.

**(5) The search rebuild is a constant** 84-87 s whatever the collection
changed (ranked table 44 s, token tables 25 s, indexes 10 s).

## 3. Hypotheses

*In progress: runs still to come are marked pending.* Controls: `roomy`
baseline 770 s first load / 442 s re-load; `pressure` same-day control 1602 s /
723 s.

| # | hypothesis | verdict | measured (sample) |
|---|---|---|---|
| H1 | resolve identities before the relationships are written | **confirmed** | Resolve 727 s -> 96 s (x7.6) under pressure with the unchanged resolver, run before the relationship phase; same decisions, proposals and merged groups |
| H2 | set-based batch machinery of Resolve | killed as a lever of its own | the per-batch statements are 16 s of 96 s (roomy) and 68 s of 798 s (pressure) |
| H3 | relationship insert: FK checks, index locality, chunk order | **confirmed (two causes)** | range-ordered chunks: phase 469 s -> 287 s under pressure, flat; FK checks are 86 % of the insert's CPU (242 s -> 33.5 s without them, roomy) |
| H4 | company_version / person_record insert | **confirmed: FK checks** | 177 s -> 13.9 s (x12.7) and 86 s -> 18.5 s (x4.6) without FK checks (roomy); parallelism pending |
| H5 | btrfs copy-on-write + zstd under pgdata | confirmed, moderate | No_COW: first load -5 % (busy host) to -14 % (quiet host), re-load -6 %; only the IO-bound phases move |
| H6 | PostgreSQL settings during the load | pending (WAL waits are 2 % of samples) | |
| H7 | incremental / cheaper search rebuild | incremental killed; skip + reuse confirmed | change detection 4 s; rebuild reusing name_norm and tokens about 22-25 s instead of 87 s |
| H8 | monthly re-load rewrites every row | **confirmed** | seen updates = 55-58 % of the re-load; guard 16-18 us per row -> 3.5-5 us with WHEN-clause triggers; ordered chunks: relationship phase 379 s -> 202 s; no-rewrite design pending |
