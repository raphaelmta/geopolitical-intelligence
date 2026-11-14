# Load optimisation: measured diagnosis and design (phase 1)

The first national load of the Receita registry took 26 h 20 min against a
12 h target ([`2026-national-load.md`](2026-national-load.md)). This document
says where the time goes, measured on the load benchmark
([`benchmark.md`](benchmark.md)), which hypotheses hold, and what to build, as
an ordered list of tasks. **Nothing here is implemented**: prototypes were
throwaway patches and hand-written SQL in the benchmark database only, and
none of them is in the repository.

**In one paragraph.** Three things cost almost all of the time, and none of
them is the disk as such. (1) Identity resolution runs *after* the
relationships are written and then re-points them row by row: run before, the
same resolver is 7.6 times faster. (2) Relationship chunks are hash buckets,
so every chunk reads every table and index at random: cut as ranges of
company, the phase runs at in-memory speed and stops slowing down. (3) The
per-row foreign-key checks are 78–92 % of the CPU of the three big inserts and
64 % of the read IO of the relationship phase. With (1) and (2) alone — small,
safe changes that leave the final state identical — the sample's first load
goes from 1602 s to 796 s; adding a set-based replacement of the FK checks
(which needs the owner's decision) it goes to 326 s, and 283 s with four
parallel sessions. The monthly re-load, never measured nationally, is
dominated by rewriting every re-seen row: 723 s → 343 s with the same tasks,
and about 140 s with a design that stops rewriting them.

## 1. How it was measured

- Harness: `make bench-load` (commit cf37d1e). The real collection path
  (`geoint collect` → worker: fetch, parse, load, CloseUnseen, Resolve, search
  rebuild) in the throwaway project `geoint-bench`, twice: first load, then a
  simulated next edition (98 % of the claims re-seen, 1–2 % new, 1–2 % gone).
- Sample: every company with head office in GO, 3.65 % of the country: 2.55 M
  companies, 1.03 M person records, 1.89 M relationships, 6.2 M rows read.
  **National = sample × 27.4** in rows.
- Two PostgreSQL profiles. `roomy` (shared_buffers 4 GB, everything in
  memory) shows CPU, trigger and WAL costs. `pressure` (shared_buffers 320 MB,
  container limited to 1100 MB, about one fifth of the database, as 8 GB +
  page cache were to the 96 GB national base) turns random access into real
  IO and reproduces the national behaviour.
- Docker root: btrfs, copy-on-write, `compress=zstd:1`, as in the national
  run, unless a run says No_COW.
- **All numbers are relative.** The machine is shared with the national
  stack, and its state moves the IO-bound phases: the same configuration took
  1681 s on 2026-11-13 (national stack just loaded, host swapping) and 1602 s
  on 2026-11-14 (after a reboot). Every `pressure` comparison below uses the
  **same-day control** (1602 s first load, 723 s re-load); `roomy` runs are
  CPU-bound and stable to about 3 %. No pair of identical runs was repeated on
  the same day, so differences under about 5 % are treated as noise.
- Twelve runs; the run table with directories is in section 10.

## 2. Baseline profile (current code)

Seconds. `r` = roomy, `p` = pressure (same-day control).

| phase | first load r | first load p | national (h) | re-load r | re-load p |
|---|---|---|---|---|---|
| parse + staging + partners | 11 | 18 | 0.12 | 11 | 18 |
| companies | 208 | 207 | 1.93 | 153 | 162 |
| persons | 92 | 93 | 0.70 | 7 | 12 |
| relationships | 273 | 469 | 8.40 | 173 | 379 |
| CloseUnseen | 0 | 0 | 0 | 5 | 19 |
| Resolve | 96 | 727 | 14.11 | 5 | 38 |
| search rebuild | 87 | 87 | 0.69 | 83 | 90 |
| **collection** | **770** | **1602** | **26.3** (with 0.39 h download) | **442** | **723** |

`pressure` reproduces the national shape: load 49 % / Resolve 45 % / search
5 % (national 44 / 54 / 3); the cost of 64 relationship chunks grows over the
phase (23 → 45.5 s on 2026-11-13, 29 → 41 s on 2026-11-14; national × 2.5);
`IO/DataFileRead` is 30–34 % of the loader's wait samples. Scaled by rows it
predicts 12.2 h where 26.3 h were measured: the national base is about twice
as deep into cache pressure as the profile. Projections (section 7) therefore
apply per-phase ratios, never absolute seconds.

### 2.1 Top time consumers and the evidence for each root cause

**(1) Resolve re-points relationships row by row — 77 % of Resolve.**
`UPDATE relationship SET src_entity_id = …` (35 batches, 583 k rows): 490–573 s
of the 727–798 s under pressure (46 s in memory), plus 45 s for the dst side.
It read 2.96 M blocks (359–435 s of read time) to change 583 k rows: every
re-pointed row is a non-HOT update (an indexed column changes; 0 HOT updates
of 584 k), so it gets a new heap tuple and a new entry in **each of the eight
indexes** of `relationship` at a random position, and pays the update guard.
Block reads of the Resolve phase: relationship heap 1.86 M, relationship
indexes 1.52 M, person_record heap 1.20 M and indexes 0.84 M. The merge itself
(`UPDATE person_record SET entity_id`, 486 k rows, non-HOT) is another
101–110 s (26 s in memory). Together 90 % of Resolve is the cost of writing
every person twice: first one entity per record and relationships pointing at
it, then moving 30 % of the records (312,720 of 1,034,365; nationally 9.57 M
of 27.9 M) and their relationships. The 312,720 entities left empty are the
same waste seen from the other side (national: 10.7 M).

**(2) Relationship insert — foreign-key checks and hash-ordered chunks.**
`INSERT INTO relationship` (1.89 M rows): 242 s in memory (128 µs per row, 50
buffer hits and 8.5 WAL records per row), 375–383 s under pressure. Block
reads of the relationship phase under pressure, by relation:

| relation | share of the 2.87 M blocks read | cause |
|---|---|---|
| `entity` heap | 34 % | FK checks on `src_entity_id` / `dst_entity_id`: a `FOR KEY SHARE` row lock on the referenced tuple — a random heap read and a dirtied page |
| `person_record` heap | 17 % | FK check on `src_record_id`, and the chunk's read of the record's current entity |
| `entity_pkey` | 13 % | the FK checks' index probes |
| `relationship_natural_key` | 11 % | unique expression index, random insert position |
| `relationship_dst_entity_id_id`, `_src_entity_id_id`, `_src_record_id` | 17 % | random insert positions |
| `relationship` heap | 7 % | re-reads of evicted pages |
| `relationship_pkey`, `relationship_first_collection_id` | 0 % | append-only |

**The `DataFileRead` of the relationship phase is mostly not index pages of
`relationship`: 64 % is the foreign-key checks** reading (and row-locking,
hence dirtying) `entity` and `person_record` at random. That part is constant
per chunk. The *growth* over the phase is relationship's four random-insert
indexes: 1 % of the reads in the first quarter of the phase, 41 % in the last,
as they outgrow memory. Both have one cause: the 1,024 chunks are **hash
buckets** of `cnpj_basico`, so each chunk touches every table and index
uniformly at random. Entity ids and person-record ids are handed out in
`cnpj_basico` order by the earlier phases; chunks cut as *ranges* of company
turn all of these accesses into short, mostly sequential ranges.

**(3) The three big inserts are CPU-bound on foreign-key checks.**
`company_version insert` 177 s and `person_record insert` 86 s in memory, the
same under pressure (174 s, 87 s): no IO wait, no WAL wait. With the foreign
keys of the three claim tables dropped in the bench database: **13.9 s
(× 12.7), 18.5 s (× 4.6), and 33.5 s instead of 242 s for the relationship
insert (× 7.2)**. Each check is an SPI query plus a row lock on the referenced
tuple, about 21 µs; a company_version or person_record row pays three, a
relationship row four or five. Under pressure, dropping them takes the whole
load phase from 786 s to 179 s (× 4.4), the relationship phase from 469 s to
86 s with the hash chunks untouched.

**(4) The monthly re-load rewrites every re-seen row.** The two "seen" updates
are 58 % of the re-load in memory (255 of 442 s) and about half under
pressure. `UPDATE company_version SET last_seen_collection_id` (2.52 M rows):
0.14 % HOT — the column is not indexed, but every row of a page is updated by
the same statement, so there is never room for the new version in the page —
6 WAL records per row, 1.7 GB of WAL. `UPDATE relationship … last_seen`
(1.86 M rows): 6 % HOT, 1.85 GB of WAL, 1.76 M blocks read under pressure.
The database grows 21–34 % per re-load until vacuum reclaims the old
versions. The update guard `guard_update` (two `to_jsonb(row)` and a jsonb
subtraction per updated row) costs 16–18 µs per row: 86 s of those 255 s.

**(5) The search rebuild is a constant** 83–90 s whatever the collection
changed: ranked table 41–44 s (of which `search_norm(name)` for every entity
is about 32 s), token tables 25 s (`search_tokens` per row), indexes 10 s.
After the fixes above it is the largest single item of the first load.

## 3. Hypotheses

| # | hypothesis | verdict | measurement (sample) | run |
|---|---|---|---|---|
| H1 | resolve identities before the relationships are written | **confirmed** | Resolve 727 s → 96 s (**× 7.6**) under pressure with the unchanged resolver called before the relationship phase; no re-pointing left; same counts of decisions, proposals, merged records and empty entities | 7 |
| H2 | make Resolve's batch machinery set-based | **killed** as a lever of its own | the per-batch statements (CTAS, ANALYZE, lock, relax, decisions) are 16 s of 96 s in memory and 68 s of 798 s under pressure (8.5 %); what remains after H1 is the merge UPDATE, addressed by task 10 | 1, 2 |
| H3 | relationship insert: FK checks, index locality, chunk order | **confirmed, two causes** | range-ordered chunks: phase 469 s → 287 s (× 1.63), flat instead of growing, blocks read by the insert −94 %; FK checks: 86 % of the insert's CPU (242 s → 33.5 s); both: 469 s → 70 s (**× 6.7**). No row trigger fires on INSERT, the loader already uses anti-joins instead of `ON CONFLICT`, and all eight indexes together (the unique expression index included) are at most 14 % of the insert's CPU (the 33.5 s that remain) | 6, 7, 8, 9 |
| H4 | `company_version` / `person_record` insert | **confirmed: FK checks**; parallelism modest | without FK checks × 12.7 and × 4.6; four parallel sessions on top: × 1.6 and × 2.6 on those statements, × 1.15 on the whole collection | 6, 9, 10 |
| H5 | btrfs copy-on-write + zstd under pgdata | **confirmed, moderate** | No_COW: first load −4.5 % (busy host) and −14 % (quiet host), re-load −6 %; only the IO-bound phases move (−16 % relationships, −20 % Resolve), the CPU-bound inserts do not. Once tasks 2, 3 and 6 remove the random IO, little is left for it to speed up | 3, 4, 5 |
| H6 | PostgreSQL settings during the load | **killed** | worker `synchronous_commit=off` + `max_wal_size` 2 → 16 GB + `checkpoint_timeout` 60 min: checkpoints 12 → 3, full-page images −82 %, WAL −10 % / −28 %, **time +11 % / +3 %** (no gain). WAL waits are 2 % of the samples. Durability defaults stay | 12 |
| H7 | incremental or cheaper search rebuild | incremental **killed**; skip + reuse **confirmed** | queries rely on the physical order of a dense global rank, which an in-place delta cannot keep. "Did anything searchable change?" costs 4 s; a full rebuild reusing `name_norm` and tokens of entities already indexed costs about 22–25 s instead of 87 s (ranked table 40.5 → 8.1 s, company tokens 19.2 → 2.5 s) | 6 (microbenchmark) |
| H8 | the monthly re-load rewrites every row | **confirmed** | seen updates = half of the re-load, non-HOT, +21–34 % database size, 3.5 GB WAL; update guard 16–18 µs → 3.5–5 µs per row with WHEN-clause triggers; ordered chunks: relationship phase 379 s → 202 s; **not rewriting re-seen rows: load phase 573 s → 102 s (× 5.6), WAL 5.5 GB → 0.37 GB, growth +3 %**. CloseUnseen itself is cheap (5–19 s for 64 k closed rows) | 6, 7, 11 |

## 4. The measured ladder

Each step adds to the previous one. `pressure` profile, seconds, same-day
control. Final-state counters (entities, relationships, open relationships,
identity decisions, pending proposals, empty person entities) are identical to
the control after both collections in every step up to and including the
parallel one.

| step (task) | first load | factor | re-load | factor | run |
|---|---|---|---|---|---|
| current code | 1602 | 1.0 | 723 | 1.0 | 5 |
| + range-ordered chunks and Resolve before relationships (2, 3) | 796 | × 2.0 | 524 | × 1.38 | 7 |
| + foreign-key checks replaced for the loader's bulk statements (6) | 326 | × 4.9 | 401 | × 1.8 | 9 |
| + four parallel sessions (7) | 283 | × 5.7 | 343 | × 2.1 | 10 |
| + cheap update guards and search reuse (4, 5) — *computed from the microbenchmarks, not a run* | ≈ 215 | × 7.4 | ≈ 245 | × 3.0 | — |
| + re-seen rows not rewritten (9) — *load phase measured (102 s), rest computed* | ≈ 215 | | ≈ 140 | × 5.1 | 11 |

Phase detail of the measured steps (first load / re-load):

| phase | current | + 2, 3 | + 6 | + 7 |
|---|---|---|---|---|
| companies | 207 / 162 | 211 / 158 | 52 / 103 | 47 / 116 |
| persons | 93 / 12 | 93 / 9 | 29 / 7 | 15 / 7 |
| Resolve (early + final) | 727 / 38 | 96 / 32 | 64 / 27 | 76 / 34 |
| relationships | 469 / 379 | 287 / 202 | 70 / 144 | 37 / 59 |
| search | 87 / 90 | 88 / 88 | 91 / 89 | 86 / 92 |

## 5. Design: ordered list of tasks

Highest gain per unit of risk first. Tasks 1–5 are **safe, do now**: they
change no rule, no privilege and no stored meaning. Tasks 6–8 are technically
contained but **need the owner's decision** (section 9). Tasks 9–10 are the
**bigger redesign**. Every task ends with the equivalence test of task 1
passing; the expected gain is on the sample, the national projection is in
section 7.

### Task 1 — Equivalence test (prerequisite, no speed-up)

- **Goal.** A canonical, id-independent dump of the claims state and a test
  that loads the same fixtures through the path before and after a change and
  compares the dumps. Every later task is reviewed against it.
- **Areas.** New `internal/claimstest` (or under `internal/testdb`): a
  function `CanonicalDump(ctx, pool) (hash string, sections map[string]string)`;
  a table-driven test in `internal/connector/receita` and `…/tse` built on the
  existing fixtures and on the recovery tests' checkpoint hook.
- **Approach.** One sorted text section per table, hashed (SHA-256), with
  surrogate ids replaced by natural identities:
  - entity: `(kind, natural_key, name, name_norm)`; a person entity is
    identified by the sorted list of its records' keys `(source_id,
    context_key, name_norm, cpf_fragment)`; person entities without records
    are listed in a section of their own (count + names) so that task 10 can
    exclude exactly that section;
  - person_record: key, entity identity, `identity_confidence`, lock (the
    locking decision's canonical form), first collection, `record_ref`;
  - relationship: `(rel_type, src identity, dst identity, src/dst record key,
    attrs, kind, confidence, valid_from, valid_to_after, valid_to_before,
    first collection, record_ref, last seen, closed)`;
  - company_version: company key, the eight attributes, first / last seen /
    closed collection, `record_ref`;
  - identity_decision: `(kind, action, sorted record keys, entity identity,
    confidence, reason, decided_by)` as a multiset; identity_proposal:
    `(group_key, record keys, reason, status, connections, decision)`;
    identity_ambiguous_key; identity_resolve_done;
  - collections by `(source, reference_date, status)`; timestamps excluded.
  "Last seen" goes through one helper so that task 9 can redefine how it is
  read without touching the test.
- **Scenarios.** First load; second edition with additions, removals and
  changed companies; a slice between two full loads; a load interrupted at
  each checkpoint and repeated; a re-load refused by the shrink guard and
  followed by a good one; analyst Merge / Split / Decide (confirm and reject)
  placed between two loads, including a locked record, an ambiguous key and a
  name above the threshold.
- **Risk.** None for production (test code). **Gain.** None; it is what makes
  the other tasks reviewable.
- **Implemented** (`internal/claimstest`; `TestEquivalence` in
  `internal/connector/receita` and `…/tse`). "Before" is a golden file per
  scenario (`testdata/equivalence/<scenario>.golden` in each package): the
  full canonical dump recorded from the code before the optimisation, with
  the hash of each section in its header. A later task must pass **without
  rewriting them**; a failure names the differing sections and their first
  missing / extra lines. Rewriting is for an intended, reviewed difference
  only: `make test PKG='./internal/connector/receita
  ./internal/connector/tse -update' RUN=TestEquivalence` (or
  `UPDATE_GOLDEN=1`). Each scenario runs the worker's path (Load,
  CloseUnseen, Resolve, search refresh, through `connector.Collect`) twice,
  the second time with every id sequence moved forward: both dumps must be
  identical. The live search tables are part of the golden, with
  `search_rank` replaced by the first rank and size of its tie group (ties
  are broken by entity id), so a change to the rebuild itself is compared
  with today's output; after each run they are also compared row for row
  with a from-scratch rebuild started from empty tables
  (`CheckSearchMatchesRebuild`, for task 5). The dump reads "last seen" only
  through `claimstest.LastSeen` (task 9). Three scenarios interrupted
  *between* relationship chunks mask the first collection of the
  relationships written by the interrupted edition (and its repeat), the one
  field that depends on how claims are cut into chunks (task 2, and a
  parallel load later). TSE has no slice and no shrink guard (not a snapshot
  source): those scenarios exist for Receita only. Two Receita scenarios
  cover what task 3 moves: a proposal that goes obsolete and reopens and
  one whose records and `connections` shrink (`proposal_lifecycle`), and
  analyst decisions between an interrupted load and its repeat.
  **Mutation check** (to repeat when the test itself changes): break one
  thing at a time, run `make test PKG=./internal/connector/receita
  RUN=TestEquivalence`, read the failure, revert. It must fail on (1) a
  claim no longer written (e.g. `AND false` on the legal_rep_of branch of
  `sqlRels`), (2) a changed confidence (the resolver's `1e5` divisor), (3)
  a search refresh that skips a needed rebuild; and it must pass on an
  order-only change (relationship chunks processed in reverse order).

### Task 2 — Range-ordered relationship chunks

- **Goal.** Each chunk touches a short range of `entity`, `person_record` and
  of every index of `relationship`, instead of all of them at random.
- **Areas.** `internal/connector/receita/upsert.go` (`sqlPartners`,
  `sqlPersonNew`, `sqlPersonRecords`, `sqlCompanyEntities`), the comment of
  migration 0004 (no schema change: `bucket` stays an `int`), and the same
  pattern in `internal/connector/tse/upsert.go` (`hashtext(context_key)`).
- **Approach.** `bucket = (dense_rank() OVER (ORDER BY s.cnpj_basico) − 1) ×
  relBuckets / count(DISTINCT cnpj_basico)`: 1,024 chunks of equal numbers of
  companies in `cnpj_basico` order (the prototype's exact expression). Make
  the id order the phase relies on explicit rather than a property of today's
  plans: `ORDER BY` in the statements that hand out ids (`nextval` in
  `context_key, name_norm, cpf_fragment` order; person records inserted in
  that order; companies already `ORDER BY cnpj_basico`).
- **Invariants.** All claims of one company stay in one chunk (the per-chunk
  deduplication rule of migration 0004 is unchanged); a chunk remains one
  short transaction under the identity lock; chunks stay idempotent
  (anti-joins on the natural key), so a crash between chunks is repaired by
  the next collection exactly as today. Bucket boundaries are recomputed from
  staging at every load, so nothing depends on them across loads.
- **Result.** Identical claims; only `relationship.id` values differ (rows
  are inserted in another order), which the canonical dump ignores and no rule
  depends on (keyset paging by id keeps working).
- **Tests.** Task 1's scenarios; the existing recovery tests (checkpoint after
  chunk n).
- **Risk.** Low. One caveat: on a base loaded with hash chunks (the existing
  national one) the heap of `relationship` is in hash order, so the re-load's
  probes gain less until the table is rewritten (section 8).
- **Gain.** Relationship phase × 1.63 under pressure (469 → 287 s) and flat;
  re-load's relationship phase × 1.9 (379 → 202 s).
- **Implemented** (`connector.RangeChunks` in `internal/connector/chunks.go`,
  used by `receita/upsert.go` and `tse/upsert.go`). One difference from the
  prototype's expression: chunks hold equal numbers of **staged rows**, not of
  companies. A key's bucket is `(rows of the keys before it) × chunks / all
  rows`, so a bucket holds at most `ceil(rows / chunks) + (rows of its last
  key) − 1` rows: a company with very many partners fills its bucket alone
  (the buckets it spans stay empty) instead of making one chunk many times the
  average, and companies without Socios rows are not keys at all. Buckets are
  a function of the staged multiset of keys only. Receita: keys are
  `cnpj_basico` of the Socios rows. TSE: keys are the candidacy
  `context_key`, weighted by its claims (its `candidate_for` and its
  donations); both claim inserts join the same computation in one transaction.
  Ids are handed out in the chunks' order: `ORDER BY` in `sqlPersonNew`
  (PostgreSQL evaluates `nextval` of the select list after the sort), person
  entities and records inserted in entity-id order; TSE candidacy records
  (`cand:<year>:<SQ>`) and donor records (`donor:<year>:<SQ>`) both follow
  the candidacies' text order. `relBuckets` need no longer be a power of two.
  Tests: `TestRangeChunks` (skewed and uneven weights, fewer keys than chunks,
  empty staging: every key in one bucket, buckets never decrease along the
  key order, the size bound, the same chunks from the rows in another order),
  `TestRelationshipChunksAreCompanyRanges` (Receita) and
  `TestRelationshipChunksAreCandidacyRanges` (TSE) on the fixtures; task 1's
  goldens pass unchanged. **Measured on the implementation** (run
  `20261114T155502Z-t2t3-pressure`, tasks 2 and 3 together, `pressure`
  profile): relationship phase of the first load 298 s and flat over the
  phase (control 469 s, growing); the `partners` step, which now computes the
  ranges, 7.8 s (5.7 s before task 2).

### Task 3 — Resolve the new records before writing the relationships

- **Goal.** No relationship is ever written pointing at an entity that
  Resolve is about to abandon: the re-pointing disappears.
- **Areas.** `internal/identity/resolver.go` (split `resolveCollection` into
  its batch loop and its sweep + completion marker); the loaders
  (`receita/upsert.go` between the persons phase and `sqlPartners`;
  `tse/upsert.go` after its person phases); `cmd/geoint/worker.go` (give the
  connectors the worker's resolver, so the threshold is the configured one).
- **Approach.** New `Resolver.ResolveRecords(ctx, collectionID)`: exactly the
  current batch loop (`resolveBatch` unchanged), **without** the sweep and
  without writing `identity_resolve_done`. The loader runs `ANALYZE
  person_record`, then `ResolveRecords` for its collection, then writes the
  relationships, whose chunks read `person_record.entity_id` under the
  identity lock as today. The runner's `Resolve` at the end stays where it is:
  its batches find nothing to move, refresh the pending proposals'
  `connections` (computed from relationships that now exist), run the sweep
  and write the completion marker. A loader without a resolver (tests, other
  connectors) behaves as today.
- **Invariants of `identity.Resolve`, all untouched because the code that
  enforces them is the same code, called earlier:**
  1. a group is the records sharing a non-empty `name_norm` and a valid
     fragment, across all sources; nothing else merges automatically;
  2. `n` = distinct valid fragments of the name over all records, linked or
     not; `n ≤ threshold` merges with confidence `max(0.5, 1 − max(n−1,1)/1e5)`,
     otherwise the group is proposed;
  3. the target is the entity of the lowest-id unlocked record (for a
     confirmed key: of the lowest record still locked by the confirming
     decision);
  4. an automatic merge never extends an entity an analyst built (impure
     target → proposal) and never moves a locked record;
  5. an ambiguous key (analyst Split) is always proposed; a rejected key is
     never merged nor proposed again; a confirmed key attracts new records
     only while its entity is clean;
  6. merges are never undone by Resolve;
  7. one `identity_decision` per group in which some member changed, with the
     ordered record ids, the target, the confidence and the reason text;
  8. a record left alone in an entity gets confidence 1;
  9. one proposal per group whose unlocked records are spread over two or more
     entities; pending ones are refreshed, obsolete ones reopened or marked;
  10. every transaction that moves records holds the identity lock and locks
      the records in id order; batches are retried on deadlock or
      serialization failure;
  11. a re-run changes nothing; a collection without `identity_resolve_done`
      is resolved again by the next Resolve (crash recovery);
  12. after the sweep every relationship end equals its record's current
      entity (verified in the prototype runs: 0 stale pointers).
- **Crash behaviour.** After `ResolveRecords` and before the relationships:
  records are merged, decisions recorded, the collection is failed and has no
  completion marker; the next collection's load is idempotent and its Resolve
  first completes the failed one (rule 11) — the state a crash *during*
  today's Resolve already leaves. Nothing is closed by this path.
- **Result.** The same rows as today in every table **when no analyst
  decision on a touched proposal lands between the early pass and the end of
  the collection**; the differences are then `decided_at` a few minutes
  earlier and, between the two passes, proposals whose `connections` do not
  yet count the new relationships (refreshed by the final pass; in the
  prototype: "29 proposed" at the final pass, 0 moved). With such a decision
  the stored state differs from the old order's under the same sequence of
  events: see "The decision window" in the "Implemented" paragraph.
- **Tests.** Task 1 (byte-identical canonical dump); the resolver's own
  tests unchanged; a test that a crash between the two passes is completed by
  the next collection.
- **Detail found by the prototype.** On a re-load the early pass spends 9 s
  per call in the re-point statement for 0 rows (62 k blocks read): the
  records it moves are new and have no relationship yet. Skip the re-point for
  records whose `first_collection_id` is the collection being loaded (as
  implemented the rule is narrower: see "The re-point skip" below).
- **Risk.** Low to medium: the resolver is delicate, but its statements do
  not change; what changes is *when* the first pass runs.
- **Gain.** Resolve × 7.6 (727 → 79 + 16.5 s). With task 2: first load × 2.0,
  re-load × 1.38.
- **Implemented** (`Resolver.ResolveRecords` in `internal/identity/resolver.go`;
  `resolveNewRecords` in `receita/upsert.go` and `tse/upsert.go`).
  `resolveCollection` is now `resolveBatches` (the unchanged batch loop) and
  `sweepAndMark`; `ResolveRecords` is the batch loop alone, for one
  collection, without recovering earlier ones. **How the loaders get it:**
  `connector.Hooks` has a `ResolveRecords` hook next to `Resolve`; the runner
  hands it to `Load`, and only to `Load`, through the context
  (`connector.WithResolveRecords` / `ResolveRecordsFrom`), so the `Connector`
  interface and the connectors without an early pass are untouched. The
  worker builds both hooks from one `identity.Resolver`
  (`collectionHooks`), so the early pass cannot run with another threshold
  than the final one. A `Load` called without the hook (tests, any other
  caller) does not resolve early and behaves as before. Receita runs
  `ANALYZE person_record` and the pass after the persons phase (new test
  checkpoint `early resolve` after it); TSE after the donors phase, before
  the claims. No transaction of the load is open across the pass; an error of
  the pass fails the load. **The re-point skip** is narrower than the detail
  above says: a batch of the early pass leaves out the moved records first
  seen by its collection only while that collection has written no
  relationship (`first_collection_id`, read under the identity lock in the
  batch). Records of earlier collections that the pass moves are re-pointed
  as always, and `Resolve` never skips: loaders without an early pass, and a
  failed collection recovered by a later one, wrote relationships before
  their records were resolved.
  **The decision window.** In the old order the new records of a collection
  entered proposals only at its final Resolve. Now they enter at the early
  pass, so there is a window in which an analyst can decide a proposal that
  already holds them while their relationships are not written yet. The
  window runs from the early pass to the end of **every** collection (the
  relationship phase and CloseUnseen: hours at national scale), not only
  after a crash; after a load that fails past the early pass it lasts until
  the next collection of that source ends. What differs when an analyst
  decides a pending proposal P of key K in the window, C having added record
  R3 to P's records R1, R2:
  - **confirm:** one analyst merge of R1, R2 and R3, all three locked by that
    decision with confidence 1. The old order, with the decision at the same
    moment, gave an analyst merge of R1, R2, then at the final Resolve an
    automatic "confirmed key" merge of R3 (unlocked, confidence
    `1 − (n−1)/1e5`), or left R3 apart when the confirmed entity was not
    clean or the key ambiguous;
  - **reject:** R3 is kept apart and locked by the reject, where the old
    order left it apart and unlocked (a rejected key is skipped);
  - **either way** P is frozen with the `connections` it had when decided,
    which do not count the relationships C writes afterwards (the final pass
    refreshes pending proposals only).
  Entity membership is the same in the clean cases. This is **accepted**
  (owner's ruling, review I1), with no change to the resolver: the state is
  exactly the one the old order produces when the analyst decides after the
  collection has ended, so it is a legal state reached earlier; the decision
  on R3 is explicit, because the proposal shows R3 and the API's decide path
  refuses a stale view (`DecideExpecting`: an analyst who opened the proposal
  before the early pass gets a conflict and reloads). The only cost is the
  frozen `connections`. Pinned by `TestDecisionBetweenTheEarlyPassAndResolve`
  (confirm and reject); no golden scenario has a decision in the window (the
  one with decisions between an interrupted load and its repeat stops before
  the pass). **Decision trail after a crash and a different edition:** when C
  dies after its early pass wrote the decision of a group (with the name's
  `n₁` fragments) and the next collection adds a fragment to that name, its
  early pass writes a second decision with `n₂` and the new confidence, where
  the old order wrote one, at recovery, with `n₂`. The records end the same;
  the trail has one more, truthful, entry (what a successful C would have
  left). **Measured on the implementation** (run
  `20261114T155502Z-t2t3-pressure`, tasks 2 and 3, `pressure` profile): first
  load 801 s (control 1602 s): early resolve 72 s, relationships 298 s flat,
  final Resolve 17 s with 0 records moved; re-load 520 s (control 723 s),
  early resolve 30 s. A `Load` that finds no early pass in its context logs
  one line saying so. Tests:
  the goldens of task 1 pass unchanged, and `TestEquivalence` now fails
  unless every done collection ran the early pass
  (`claimstest.Pipeline.CheckEarlyPass`); `TestEarlyResolveLeavesNothingToRepoint`
  (Receita, TSE: with a trigger logging every re-pointed relationship, 0
  with the pass, some without it, same canonical dump both ways);
  `TestCrashBetweenEarlyResolveAndRelationshipsIsCompletedByTheNext`
  (Receita, TSE); `internal/identity/early_test.go` (same state as Resolve
  alone, the threshold, the skip's two limits, recovery).

### Task 4 — Cheap update guards

- **Goal.** Keep the default-deny update guard, without two `to_jsonb(row)`
  per updated row.
- **Areas.** A new migration (never editing 0001/0019); `internal/db`
  (a test next to `roles_test.go`).
- **Approach.** For `relationship`, `company_version` and `person_record`,
  replace the `guard_update` row trigger by three triggers whose `WHEN`
  clause is evaluated by the executor and whose function only raises:
  immutable columns (`WHEN ((OLD.c1, …) IS DISTINCT FROM (NEW.c1, …))`, every
  column not on the allow-list), no reopening, `last_seen_collection_id`
  never decreasing. Same messages as today. **Default-deny is kept by
  construction and by test:** the migration generates the column list from
  the catalogue (all columns minus the allow-list) and a test fails when a
  table has a column that is neither allow-listed nor in the trigger's
  condition — so a column added later without touching the guard breaks CI
  instead of being silently writable. The small tables keep `guard_update`.
- **Invariants.** Same refusals as today (prototype: changing an immutable
  column and decreasing `last_seen` are both refused); the triggers stay
  owned by the owner; the worker and API roles cannot alter them.
- **Tests.** The existing guard tests against the new triggers; the new
  catalogue test; task 1.
- **Risk.** Low. **Gain.** Guard cost per updated row 16–18 µs → 3.5–5 µs:
  about 65 s of the 442 s in-memory re-load (−15 %); little on a first load.
- **Implemented** (migration `0023_cheap_update_guards.sql`; tests in
  `internal/db/guards_test.go`, microbenchmark in
  `internal/db/guards_bench_test.go`). One difference from the approach
  above: **one trigger per table, not three, and it still calls
  `guard_update`.** `<table>_guard_update` is replaced in place (`CREATE OR
  REPLACE TRIGGER`, same name, same function, same allow-list arguments) and
  gains a `WHEN` clause that is a pure pre-filter: true whenever
  `guard_update` could refuse the row — (a) `ROW(OLD.c…) IS DISTINCT FROM
  ROW(NEW.c…)` over every column off the allow-list, generated from the
  catalogue by `install_update_guard(table, allow-list)`; (b) reopening; (c)
  `last_seen_collection_id` set to NULL or decreasing; (d), a second layer
  only, `update_guard_columns_changed(table, columns)`, true when the
  table's columns are no longer those (a) was generated for. A row that
  passes the clause runs the original function, so refusals, the order of
  the checks, messages and SQLSTATE are identical by construction; a row that
  fails it would have passed (each type's equality agrees with the jsonb
  comparison for the types of these tables; pinned with values written
  differently by `TestUpdateGuardEqualValuesWrittenDifferently`).
  **Default-deny at run time for a column added later, at every isolation
  level:** the event trigger `geoint_update_guard_reinstall`
  (`ddl_command_end`, `ALTER TABLE`; schema `geoint_guard`, one for the
  database) re-installs the guard of a guarded table with its current
  allow-list in the same transaction as the ALTER, so a new column is
  compared by (a) from the moment it exists; a statement that sees the column
  sees the new clause too (both come from the relation cache), including a
  REPEATABLE READ transaction whose snapshot predates the ALTER
  (`TestUpdateGuardNewColumnUnderAnOlderSnapshot`). `TestUpdateGuardCatalogue`
  fails until the column's mutability is decided (in the tests' lists, and by
  a migration calling `install_update_guard` if it may change). An ALTER that
  would leave the table without its guard (`DROP COLUMN … CASCADE` of a
  compared column) is refused; a migration that drops a column runs, in one
  transaction, `SET LOCAL geoint.update_guard_installing = 'on'`, the
  `DROP COLUMN … CASCADE` and `install_update_guard`. Re-installing keeps the
  trigger's enabled state (`CREATE OR REPLACE TRIGGER` alone resets it to
  `O`; verified), and the catalogue test requires one state,
  `guardTriggerEnabled` (`O`; task 6 changes it to `A`). (d) only acts when
  the event trigger did not run (single-user mode, `event_triggers = off`,
  or that switch): it reads `pg_attribute` through the statement's snapshot,
  so it is best effort. It is declared `IMMUTABLE` although it reads the
  catalogue, on purpose: the planner folds it once per statement (trigger
  `WHEN` clauses are prepared at each execution, never cached), not once per
  row — pinned by `TestUpdateGuardFastPath` (function statistics: 0 calls of
  `guard_update` and 1 column check for a "seen" update of every row; one
  `guard_update` call per row when a column was added with the event trigger
  skipped). **search_path:** the worker may `CREATE` in the schema (0021), so
  every trigger function of the schema (`guard_update`, `forbid_delete`,
  `forbid_update`, `guard_proposal_update`, `guard_collection_status`,
  `guard_entity_update`) and the functions of 0023 run with `search_path =
  pg_catalog, pg_temp`; before, an exact-type `to_jsonb(relationship)`
  created by the worker switched `guard_update` off
  (`TestWorkerCannotShadowGuardFunctions` shows the attack on an unpinned
  function, then its failure; `TestTriggerFunctionsPinSearchPath` fails on
  an unpinned trigger, event trigger or SECURITY DEFINER function). The
  migration takes `SHARE ROW EXCLUSIVE` on the three tables for milliseconds
  (catalogue only: no scan, no rewrite; reads continue; `lock_timeout` 5 s);
  there is no moment without a guard; `CREATE EVENT TRIGGER` needs the owner
  to be a superuser, which it is (0021). Tests: `TestUpdateGuardCharacterisation` (written and
  green on the old guard first: every column × no-op / change / NULL /
  collection below, equal, above × never seen / seen / closed rows ×
  owner, worker, API; combinations of allowed and forbidden columns; 435
  updates per role), the same matrix with an unguarded column added,
  `TestUpdateGuardMatchesTheOriginal` (9,004 single- and two-column updates,
  outcome with the installed guard = outcome with the trigger of 0001 put
  back in the transaction), the existing guard tests of `internal/claims`
  and task 1's goldens, all unchanged. **Measured** (`make test
  PKG='-bench BenchmarkUpdateGuard -benchtime 5x ./internal/db' RUN=NONE`,
  test database with the small settings, 200 k rows, `UPDATE … SET
  last_seen_collection_id` on every row, rolled back, 5 runs): relationship
  53.2 µs/row with the original guard, 34.0 with the new one, 33.5 without
  any guard — guard cost 19.7 → 0.5 µs/row; company_version 45.7 / 29.0 /
  27.5 — 18.3 → 1.5 µs/row. The "seen" statements are 36–37 % faster.
  Cost of the pinned `search_path` (same benchmark, fix round 1; pinned /
  unpinned, µs/row): "seen" update with the installed guard 34.33 / 34.46
  (relationship) and 28.51 / 28.32 (company_version) — no function is called;
  bulk `INSERT` into relationship 93.03 / 93.35 — no row trigger fires on
  INSERT; `UPDATE identity_proposal SET connections` (Resolve's refresh,
  `guard_proposal_update` on every row) 14.29 / 13.62, 11.42 without the
  trigger: the pin adds about 0.7 µs per proposal row (guard 2.2 → 2.9 µs).

### Task 5 — Search: skip when nothing changed, reuse what is already computed

- **Goal.** A collection that changes nothing searchable does not pay a
  rebuild; a real rebuild does not recompute what cannot have changed.
- **Areas.** `internal/search/rebuild.go`; migration for one column of
  `search_state` (version of the normalisation functions).
- **Approach.** Same rebuild + swap, same dirty marker. Two changes inside
  the build: (a) after computing the degree table (1.4 s on the sample),
  compare it and the set of searchable entities with the live tables (2.6 s);
  if nothing differs, record the request as covered and return without
  building; (b) otherwise build as today, but take `name_norm` and `tokens`
  from the live tables for entities already there (`LEFT JOIN … ON entity_id`,
  `coalesce(old.name_norm, search_norm(e.name))`, likewise for tokens). This
  is exact because entity names are immutable (`guard_entity_update`); a
  stored version number forces a from-scratch build when a migration changes
  `search_norm`, `search_words` or `search_tokens`.
- **Invariants.** Tables written in `search_rank` order, the swap, the lock
  order, the frequent-token table and the recovery of a lost dirty mark are
  unchanged. The result is row-for-row what a from-scratch rebuild produces.
- **Tests.** Build both ways on the fixtures and compare the tables; a
  no-change collection leaves the generation untouched and the state clean.
- **Note from task 3.** "Nothing changed" must count the moves of the early
  identity pass of a collection that later **failed**: its merges are
  committed, so an indexed person entity may have been emptied by a
  collection that never reached its search refresh.
- **Not proposed.** A truly incremental index: queries rely on the physical
  order of the tables by a dense global rank; a delta cannot maintain it.
  That is a redesign of search, outside this work.
- **Risk.** Low. **Gain.** Rebuild 87 s → about 22–25 s (× 3.5); 4 s for a
  collection that changed nothing (the weekly small sources).
- **Implemented** (`internal/search/rebuild.go`; migration
  `0024_search_build_version.sql`). Same dirty marker, lock, build tables,
  swap and recovery as before. The build now starts with a degree table
  (`search_degree`); the degree table, the change check and the ranked table
  run in one REPEATABLE READ transaction, so the tables and the decision to
  skip come from one snapshot. **"Nothing changed"** (`changedSQL`, only in
  `RebuildIfDirty`, i.e. the worker's refresh and catch-up; `Rebuild` never
  skips) is a comparison of data, not a log of writes: a FULL JOIN of the
  candidate entities (every entity of a kind with a code; a person entity
  only while it has a person record) with the three live tables, true when an
  entity missing from them would be searchable (`search_norm(name)` not NULL,
  computed for those entities only), when an indexed entity is no longer a
  candidate (emptied by a merge, early pass of a failed collection included),
  or when kind or grau differ. Those are all the inputs of the build given the
  same build version, because an entity's name is immutable
  (`guard_entity_update`) and entities are never deleted; so whatever wrote
  `entity`, `person_record.entity_id` or `relationship` (src/dst entity,
  closed collection) — loaders, CloseUnseen, the early and final identity
  passes, analyst Merge/Split/Decide, another source — and whether or not its
  collection reached a search refresh, the next refresh sees it. A failed
  collection counts as finished, so the state is dirty after it. A skip also
  needs the stored build version and frequent-token threshold to equal the
  current ones; it then records the request as covered (`rebuilt_from`,
  `finished_collections`) and leaves the tables and `generation` untouched.
  **Reuse**: a build takes `name_norm` and `tokens` of entities already in
  the live tables (`coalesce(live, computed)`), only when
  `search_state.build_version` equals the build version computed now: an md5
  of the catalogue definitions of `search_norm`, `search_words` and
  `search_tokens` (`pg_get_functiondef`), the unaccent extension version and
  dictionary options, the server version, and a sha256 of the build
  statements in `rebuild.go`. A migration or code change to any of them
  changes the version without anyone bumping a number; the next rebuild is
  then full and is not skipped. The version is checked again inside the swap
  transaction: if it changed during the build (a migration meanwhile), the
  tables are not swapped in and the state stays dirty. The first rebuild
  after migration 0024 is full (no version stored). **Tests**
  (`internal/search/reuse_test.go`, `receita/search_refresh_test.go`; every
  step compared with a from-scratch build by
  `claimstest.CheckSearchMatchesRebuild`, which now also compares the stored
  rows with entity ids in physical order and clears the version so the
  reference cannot reuse): a no-change refresh skips (generation, rows and
  clean state); ten kinds of change (new entity, degree only, close,
  re-point, emptied person entity, split, ...) rebuild, and the two that
  change nothing searchable skip; another threshold rebuilds; a live table
  missing a row rebuilds; reuse is observable (a corrupted live value
  survives a same-version rebuild) and disappears with another stored
  version and after replacing each of the three functions; a function
  replaced during the build is not swapped in; an interrupted reusing
  rebuild leaves the old tables byte for byte; a failed collection's merge is
  not skipped. On the Receita pipeline: first load, re-load of the same
  edition (skipped), analyst Decide, a load failing after its early pass
  (which empties an indexed entity) followed by an unchanged collection
  (rebuilt), Merge, Split, unchanged again (skipped), re-load with changes
  (rebuilt, reusing), another source adding only relationships (rebuilt), the
  next edition unchanged (skipped). Task 1's goldens pass unchanged.
  Mutation check: ignoring grau or emptied entities in `changedSQL` fails
  these tests. **Measured** (`TestSearchRebuildTimings` in
  `internal/search/bench_test.go`, `SEARCH_BENCH=1 SEARCH_BENCH_N=1000000`,
  test database with the small settings; 1 M synthetic entities, 1.5 M
  relationships; the code before this task run on the same generator with
  the same steps; two rounds each, both rounds within 1 %):

  | refresh | before | after |
  |---|---|---|
  | first build | 19.9 s | 20.1 s |
  | nothing changed | 19.8–20.2 s | **2.5 s** (skipped) |
  | after 1 % new entities and relationships | 20.4–20.5 s | **8.9 s** (reusing) |
  | same data, no reuse (version changed) | — | 22.2–22.4 s |

  A full build without reuse costs about 9 % more than before (the degree
  is materialised once for the check and the ranking); it happens only on
  the first rebuild after the migration and after a change to the search
  functions or build statements. The sample measurement of section 2
  (87 s) was not repeated: `make bench-load` is run by the owner.

### Task 6 — Set-based verification instead of per-row FK checks, for the loader's bulk statements only *(owner decision D1)*

- **Goal.** The bulk `INSERT … SELECT` / `UPDATE … FROM` of the loaders stop
  paying one SPI query and one row lock per foreign key per row, without
  dropping or weakening any constraint for anyone else.
- **Areas.** Migration (grant + trigger modes); a helper in the loaders
  (`receita/upsert.go`, `tse/upsert.go`); `make national-check`; tests.
- **Approach.**
  1. Migration: `GRANT SET ON PARAMETER session_replication_role TO
     geoint_worker` (the owner is a superuser; `geoint_api` gets nothing), and
     every user trigger of the schema — update guards, no-delete, no-truncate,
     immutability — becomes `ENABLE ALWAYS`, so that in replica mode **only
     PostgreSQL's internal foreign-key triggers stop firing**. A test fails
     when any non-internal trigger is not `ALWAYS`.
  2. Loader helper `bulk(tx, statement, verify…)`: `SET LOCAL
     session_replication_role = replica`, run the statement, then run its
     verification queries **in the same transaction** — one anti-join per
     foreign key over exactly the rows the statement wrote (e.g. the chunk's
     staged rows against `entity` and `person_record`) — and fail the
     transaction if any returns a row. `SET LOCAL` ends with the transaction.
     Only the statements listed in the helper's callers use it: the
     company_version close / seen / insert, the person_record insert, the
     relationship seen / insert, and the person entities' insert.
  3. `make national-check` gains a full verification of every foreign key of
     the claim tables (set-based anti-joins).
- **Why it is safe.** (a) Every referencing value those statements write
  comes from a join with the referenced table in the same statement or from
  staging rows filled from it earlier in the same load, under the load lock.
  (b) A referenced row cannot disappear or change key afterwards: deletes and
  truncates of `entity`, `person_record` and `collection` are refused by
  triggers (now `ALWAYS`) and the worker has no DELETE privilege on them;
  primary keys are immutable. The row lock the FK check takes protects only
  against exactly those two events. (c) The verification re-proves it before
  commit. (d) Everything else — `claims.Store` single-row writes, the API,
  analyst decisions — keeps the per-row checks.
- **Measured on the prototype** (worker role in replica mode for the whole
  run, FKs kept): guards still fired 4.46 M times; an insert with a dangling
  reference is accepted in replica mode and refused in the default mode;
  the API role gets `permission denied to set parameter`; the full
  verification of all seven FK relations of the three tables after two
  bypassed collections found 0 dangling rows in 5.3 s (the per-row checks
  cost about 430 s of the first load).
- **What it costs in least privilege.** The worker role becomes *able* to
  skip FK checks in a session. Mitigations: `SET LOCAL` only inside the
  helper; a test that the parameter's name appears nowhere else in the code;
  the in-transaction verification; the periodic full check. A stricter
  variant keeps the grant away from the worker altogether: the bulk statements
  live in `SECURITY DEFINER` procedures owned by the owner, which set the mode
  themselves. It costs moving loader SQL into migrations; recommended only if
  the owner rejects the grant.
- **Risk.** Medium (a privilege decision, and a bug in a verification query
  would hide a bug in a load statement — the periodic full check is the
  second net). **Gain.** Load phase × 3.6 on top of tasks 2–3 (689 → 220 s);
  first load 796 → 326 s; re-load 524 → 401 s. It also removes 64 % of the
  read IO of the relationship phase where task 2 is not enough (heap of
  `entity` larger than memory).

### Task 7 — Parallel sessions *(after task 6; owner decision D2 on the default)*

- **Goal.** Use more than one of the six cores for the three big inserts and
  the relationship chunks.
- **Areas.** `receita/upsert.go`, `tse/upsert.go`, `internal/identity`
  (`LockTx` gains a shared variant), migrations 0003/0004 staging (a record-id
  column; per-session staging for chunks), worker configuration (`LOAD_WORKERS`,
  default 4, bounded by the pool).
- **Approach.** Ids are assigned in staging in a deterministic order before
  any parallel work (entity ids already are; add the person-record id and the
  company_version id), and the inserts carry them explicitly: the final state
  does not depend on which session wrote what, and the resolver's "lowest-id
  record" stays the same record. Inserts are split in contiguous id ranges,
  one transaction per range that writes the range's entities *and* its
  records, so a crash leaves whole ranges and the anti-joins skip them on the
  next run. Relationship chunks go to K sessions over disjoint chunks, each
  with its own staging table. **Identity lock:** chunk writers only *read*
  `person_record.entity_id`; they take the lock in SHARED mode, while every
  transaction that moves records (Resolve, Merge, Split, Decide) and
  CloseUnseen keep the EXCLUSIVE mode — a mover still excludes every reader,
  which is all the current rule guarantees; PostgreSQL queues a waiting
  exclusive request ahead of later shared ones, so an analyst is not starved.
- **Invariants.** Chunks are disjoint by company, so sessions never write the
  same natural key or row; lock order unchanged; idempotence per chunk and per
  range.
- **Tests.** Task 1 with 1 and 4 sessions giving the same dump *including
  ids*; a crash test with ranges half done; a concurrency test of an analyst
  Merge during the chunks.
- **Risk.** Medium (concurrency). **Gain.** × 1.6 (company_version insert),
  × 2.6 (person_record insert), × 1.9–2.4 (relationship phase); × 1.15–1.17
  on the whole collection, because after tasks 2, 3 and 6 the rest is serial.
  **Not measured without task 6**, and not recommended there: four sessions
  would each share-lock the same `collection` row millions of times.

### Task 8 — No_COW for pgdata on btrfs *(owner decision D3)*

- **Goal.** New installations on a btrfs Docker root do not pay copy-on-write
  and compression under PostgreSQL's data files.
- **Areas.** `scripts/volume-init.sh` (+ its test), `deploy/compose.yml`
  (the `volume-init` service needs `chattr` and `CAP_FOWNER`), README,
  `deploy/TUNING.md`.
- **Approach.** When the pgdata directory is **empty** and its filesystem is
  btrfs, `chattr +C` it before the first `initdb` (the harness does exactly
  this with `BENCH_NOCOW=1`); print what was done; on other filesystems do
  nothing (ext4 and xfs have no such cost). The attribute only applies to
  files created afterwards: **an existing installation is not changed** —
  `national-check` reports "pgdata on btrfs with CoW" and the README explains
  that it takes a backup + new volume + restore (or a fresh load) to change.
  Consequences to document: No_COW files are neither compressed nor
  checksummed by btrfs (PostgreSQL's own `full_page_writes` and WAL still
  protect against torn pages; data checksums are PostgreSQL's), and the
  database takes more disk space than `df` showed under zstd.
- **Risk.** Low for new volumes; **Gain.** −5 % to −14 % on the first load of
  the current code, −6 % on the re-load; expected to shrink further after
  tasks 2, 3 and 6 because the phases it helps stop being IO-bound.
  `full_page_writes` stays on in every case.

### Task 9 — Re-seen rows are not rewritten *(bigger redesign; owner decision D4)*

- **Goal.** A monthly re-load writes the 1–2 % of rows that changed, not the
  98 % that did not.
- **Areas.** Migration (one table, one nullable column on `relationship` and
  `company_version`, guards' allow-list); `receita/upsert.go`;
  `internal/claims/close.go`; every reader of `last_seen_collection_id`
  (`internal/api/entities.go`, `claims/ended.go`, `cvm/write.go`).
- **Approach.**
  - `source_seen(source_id, collection_id)`: "every open fact row first
    written by this source and not flagged unseen was seen by this full
    collection" — the source's watermark.
  - `unseen_collection_id` (nullable) on the two tables: set when a full load
    did not find the open row. At that moment its stored
    `last_seen_collection_id` is raised to the previous watermark, i.e. to the
    value today's code would have stored.
  - Effective last seen, defined once in a SQL function used by every reader:
    the stored value for a closed or flagged row and for inferences; otherwise
    `GREATEST(stored, watermark of the row's source)`.
  - Full load N: new rows are inserted as today; per chunk the ids of the
    re-seen open rows are collected read-only into an unlogged staging table,
    and only rows whose entities changed are updated; then **one sealing
    transaction**: flag the open rows of the source that are not in the seen
    set (materialising their last seen), clear the flag of flagged rows that
    reappeared (stored last seen = N), and move the watermark to N. A crash
    before the seal leaves the previous watermark, which is still true.
  - CloseUnseen keeps its rules — full collections only, shrink guard,
    regression check, observed-date bound — with "unseen" read as "flagged"
    and `valid_to_after` taken from the materialised last seen.
  - Slices and the single-row `claims.Store` path keep stamping rows one by
    one (few rows); a source without a watermark behaves exactly as today, so
    the change can be adopted source by source.
- **Invariants.** Closing stays irreversible and never earlier than the last
  sighting; `last_seen` never decreases (guard); a failed or shrunk collection
  closes nothing and its flagged rows are either re-seen (unflagged) or closed
  later with the same interval as today.
- **Tests.** Task 1 comparing **effective** last seen, closing collection and
  both validity bounds between today's path and this one over: two full loads;
  a shrink-guard refusal followed by a good load; a slice between full loads;
  a crash before the seal; a row that disappears and comes back.
- **Risk.** High: it changes how a stored column is read, in every reader.
  Do it after the safe tasks, with the national re-load measured first.
- **Gain.** Re-load's load phase 573 → 102 s (× 5.6, with tasks 2, 6, 7);
  WAL 5.5 GB → 0.37 GB; database growth per re-load +21–34 % → +3 %.

### Task 10 — Entity assignment at insert *(bigger redesign)*

- **Goal.** A new person record is inserted directly in its final entity: no
  merge UPDATE, no empty entity.
- **Approach.** After staging the new records (ids and provisional entity ids
  assigned as today), run the resolver's group computation over existing
  records ∪ staged ones for the touched names, under the identity lock, and
  derive each new record's final entity and confidence; insert only the
  entities that end up holding a record, the records already placed, and the
  same `identity_decision` rows Resolve writes today (the "old" side of
  `group_changed` being the provisional singleton). Because the provisional
  ids are the ids today's code would have inserted, the target of each group
  is the same entity id as today. Existing records that must change (rare:
  confidence of old groups, confirmed keys) are left to the regular Resolve.
- **Result.** Identical to today **except that person entities without
  records are never created** (30 % of the person entities of a first load).
  The canonical dump compares everything else.
- **Risk.** High (re-implements the resolver's decision for staged rows; must
  share its SQL, not copy it). **Gain (not prototyped; from statement
  times).** The merge UPDATE is 44 s of the 79 s early Resolve under pressure
  and 26 s in memory; plus 30 % fewer person-entity inserts and a smaller
  `entity`. Do it only if Resolve still matters after task 3.

### Not proposed

- Building relationship's indexes after a first load into an empty table:
  with tasks 2 and 6 the phase already runs at the in-memory rate and stays
  flat; it would add a second code path for first loads only. Not measured.
- H2 on its own, H6 (settings), an incremental search index: see section 3.

## 6. What the equivalence test must show, per task

| task | expected difference in the canonical dump |
|---|---|
| 2 | none (relationship ids are not part of it) |
| 3 | none |
| 4, 5 | none (5: search tables compared row for row with a from-scratch build) |
| 6 | none; plus the full FK verification returns 0 rows |
| 7 | none, and with pre-assigned ids the raw ids of entity, person_record and company_version are equal too |
| 8 | none |
| 9 | none when "last seen" is read through the effective-last-seen helper |
| 10 | only the section "person entities without records" (empty after the change) |

## 7. Projected national timings

Method: national phase time ÷ the factor measured for that phase on the
sample under `pressure`. **Uncertainty is large and stated**: the sample is
3.65 %, the national base is about twice as deep into cache pressure, and the
national re-load has never been measured at all.

First national load (h; measured today: 26.3):

| after | companies | persons | relationships | Resolve | search | fixed (download, parse, partners) | **total** | range |
|---|---|---|---|---|---|---|---|---|
| today | 1.93 | 0.70 | 8.40 | 14.11 | 0.69 | 0.51 | **26.3** | measured |
| tasks 2, 3 | 1.93 | 0.70 | 5.2 | 1.9 | 0.69 | 0.51 | **≈ 10.9** | 7.5–11.5 |
| + tasks 4, 5 (**all safe tasks**) | 1.93 | 0.70 | 5.2 | 1.9 | 0.20 | 0.51 | **≈ 10.4** | 7–11 |
| + task 6 | 0.48 | 0.22 | 1.25 | 1.25 | 0.20 | 0.51 | **≈ 3.9** | 3–5.5 |
| + task 7 | 0.44 | 0.11 | 0.66 | 1.25 | 0.20 | 0.51 | **≈ 3.2** | 2.5–5 |
| + task 10 (estimated, not prototyped) | 0.44 | 0.11 | 0.66 | ≈ 0.6 | 0.20 | 0.51 | **≈ 2.5** | 2–4.5 |

- Relationships after task 2: 5.2 h is the sample's factor (× 1.63). The
  national phase was far more IO-bound than the sample's (347 µs per row in
  its *first* chunks against 144 µs in memory), so if ordering brings it to
  the in-memory rate as it did on the sample, the phase is nearer 2.1–3 h:
  that is the low end of the range.
- Resolve after task 3: 1.9 h is the sample's factor (× 7.6). Bottom-up from
  national per-row costs (merge UPDATE of about 15 M record rows, 5 M
  decisions, batch machinery) gives 1.5–2.6 h.
- **The 12 h target is met by the safe tasks alone, with little margin at the
  pessimistic end; with task 6 it is met with a factor of two to spare.**

Monthly national re-load (h, including 0.4 h of download). There is no
national measurement: "today" is the sample × 27.4 (5.9 h), and could be up to
about 10 h if the re-load suffers from cache pressure in the proportion the
first load did.

| after | sample re-load (s) | national (h) |
|---|---|---|
| today | 723 | **≈ 5.9** (5.9–10, never measured) |
| all safe tasks (2–5) | ≈ 405 | **≈ 3.5** (3.5–5) |
| + tasks 6, 7 | ≈ 245 | **≈ 2.3** |
| + task 9 | ≈ 140 | **≈ 1.5** |

To re-measure nationally, in this order: (1) a first load after tasks 2 and 3
(is the relationship phase flat? how long is the early Resolve?) — it decides
whether tasks 6 and 7 are needed for the target or only welcome; (2) the first
real monthly re-load (edition 2026-11 when published), before deciding on
task 9; (3) the search rebuild after task 5.

## 8. The existing national database

- **10.7 M empty person entities.** Do nothing. They are excluded from search
  and from every query that starts from records; they cost about 2 GB (heap and indexes of `entity`).
  Nothing may be deleted by design, and a cleanup would need the owner to
  disable the no-delete triggers — a precedent worth more than the space.
  Task 10 stops creating new ones; task 3 alone does not.
- **Hash-ordered heap of `relationship`.** After task 2 new rows arrive in
  company order, but the 50.6 M existing rows stay in hash-bucket order, so
  the re-load's probes of old rows remain scattered (index pages become
  local, heap pages do not). The gain of task 2 on the *re-load* of the
  existing base will be smaller than measured here.
