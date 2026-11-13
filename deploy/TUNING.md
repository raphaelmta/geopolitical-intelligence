# Configuração do PostgreSQL

O serviço `postgres` de [`compose.yml`](compose.yml) sobe com estes parâmetros na
linha de comando (`postgres -c …`) e com `shm_size`. Cada valor marcado com uma
variável pode ser trocado no `.env`; vazio significa o valor da coluna
"Padrão", dimensionado para a **máquina de referência** (6 núcleos, 31 GB de
RAM, NVMe). Depois de mudar, rode `make up` (o PostgreSQL é recriado com a nova
linha de comando; os dados ficam).

**Todo número de desempenho publicado (tempos de carga, de busca, de caminhos,
da reconstrução da busca) precisa dizer com quais valores desta tabela foi
medido.** O ensaio da Receita ([`docs/spikes/2026-11-receita-path-queries.md`](../docs/spikes/2026-11-receita-path-queries.md))
usou valores parecidos, mas com `wal_level=minimal`, `synchronous_commit=off` e
`autovacuum=off`, que **não** são usados aqui (abaixo); seus números são
estimativas até a medição da carga nacional. `make national-check` imprime os
valores em vigor.

| Parâmetro | Variável | Padrão (31 GB) | Perfil 16 GB | `make test` / `make e2e` | Por quê |
|---|---|---|---|---|---|
| `shm_size` (do contêiner) | `PG_SHM_SIZE` | `8g` | `4g` | `512m` | Consultas paralelas guardam suas tabelas de hash em `/dev/shm`; o padrão do Docker (64 MB) faz a reconstrução da busca falhar na base nacional ("could not resize shared memory segment") |
| `shared_buffers` | `PG_SHARED_BUFFERS` | `8GB` | `4GB` | `256MB` | ~25 % da RAM |
| `effective_cache_size` | `PG_EFFECTIVE_CACHE_SIZE` | `20GB` | `10GB` | `1GB` | o que o sistema operacional costuma ter em cache; só orienta o planejador |
| `maintenance_work_mem` | `PG_MAINTENANCE_WORK_MEM` | `2GB` | `1GB` | `256MB` | criação de índices e `VACUUM` mais rápidos |
| `work_mem` | `PG_WORK_MEM` | `64MB` | `32MB` | `16MB` | por operação de ordenação/hash; a reconstrução da busca define a sua (1 GB) |
| `max_wal_size` | `PG_MAX_WAL_SIZE` | `32GB` | `16GB` | `2GB` | menos checkpoints durante as cargas; o WAL pode ocupar até isso em disco |
| `checkpoint_timeout` | `PG_CHECKPOINT_TIMEOUT` | `30min` | `30min` | `30min` | idem |
| `wal_compression` | — | `lz4` | `lz4` | `lz4` | menos WAL nas escritas de página inteira; não muda a durabilidade |
| `random_page_cost` | `PG_RANDOM_PAGE_COST` | `1.1` | `1.1` | `1.1` | NVMe/SSD: leitura aleatória quase tão barata quanto sequencial |
| `effective_io_concurrency` | `PG_EFFECTIVE_IO_CONCURRENCY` | `200` | `200` | `200` | NVMe aguenta muitas leituras simultâneas |
| `max_worker_processes` | `PG_MAX_WORKER_PROCESSES` | `8` | `8` | `8` | teto dos processos de fundo (inclui os paralelos) |
| `max_parallel_workers` | `PG_MAX_PARALLEL_WORKERS` | `6` | `4` | `2` | um por núcleo |
| `max_parallel_workers_per_gather` | `PG_MAX_PARALLEL_WORKERS_PER_GATHER` | `4` | `2` | `2` | como no ensaio |
| `max_parallel_maintenance_workers` | `PG_MAX_PARALLEL_MAINTENANCE_WORKERS` | `4` | `2` | `2` | índices criados em paralelo |
| `jit` | — | `off` | `off` | `off` | a compilação JIT atrasava as consultas curtas do ensaio |
| `max_connections` | `PG_MAX_CONNECTIONS` | `60` | `60` | `200` (test) / `60` (e2e) | worker 16 + API 10 + `migrate`/`collect`/backup/psql e reserva; `make test` roda muitos pacotes ao mesmo tempo |
| `autovacuum_max_workers` | `PG_AUTOVACUUM_MAX_WORKERS` | `4` | `3` | `3` | as quatro tabelas grandes (`company_version`, `relationship`, `person_record`, `entity`) podem ser limpas ao mesmo tempo |
| `autovacuum_work_mem` | `PG_AUTOVACUUM_WORK_MEM` | `1GB` | `512MB` | `64MB` | por processo de autovacuum (4 × 1 GB no pior caso), em vez de `maintenance_work_mem` (2 GB) |
| `autovacuum_vacuum_scale_factor` | — | `0.02` | `0.02` | `0.02` | o padrão (20 %) esperaria 12 milhões de linhas mortas numa tabela de 60 milhões; cada coleta mensal atualiza `last_seen` de quase todas as linhas uma vez, e a limpeza deve começar durante essa regravação, não semanas depois |
| `autovacuum_vacuum_insert_scale_factor` | — | `0.05` | `0.05` | `0.05` | tabelas que quase só recebem inserções: o vacuum depois de 5 % de linhas novas marca as páginas como visíveis (varreduras só de índice) e congela as linhas aos poucos, em vez de um vacuum anti-wraparound enorme mais tarde |
| `autovacuum_analyze_scale_factor` | — | `0.02` | `0.02` | `0.02` | estatísticas atualizadas depois de 2 % de mudanças (os carregadores também rodam `ANALYZE` ao fim das fases grandes) |
| `autovacuum_vacuum_cost_limit` | — | `2000` | `2000` | `2000` | o padrão (200) limita o autovacuum a poucos MB/s, e uma tabela de dezenas de GB levaria dias; NVMe aguenta 10 × mais |

Ficam nos padrões seguros do PostgreSQL (decisão do projeto): `fsync=on`,
`synchronous_commit=on`, `full_page_writes=on`, `wal_level=replica`,
`autovacuum=on`. Uma queda de energia durante uma carga não corrompe o banco nem
perde o que já foi confirmado; a carga interrompida é marcada como falha e pode
ser pedida de novo.

## Máquina com 16 GB de RAM

Copie para o `.env` as linhas do perfil 16 GB que estão comentadas no fim de
[`.env.example`](../.env.example) e rode `make up`. Com 16 GB, a carga nacional
da Receita deve caber, mas mais devagar (estimativa, não medida); a busca e os
caminhos competem por memória com o sistema operacional. Menos de 16 GB não é
recomendado para a base nacional.

## Pilhas de teste

`make test` e `make e2e` passam os valores da coluna de teste pela linha de
comando (variável `SMALL_PG` do `Makefile`), para rodar num executor de CI com
pouca memória: a mesma linha de comando de produção, com valores pequenos.
