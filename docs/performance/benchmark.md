# Load benchmark

A repeatable measurement of the data-load path: the real collection code
(`geoint collect` → worker: fetch, parse, load, CloseUnseen, Resolve, search
rebuild) on a sample of a Receita edition, run twice — the **first load** and
the **monthly re-load** — in a throwaway compose project. It exists to compare
two versions of the code or two configurations on the same machine (A/B). Its
numbers are **relative**: the sample is a few percent of the national base and
the PostgreSQL profile is much smaller than production.

Everything runs in containers (Docker, make and a POSIX shell with awk on the
host), like the rest of the repository.

## What it runs

| Piece | Where |
|---|---|
| The harness | [`scripts/bench-load.sh`](../../scripts/bench-load.sh) (`make bench-load`) |
| Compose overlay | [`deploy/compose.bench.yml`](../../deploy/compose.bench.yml) over `deploy/compose.yml`, project `geoint-bench` |
| Sample cutter and share simulator | [`cmd/loadbench`](../../cmd/loadbench/main.go) (`make bench-sample`) |
| Phase table from the worker log | [`scripts/bench-report.awk`](../../scripts/bench-report.awk) |

The project `geoint-bench` has its own fresh volumes and publishes **no host
port**; api, web and keycloak are never started. The script refuses any
project name that is not `geoint-bench` or `geoint-bench-<suffix>`, so its
`down --volumes` can never reach `geoint` or `geoint-national`. Its settings
file (random secrets) is `results/bench/env`.

## The sample

`make bench-sample UF=GO` reads an edition already downloaded to
`data/raw/receita/2026-10` (`BENCH_SRC`; the 30 data zips; nothing is
downloaded) and writes `data/bench/receita/GO/` (git-ignored):

- **Every company whose head office is in the state**, with all its
  establishment rows and all its partner rows, copied byte for byte. Cutting by
  state (and not by file part or by a hash of the CNPJ) keeps the sample
  self-consistent and keeps most of what identity resolution merges: a person
  who is a partner in several companies usually has them in one state. The
  published parts are not aligned with each other (part 1 of Empresas is a
  CNPJ range, part 1 of Socios is not), so "one part of each kind" would load
  partners of companies that are missing.
- **Two simulated editions**, `2026-10/` and `2026-11/`, each with 4 zips per
  kind, "published" on 2026-10-13 and 2026-11-10 (file dates). They differ by
  1 % of the companies in each direction (1 % only in the second edition: new
  companies; 1 % only in the first: companies that disappear) and by 1 % of
  the partner rows of the other companies in each direction. So the re-load
  sees ≈ 98 % of the claims again, inserts ≈ 1–2 % and closes ≈ 1–2 %. The
  second edition is a benchmark fiction (the real 2026-11 edition is not
  used): it exists only in the benchmark's own database.
- GO has 3.65 % of the country's head offices: 2.55 M companies, 1.05 M
  partner rows per edition. Other sizes: `UF=MG` (10.9 %), `UF=SP` (28.9 %),
  or several states (`UF=GO,DF`).

The sample holds personal data as published by the Receita. It stays under
`data/` (git-ignored); the harness records counts and timings only, never
rows, and statement texts are stored with their literals removed.

`cmd/loadbench share` serves the two folders the way the Receita share
answers (PROPFIND, GET with Range). The worker of the bench project downloads
from it through its normal `Fetch` path (`RECEITA_BASE_URL`), stores the files
in the project's MinIO and loads them; `RECEITA_MIN_PARTS=1` and
`RECEITA_SETTLE_HOURS=0` relax the completeness rules for the sample.

## Running

```sh
make cache                      # once: the shared Go build cache volume
make bench-sample UF=GO         # once: ≈ 6 min
make bench-load LABEL=baseline  # first load + re-load + report + teardown
```