- **Recommendation (decision D5).** If the national base holds no analyst
  decision yet (no row of `identity_decision` with `kind = 'analyst'`, no
  decided proposal), re-create it with a fresh load once tasks 2–6 are in —
  about 4–11 h instead of 26 — on a No_COW pgdata if D3 is yes. That removes
  the empty entities, the bloat and the hash-ordered heaps in one step and is
  the national re-measurement of section 7. If analyst decisions exist, keep
  the base (decisions and their audit trail are never discarded) and accept
  the smaller re-load gain.
- No migration of data is needed by tasks 2–8. Task 4 and task 6 are
  catalogue-only migrations (triggers, a grant). Task 9 adds a nullable
  column (catalogue-only) and a small table; its watermark starts empty, which
  means "behave as before" until the first full load under the new code.

## 9. Decisions needed from the owner

| # | decision | recommendation |
|---|---|---|
| D1 | Task 6: may the worker role skip per-row FK checks inside the loaders' bulk statements (`session_replication_role`, guards `ENABLE ALWAYS`, set-based verification in the same transaction)? | **Yes.** It is the largest remaining lever after tasks 2–3 (load phase × 3.6), the constraints stay in force for every other writer, and the verification is cheap. If the grant to the worker is unacceptable, the `SECURITY DEFINER` variant |
| D2 | Task 7: default number of load sessions | 4 on the reference machine, 1 on smaller ones; only after task 6 |
| D3 | Task 8: set No_COW automatically on an empty pgdata when the Docker root is btrfs | **Yes** for new volumes (documented loss of btrfs compression and checksums for those files); no automatic change of existing volumes |
| D4 | Task 9: change how "last seen" is stored (watermark + unseen flag) | Decide **after** the first real monthly re-load is measured with the safe tasks; if it is then under about 4 h, the redesign may not be worth its risk |
| D5 | Re-create the national base after tasks 2–6, or keep it | Re-create if it holds no analyst decision; otherwise keep |
| D6 | Durability during loads (`synchronous_commit`, checkpoints) | **No change.** Measured: no gain. `fsync`, `synchronous_commit` and `full_page_writes` stay on |
| D7 | Search freshness for the weekly small sources | Nothing to decide if task 5 is done: unchanged collections skip the rebuild in seconds |

## 10. What was run, and what could not be measured

| # | run directory (`results/bench/`) | profile | change | first load (s) | re-load (s) |
|---|---|---|---|---|---|
| 1 | 20261113T193526Z-baseline-roomy | roomy | none | 770 | 442 |
| 2 | 20261113T200530Z-baseline-pressure | pressure | none (busy host) | 1681 | 813 |
| 3 | 20261113T204930Z-nocow-pressure | pressure | No_COW (busy host; re-load cut by a reboot) | 1605 | — |
| 4 | 20261114T111953Z-nocow-pressure | pressure | No_COW | 1374 | 681 |
| 5 | 20261114T115449Z-baseline2-pressure | pressure | none (**same-day control**) | 1602 | 723 |
| 6 | 20261114T123430Z-nofk-roomy | roomy | FKs of the three claim tables dropped; guard and search microbenchmarks | 318 | 329 |
| 7 | 20261114T124947Z-X-ordered-early-pressure | pressure | range-ordered chunks + Resolve before relationships | 796 | 524 |
| 8 | 20261114T131253Z-nofk-pressure | pressure | FKs dropped | 953 | 637 |
| 9 | 20261114T134342Z-fkbypass-X-pressure | pressure | 7 + FK checks skipped through `session_replication_role`, FKs kept, guards ALWAYS | 326 | 401 |
| 10 | 20261114T135631Z-fkbypass-Y-pressure | pressure | 9 + four parallel sessions | 283 | 343 |
| 11 | 20261114T140801Z-fkbypass-Z-pressure | pressure | 10 + re-seen rows not rewritten; FK mechanism check | 287 | load phase 102 (the prototype's collection then fails at the shrink guard, as expected) |
| 12 | 20261114T141656Z-fkbypass-X-walsettings-pressure | pressure | 9 + `synchronous_commit=off`, `max_wal_size` 16 GB, `checkpoint_timeout` 60 min | 362 | 413 |

Not measured, and why:

- **National scale.** Every projection is a ratio from a 3.65 % sample.
- **Run-to-run noise on the same day** (no repeated pair); the two No_COW
  pairs differ by 4.5 % and 14 %, which bounds what a single A/B can claim.
- **Parallel sessions without task 6** (deliberately: see task 7).
- **Task 10** and the search changes as code: task 10 is estimated from
  statement times, task 5 from hand-written SQL on the loaded bench database.
- **Task 9 end to end:** the prototype measures its load phase; its
  CloseUnseen and readers are designed, not built.
- **TSE.** Not benchmarked; its loader has the same structure (hash buckets,
  Resolve after the claims), so tasks 2, 3, 6 and 7 apply by reading the code.
  Its national Resolve (1 h 45 m of 2 h 53 m) should shrink like Receita's.
- **Indexes built after a first load** into an empty table: not tried.