Options are environment variables (the script's header lists all of them):

| Variable | Meaning |
|---|---|
| `LABEL` | name of the run; results go to `results/bench/<UTC time>-<label>/` |
| `STEP` | `all` (default), or one of `up`, `first`, `reload`, `snapshot`, `report`, `down` to keep the stack between steps (experiments on the loaded database) |
| `KEEP=1` | with `STEP=all`, leave the stack up at the end (remove it later with `STEP=down`) |
| `BENCH_PROFILE` | `roomy` (default) or `pressure`, below |
| `BENCH_NOCOW=1` | btrfs only: set No_COW (`chattr +C`) on the empty pgdata volume before `initdb` |
| `BENCH_PG_EXTRA` | `name=value;…` applied with `ALTER SYSTEM` (bench database only) |
| `BENCH_WORKER_SET` | `name=value;…` applied with `ALTER ROLE geoint_worker SET` (e.g. `synchronous_commit=off`) |
| `BENCH_SQL_AFTER_MIGRATE` | a `.sql` file run as the owner after the migrations (experiments: drop a trigger or an index) |
| `BENCH_SAMPLE_DIR` | another sample (default `data/bench/receita/GO`) |

Run **one configuration at a time** and note what else the machine is doing:
`config.txt` records the number of other running containers and the memory
available at the start.

### PostgreSQL profiles

The command line is the production one (`deploy/compose.yml`); only values
differ. Both profiles are far below the reference profile of
[`deploy/TUNING.md`](../../deploy/TUNING.md) because the machine is shared.

| | `roomy` | `pressure` |
|---|---|---|
| `shared_buffers` | 4GB | 320MB |
| container memory limit (also bounds the page cache charged to PostgreSQL) | none | 1100 MB |
| `max_wal_size` | 4GB | 2GB |
| `maintenance_work_mem` | 1GB | 128MB |
| what it shows | CPU, trigger, WAL and statement-shape costs; the whole sample fits in memory | the same with memory at about one fifth of the database, the proportion of the national run (8 GB + page cache against 90 GB): random reads become real I/O |

`pressure` exists because the pathologies of the national load (cost per row
growing as tables outgrow memory, `DataFileRead` waits) do not appear when a
3 GB database sits in 20 GB of free memory.

## What a run records

`results/bench/<UTC time>-<label>/`:

| File | Content |
|---|---|
| `config.txt` | label, commit, sample, profile, filesystem of the Docker root, No_COW or not, every non-default PostgreSQL setting |
| `summary.md` | the phase table, the wait-event distribution, totals after each collection |
| `phases.tsv` | collection, phase, step, rows, seconds — from the worker's own step log lines |
| `worker.log`, `postgres.log` | full logs (checkpoints, autovacuum runs) |
| `statements-first.tsv`, `statements-reload.tsv` | top 60 statements of each collection from `pg_stat_statements`: calls, time, blocks hit/read/dirtied, read time, WAL records/full-page images/bytes |
| `waits.tsv` | every second: wait event of each active backend, with the first 60 characters of its statement |
| `counters.tsv` | every 10 s: WAL bytes and full-page images, database size, checkpoints, blocks read/hit per claim table and per index, `pg_stat_io` |
| `tables-*.tsv`, `sizes-*.tsv`, `functions-*.tsv`, `totals-*.tsv` | inserts/updates/HOT updates per table, relation sizes, calls and time of the trigger functions, row counts and WAL totals |

Statistics are reset after each collection, so `*-reload.*` covers the
re-load alone.

## Reading the results against the national run

The national figures are in [`2026-national-load.md`](2026-national-load.md).
A sample cannot reproduce absolute times. What transfers, and how the design
document ([`2026-load-optimization-design.md`](2026-load-optimization-design.md))
uses it:

- **ratios between two runs of the same harness** (a fix against the
  baseline);
- **per-row costs by cause** (trigger, foreign-key check, index maintenance,
  WAL), multiplied by national row counts, with the cache-pressure factor
  taken from the `pressure` profile;
- **not** the absolute duration of a phase, and not effects that depend on
  the absolute size of an index or table relative to 8 GB of shared buffers
  unless the `pressure` profile reproduces them.
