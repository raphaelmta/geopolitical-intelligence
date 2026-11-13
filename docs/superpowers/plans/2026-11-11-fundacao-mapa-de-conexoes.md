# Fundação e Mapa de Conexões Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Entregar a bancada do analista: coleta de Receita, TSE, Câmara, Senado e CVM, um mapa de conexões com procedência e histórico, e uma interface para buscar, navegar, achar caminhos e ver grupos de controle, com a base nacional carregada na máquina de referência.

**Architecture:** Um binário Go (`geoint`) com subcomandos `api`, `worker`, `migrate` e `collect`, sobre PostgreSQL (mapa em tabelas de entidades e vínculos), NATS JetStream (coletas e tarefas demoradas) e MinIO (arquivos brutos). Keycloak emite tokens OIDC que a API só valida. Interface React/TypeScript fala apenas com a API. As regras de dados (procedência obrigatória, nada apagado, fato separado de inferência) são impostas no banco por restrições e gatilhos, não só no código.

**Tech Stack:** Go (stdlib `net/http`, `pgx/v5`, `nats.go`, `minio-go/v7`, `coreos/go-oidc/v3`, `pressly/goose/v3`, `google/uuid`), PostgreSQL 18 com `pg_trgm`, NATS JetStream, MinIO, Keycloak, React + TypeScript + Vite, TanStack Query, React Router, `react-oidc-context`, Cytoscape.js, Vitest, Playwright, Docker Compose.

**Spec:** `docs/superpowers/specs/2026-11-11-fundacao-mapa-de-conexoes-design.md`

## Global Constraints

- Licença AGPL-3.0; toda dependência precisa ser compatível.
- Código e nomes internos em inglês; interface e documentação de uso em português do Brasil.
- A plataforma ainda não tem nome. Nome de trabalho: `geoint` (módulo Go, binário, banco, realm). Não espalhar o nome em textos da interface.
- Go e Node.js não estão instalados na máquina: todo build e teste roda em contêiner via `make`. Nenhum passo pode pedir `go` ou `npm` no host.
- Só dados públicos. Nunca guardar mais do que a base publica (CPF mascarado continua mascarado).
- Toda afirmação gravada tem `collection_id` e `record_ref`. Sem isso, o banco recusa.
- Nenhuma linha de afirmação é apagada. Mudança encerra a antiga e grava a nova.
- Inferência sempre tem `confidence`; fato nunca é exibido igual a inferência.
- Só nome igual nunca junta pessoas automaticamente.
- Consultas ao banco sempre parametrizadas. Segredos só por variável de ambiente. Contêineres sem root. Só `web` e `api` publicam porta no host.
- Toda rota da API exige token válido, exceto `GET /healthz`.
- Metas na máquina de referência (6 núcleos, 31 GB, NVMe) com base nacional: busca e página de entidade ≤ 1 s; expansão de vizinhos ≤ 2 s; caminhos até profundidade 6 e grupo de controle ≤ 2 min em segundo plano; carga nacional da Receita ≤ 12 h.
- Pré-requisito do dono: configurar `git config user.name` e `user.email` no repositório antes do primeiro commit (o e-mail fica público no histórico).
- Versões: na Tarefa 2, fixar por tag exata a versão estável corrente de cada imagem e registrar em `deploy/VERSIONS.md`. Nunca usar `latest`.

## Review Focus

1. **Coleta parcial ou corrompida da Receita.** Um download incompleto faria milhões de vínculos parecerem "saídas". Esperado: a coleta falha, nada é encerrado. Teste na Tarefa 8.
2. **Nós gigantes em caminhos.** Partidos, Câmara, fundos e holdings com milhares de vínculos ligam todo mundo a todo mundo em 2 saltos. Esperado: a busca não expande nós acima de um grau limite e informa onde parou. Teste na Tarefa 9.
3. **Linhas malformadas nos CSV.** Latin-1, aspas soltas, bytes nulos, colunas faltando. Esperado: a linha é pulada, contada e reportada nas estatísticas da coleta; a carga não cai. Teste na Tarefa 7.
4. **Mesma coleta executada duas vezes.** Esperado: nenhuma entidade, ficha ou vínculo duplicado. Teste nas Tarefas 3 e 7.
5. **Busca como as pessoas digitam.** Minúsculas, acentos, CNPJ com pontuação (`12.345.678/0001-90`), só os 8 primeiros dígitos. Esperado: encontra o mesmo resultado. Teste na Tarefa 12.

## Estrutura de arquivos

```
Makefile                         alvos: up, down, test, test-web, e2e, lint, vuln
LICENSE                          AGPL-3.0
deploy/
  compose.yml                    postgres, nats, minio, keycloak, api, worker, web
  compose.dev.yml                serviços `go` e `node` para build e teste
  Dockerfile.geoint  Dockerfile.web
  keycloak/realm-geoint.json     realm, cliente geoint-web, papéis analyst/admin
  VERSIONS.md
cmd/geoint/main.go               subcomandos
internal/
  db/            pool, migrações embutidas (migrations/*.sql)
  claims/        tipos e gravação de entidades, fichas, vínculos, versões
  collection/    ciclo de vida de uma coleta (lote)
  rawstore/      arquivos brutos em S3
  queue/         JetStream: streams, publicar, consumir
  sources/       cadastro das bases e prazos de validade
  connector/     contrato e executor
  connector/receita/  tse/  camara/  senado/  cvm/
  identity/      normalização, resolução, decisões do analista
  graph/         vizinhos, caminhos, grupo de controle
  jobs/          tarefas demoradas
  freshness/     situação das bases e agendador
  auth/          validação OIDC e papéis
  audit/         registro de auditoria
  api/           rotas HTTP
  testutil/      banco, fila e S3 para testes
testdata/<fonte>/                recortes reais públicos, pequenos
web/                             aplicação React
docs/sources/<fonte>.md          formato confirmado de cada base
docs/spikes/                     relatório do teste de risco
```

---

## Fase 0 — Teste do maior risco

### Task 1: Teste descartável de carga nacional e busca de caminhos

Decide, antes de qualquer outra construção, se o PostgreSQL atende às metas de consulta com a Receita nacional.

**Files:**
- Create: `spike/schema.sql`, `spike/load.sh`, `spike/queries.sql`, `spike/bfs/main.go`, `spike/compose.yml`
- Create: `docs/sources/receita.md`, `docs/spikes/2026-11-receita-path-queries.md`
- Create: `.gitignore` (inclui `data/`)

**Interfaces:**
- Produces: `docs/sources/receita.md` (URL atual, lista de arquivos, separador, codificação, colunas em ordem, códigos de qualificação de sócio e de natureza jurídica de fundos). `docs/spikes/2026-11-receita-path-queries.md` com a decisão **PATHS_STRATEGY** = `bfs` ou `cte`. Arquivos brutos em `data/raw/receita/<AAAA-MM>/`, reaproveitados na Tarefa 20.

- [ ] **Step 1: Confirmar o formato atual da Receita e registrar em `docs/sources/receita.md`**

Conferir na página oficial de dados abertos do CNPJ. Esperado, a confirmar: arquivos `Empresas0..9.zip`, `Estabelecimentos0..9.zip`, `Socios0..9.zip`, CSV sem cabeçalho, separador `;`, Latin-1. Colunas esperadas de Sócios: `cnpj_basico, identificador_socio (1 PJ, 2 PF, 3 estrangeiro), nome_socio, cnpj_cpf_socio (PF mascarado ***NNNNNN**), qualificacao_socio, data_entrada_sociedade, pais, representante_legal, nome_representante, qualificacao_representante, faixa_etaria`. Registrar qualquer diferença.

- [ ] **Step 2: Baixar Empresas, Estabelecimentos e Sócios do mês corrente para `data/raw/receita/<AAAA-MM>/`**

`spike/load.sh download` com retomada (`curl -C -`) e conferência de tamanho. Verificar: `unzip -t` passa em todos os arquivos.

- [ ] **Step 3: Carregar em tabelas `UNLOGGED` com `COPY ... ENCODING 'LATIN1'` e cronometrar**

`spike/schema.sql` cria `s_company(cnpj_basico, name, legal_nature)`, `s_partner(...)` e deriva `s_node(id, kind, key, name_norm)` e `s_edge(src, dst, rel)` com pessoa identificada por `(nome normalizado, 6 dígitos do CPF)`. Índices em `s_edge(src)`, `s_edge(dst)` e GIN `pg_trgm` em `s_node(name_norm)`. Registrar tempo de carga e `pg_database_size`.

- [ ] **Step 4: Medir as cinco metas**

| Medida | Como | Meta |
|---|---|---|
| M1 busca por nome | 50 termos, trigram em `s_node` | p95 ≤ 1 s |
| M2 vizinhos | 200 nós aleatórios + os 20 de maior grau, paginado em 200 | p95 ≤ 2 s |
| M3a caminhos, CTE recursiva | 20 pares aleatórios + 5 pares passando por nós de grau > 1000, profundidade ≤ 6 | cada ≤ 120 s |
| M3b caminhos, BFS bidirecional em Go (`spike/bfs`) com consultas `= ANY($1)` por fronteira e sem expandir nós de grau > 500 | mesmos pares | cada ≤ 120 s |
| M4 grupo de controle | 20 empresas, incluindo as 5 holdings com mais participadas | cada ≤ 120 s |
| M5 carga | tempo total do Step 3 | ≤ 12 h |

- [ ] **Step 5: Escrever `docs/spikes/2026-11-receita-path-queries.md` com números e a decisão**

Regra: M3b passa → `PATHS_STRATEGY = bfs`. Só M3a passa → `cte`. Nenhuma passa, ou M1, M2, M4 ou M5 falha → **PARAR o plano** e devolver ao Rafael o relatório com os números e as alternativas avaliadas (grafo em memória num serviço Go, Apache AGE, banco de grafos dedicado). Não seguir para a Tarefa 2 nesse caso.

- [ ] **Step 6: Remover `spike/`, manter os dois documentos e `data/` (ignorado pelo git), e commitar**

```bash
rm -rf spike
git add .gitignore docs/sources/receita.md docs/spikes
git commit -m "docs: record Receita format and path-query spike results"
```

---

## Fase 1 — Fundação

### Task 2: Esqueleto do repositório, ambiente local e CI

**Files:**
- Create: `go.mod` (module `geoint`), `cmd/geoint/main.go`, `internal/api/health.go`, `internal/api/health_test.go`
- Create: `Makefile`, `LICENSE`, `README.md`, `deploy/compose.yml`, `deploy/compose.dev.yml`, `deploy/Dockerfile.geoint`, `deploy/VERSIONS.md`, `.env.example`, `.github/workflows/ci.yml`

**Interfaces:**
- Produces: `make up | down | test | lint | vuln`. `geoint api` escuta em `:8080`. `func api.NewRouter(deps api.Deps) http.Handler` com `GET /healthz` → `200 {"status":"ok"}`. Variáveis: `DATABASE_URL`, `NATS_URL`, `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_BUCKET`, `OIDC_ISSUER`, `OIDC_AUDIENCE`.

- [ ] **Step 1: Escrever o teste que falha**

```go
func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	api.NewRouter(api.Deps{}).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: Criar `Makefile` e `deploy/compose.dev.yml`; rodar `make test` e ver falhar**

`make test` sobe `postgres`, `nats` e `minio` e executa `go test ./...` no serviço `go` da mesma rede, com `TEST_DATABASE_URL`, `TEST_NATS_URL`, `TEST_S3_ENDPOINT`. Esperado: FAIL, pacote `api` não existe.

- [ ] **Step 3: Implementar `api.NewRouter`, `cmd/geoint` com subcomandos `api`, `worker`, `migrate`, `collect`, `version` (os três do meio retornam "not implemented" por ora)**

- [ ] **Step 4: Escrever `deploy/compose.yml` e `Dockerfile.geoint`**

Build em estágios, imagem final sem shell, `USER 65532`. Só `api` (8080) e `web` (5173) publicam porta; `keycloak` publica 8081 porque o navegador precisa alcançá-lo. Postgres, NATS e MinIO sem porta no host. Fixar versões e registrar em `deploy/VERSIONS.md`. `LICENSE` com o texto integral da AGPL-3.0.

- [ ] **Step 5: CI**

`.github/workflows/ci.yml`: `make test`, `make lint` (`go vet`), `make vuln` (`govulncheck ./...`).

- [ ] **Step 6: Verificar**

Run: `make test` → PASS. Run: `make up && curl -s localhost:8080/healthz` → `{"status":"ok"}`. Run: `docker compose -f deploy/compose.yml ps --format '{{.Service}} {{.Ports}}'` → só `api`, `web` e `keycloak` com porta no host.

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "chore: scaffold repo, local stack and CI"
```

### Task 3: Esquema do banco e regras de dados

**Files:**
- Create: `internal/db/db.go`, `internal/db/migrations/0001_core.sql`, `internal/testutil/db.go`
- Create: `internal/claims/types.go`, `internal/claims/store.go`, `internal/claims/store_test.go`
- Create: `internal/collection/collection.go`, `internal/collection/collection_test.go`, `internal/sources/sources.go`

**Interfaces:**
- Produces:

```go
// internal/db
func Open(ctx context.Context, url string) (*pgxpool.Pool, error)
func Migrate(ctx context.Context, url string) error
// internal/testutil
func DB(t *testing.T) *pgxpool.Pool // esquema novo e migrado por teste, removido no Cleanup

// internal/sources
type Source struct{ ID int16; Key, Name string; PublishEvery, StaleAfter time.Duration; Snapshot bool }
var All = []Source{ // Snapshot: a base publica o estado completo a cada vez
	{1, "receita", "Receita Federal — CNPJ", 30 * day, 45 * day, true},
	{2, "tse", "TSE — candidaturas e contas", 180 * day, 200 * day, false},
	{3, "camara", "Câmara dos Deputados", 7 * day, 14 * day, false},
	{4, "senado", "Senado Federal", 7 * day, 14 * day, false},
	{5, "cvm", "CVM — companhias abertas", 7 * day, 30 * day, true},
}
func ByKey(key string) (Source, bool)

// internal/collection
type Scope struct{ Full bool; CNPJs []string; Depth int }
type Collection struct{ ID int64; SourceID int16; ReferenceDate time.Time; Scope Scope; Status string } // running|done|failed
func Begin(ctx context.Context, p *pgxpool.Pool, sourceID int16, ref time.Time, s Scope) (Collection, error)
func Finish(ctx context.Context, p *pgxpool.Pool, id int64, stats map[string]int64) error
func Fail(ctx context.Context, p *pgxpool.Pool, id int64, cause error) error
func LastDone(ctx context.Context, p *pgxpool.Pool, sourceID int16) (Collection, bool, error)

// internal/claims
type EntityKind string // person | company | party | public_body | foreign
type RelType string    // partner_of | administrator_of | legal_rep_of | donated_to | candidate_for | holds_mandate | controls | shareholder_of
type ClaimKind string  // fact | inference
type Provenance struct{ CollectionID int64; RecordRef string }
type PersonRecordInput struct{ SourceID int16; ContextKey, Name string; CPFFragment string; Prov Provenance }
type RelationshipInput struct {
	Type RelType; SrcEntityID, DstEntityID int64; SrcRecordID, DstRecordID *int64
	Attrs map[string]any; Kind ClaimKind; Confidence *float64; ValidFrom *time.Time; Prov Provenance
}
type CompanyVersionInput struct {
	EntityID int64; Name, LegalNature, Size, UF, City, CNAE, Status string; Capital *float64; Prov Provenance
}
func NewStore(p *pgxpool.Pool) *Store
func (s *Store) UpsertEntity(ctx context.Context, kind EntityKind, naturalKey, name string) (int64, error)
func (s *Store) UpsertPersonRecord(ctx context.Context, in PersonRecordInput) (recordID, entityID int64, err error)
func (s *Store) AssertRelationship(ctx context.Context, in RelationshipInput) (int64, error)
func (s *Store) PutCompanyVersion(ctx context.Context, in CompanyVersionInput) error
var ErrNoProvenance, ErrInferenceNeedsConfidence error
```

- Tabelas de `0001_core.sql` (colunas além das óbvias):
  - `source`, `collection(reference_date, scope jsonb, status, started_at, finished_at, stats jsonb, error)`
  - `entity(id, kind, natural_key, name, name_norm)`, único em `(kind, natural_key)` quando não nulo; GIN `pg_trgm` em `name_norm`; btree `text_pattern_ops` em `natural_key`. Empresa usa `natural_key = cnpj_basico` (8 dígitos).
  - `person_record(id, source_id, context_key, name, name_norm, cpf_fragment, entity_id, identity_confidence real default 1, locked_by_decision_id, first_collection_id, record_ref)`, único em `(source_id, context_key, name_norm, cpf_fragment)`. Uma ficha por pessoa por contexto (na Receita, o contexto é o `cnpj_basico` da empresa).
  - `relationship(id, rel_type, src_entity_id, dst_entity_id, src_record_id, dst_record_id, attrs jsonb, kind, confidence, valid_from, valid_to_after, valid_to_before, first_collection_id NOT NULL, record_ref NOT NULL, last_seen_collection_id, closed_collection_id, recorded_at)`, índices em `src_entity_id` e `dst_entity_id`, `CHECK (kind = 'fact' OR confidence IS NOT NULL)`. Chave natural única: `(rel_type, coalesce(src_record_id, -src_entity_id), dst_entity_id, attrs->>'role', valid_from)` entre os não encerrados.
  - `company_version(entity_id, ..., first_collection_id NOT NULL, record_ref NOT NULL, last_seen_collection_id, closed_collection_id)`.
  - Gatilho `forbid_delete()` em `entity`, `person_record`, `relationship`, `company_version`, `collection`: `RAISE EXCEPTION 'claims are never deleted'`.
  - `src_entity_id`/`dst_entity_id` de `relationship` e `entity_id` de `person_record` são ponteiros derivados da identidade e podem ser atualizados; as demais colunas de afirmação, não.

- [ ] **Step 1: Escrever os testes que falham (`internal/claims/store_test.go`)**

```go
func TestRelationshipWithoutProvenanceIsRejected(t *testing.T) {
	s := claims.NewStore(testutil.DB(t))
	_, err := s.AssertRelationship(ctx, claims.RelationshipInput{Type: "partner_of", SrcEntityID: a, DstEntityID: b, Kind: "fact"})
	if !errors.Is(err, claims.ErrNoProvenance) { t.Fatalf("got %v", err) }
}
func TestInferenceWithoutConfidenceIsRejected(t *testing.T) // errors.Is(err, ErrInferenceNeedsConfidence)
func TestDeleteIsForbiddenByDatabase(t *testing.T)          // DELETE FROM relationship → erro contendo "never deleted"
func TestProvenanceConstraintHoldsForRawSQL(t *testing.T)   // INSERT direto com record_ref NULL → violação NOT NULL
func TestAssertSameRelationshipTwiceIsIdempotent(t *testing.T) {
	// mesma entrada em duas coletas: 1 linha, first_collection_id = c1, last_seen_collection_id = c2
}
func TestCompanyVersionChangeClosesOldAndKeepsIt(t *testing.T) {
	// nome muda em c2: 2 linhas; a antiga com closed_collection_id = c2; a nova aberta
}
func TestUpsertPersonRecordCreatesSingletonEntity(t *testing.T) // ficha nova → entidade person própria, confidence 1
```

- [ ] **Step 2: Rodar e ver falhar**

Run: `make test` → FAIL, pacote `claims` não compila.

- [ ] **Step 3: Escrever a migração, `db.Migrate` (goose com `embed`), `testutil.DB`, `sources`, `collection` e `claims.Store`**

`geoint migrate` chama `db.Migrate` e insere/atualiza as linhas de `sources.All`.

- [ ] **Step 4: Rodar e ver passar**

Run: `make test` → PASS.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: core schema with provenance and no-delete rules"
```

### Task 4: Arquivos brutos, fila e contrato de conector

**Files:**
- Create: `internal/rawstore/rawstore.go`, `internal/rawstore/rawstore_test.go`
- Create: `internal/queue/queue.go`, `internal/queue/queue_test.go`
- Create: `internal/connector/connector.go`, `internal/connector/runner.go`, `internal/connector/runner_test.go`
- Modify: `cmd/geoint/main.go` (subcomandos `worker` e `collect`)

**Interfaces:**
- Consumes: `collection.*`, `claims.Store`, `sources.ByKey`.
- Produces:

```go
// internal/rawstore
type File struct{ Key, SHA256 string; Size int64 }
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) (File, error) // key: raw/<fonte>/<AAAA-MM-DD>/<arquivo>
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Exists(ctx context.Context, key string) (bool, error)
}
func NewS3(endpoint, access, secret, bucket string) (Store, error)

// internal/queue
const StreamIngest, StreamJobs = "INGEST", "JOBS"
func Connect(url string) (*Client, error)           // cria os streams se não existirem
func (c *Client) Publish(ctx context.Context, subject string, v any) error
func (c *Client) Consume(ctx context.Context, stream, consumer, subject string, h func(context.Context, []byte) error) error
// assuntos: ingest.request.<fonte>, jobs.run.<tipo>. Ack só após h retornar nil; erro → Nak com espera.

// internal/connector
type Request struct{ Source string; ReferenceDate time.Time; Scope collection.Scope }
type Stats map[string]int64 // inclui obrigatoriamente "rows_read", "rows_skipped"
type Connector interface {
	Key() string
	Fetch(ctx context.Context, ref time.Time, raw rawstore.Store) ([]rawstore.File, error)
	Load(ctx context.Context, col collection.Collection, files []rawstore.File, raw rawstore.Store, pool *pgxpool.Pool) (Stats, error)
}
type Hooks struct { // preenchidos nas Tarefas 6 e 8; nil = não faz nada
	CloseUnseen func(ctx context.Context, src sources.Source, col collection.Collection) (int64, error)
	Resolve     func(ctx context.Context, collectionID int64) error
}
func Run(ctx context.Context, c Connector, req Request, pool *pgxpool.Pool, raw rawstore.Store, h Hooks) error
func Register(c Connector); func Get(key string) (Connector, bool)
```

`Run`: `Begin` → `Fetch` → `Load` → `h.CloseUnseen` (só se `Source.Snapshot`) → `h.Resolve` → `Finish`. Qualquer erro: `Fail` e nenhuma etapa seguinte executa.

- [ ] **Step 1: Escrever os testes que falham**

```go
func TestPutStoresBytesAndReturnsSHA256(t *testing.T) // conteúdo "abc" → SHA256 ba7816bf...15ad, Open devolve "abc"
func TestConsumeRedeliversWhenHandlerFails(t *testing.T) // handler falha na 1ª, passa na 2ª; chamado exatamente 2 vezes
func TestRunMarksCollectionDoneAndStoresStats(t *testing.T) // conector falso; status "done", stats["rows_read"] == 3
func TestRunFailureSkipsCloseUnseenAndMarksFailed(t *testing.T) {
	// Load retorna erro: status "failed", coluna error preenchida, hook CloseUnseen nunca chamado
}
func TestRunSkipsCloseUnseenForNonSnapshotSource(t *testing.T)
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 3: Implementar `rawstore` (minio-go), `queue` (nats.go/jetstream), `connector.Run`, `geoint worker` (consome `ingest.request.*`) e `geoint collect <fonte> [--cnpjs a,b --depth N] [--ref AAAA-MM-DD]` (publica o pedido)**
- [ ] **Step 4: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: raw store, queue and connector runner"
```

### Task 5: Normalização de nomes e CPF

**Files:**
- Create: `internal/identity/normalize.go`, `internal/identity/normalize_test.go`

**Interfaces:**
- Produces: `func NormalizeName(s string) string` · `func CPFFragment(s string) (string, bool)` · `func CNPJBase(s string) (string, bool)`

- [ ] **Step 1: Escrever os testes que falham**

```go
func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{
		"José  da Silva":      "JOSE DA SILVA",
		" joão d'ávila júnior ": "JOAO D AVILA JUNIOR",
		"MARIA-APARECIDA":     "MARIA APARECIDA",
		"":                    "",
	} { if got := identity.NormalizeName(in); got != want { t.Errorf("%q: got %q want %q", in, got, want) } }
}
func TestCPFFragment(t *testing.T) {
	// "***123456**" → "123456", true     (mascarado da Receita)
	// "11122233344" → "222333", true     (CPF completo: dígitos 4 a 9)
	// "111.222.333-44" → "222333", true
	// "" → "", false;  "-4" → "", false;  "12345" → "", false
}
func TestCNPJBase(t *testing.T) {
	// "12.345.678/0001-90" → "12345678"; "12345678" → "12345678"; "1234" → "", false
}
```

`CPFFragment` devolve `"", false` para vazio, para valores não numéricos como `"-4"` e para qualquer entrada que não tenha 6 ou 11 dígitos após remover máscara e pontuação.

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 3: Implementar as três funções** (remoção de acentos com `golang.org/x/text/unicode/norm`; pontuação vira espaço; espaços colapsados).
- [ ] **Step 4: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: name and document normalization"`

### Task 6: Resolução de identidades

**Files:**
- Create: `internal/db/migrations/0002_identity.sql`, `internal/identity/resolver.go`, `internal/identity/resolver_test.go`
- Modify: `cmd/geoint/main.go` (liga `Hooks.Resolve`)

**Interfaces:**
- Consumes: `claims.Store.UpsertPersonRecord`, `identity.NormalizeName`.
- Produces:

```go
type Resolver struct{ Pool *pgxpool.Pool; CommonNameThreshold int } // padrão 1000, env IDENTITY_COMMON_NAME_THRESHOLD
type Result struct{ AutoMerged, Proposed int64 }
type Candidate struct{ EntityID int64; Name string; Records int; SharedFragment bool }
func (r *Resolver) Resolve(ctx context.Context, collectionID int64) (Result, error)
func (r *Resolver) Decide(ctx context.Context, proposalID int64, confirm bool, user string) error
func (r *Resolver) Merge(ctx context.Context, recordIDs []int64, user, reason string) (entityID int64, err error)
func (r *Resolver) Split(ctx context.Context, recordIDs []int64, user, reason string) (entityID int64, err error)
func (r *Resolver) PossibleSame(ctx context.Context, entityID int64, limit int) ([]Candidate, error)
```

- Tabelas: `identity_decision(id, kind auto|analyst, action merge|split|reject, record_ids bigint[], entity_id, confidence, reason, decided_by, decided_at)` e `identity_proposal(id, group_key unique, record_ids bigint[], reason, status pending|confirmed|rejected, connections int, decided_by, decided_at)`.
- Regras:
  - Grupo = fichas com mesmo `(name_norm, cpf_fragment)`, fragmento não vazio, de qualquer base.
  - `n` = número de fragmentos distintos para aquele `name_norm`. Se `n ≤ CommonNameThreshold`: junta, `confidence = 1 − n/1_000_000`, grava em `person_record.identity_confidence` e numa `identity_decision` `auto`. Se `n > CommonNameThreshold`: cria ou atualiza uma `identity_proposal` `pending` (uma por grupo), sem juntar.
  - Juntar = apontar `person_record.entity_id` das fichas para uma entidade e atualizar `relationship.src_entity_id`/`dst_entity_id` das linhas que citam essas fichas.
  - Ficha com `locked_by_decision_id` nunca é movida por `Resolve`. `Merge`, `Split` e `Decide` travam as fichas envolvidas.
  - Proposta `rejected`: o grupo nunca mais é juntado nem reproposto.
  - `PossibleSame`: entidades de pessoa com mesmo `name_norm` e fragmento diferente ou ausente, calculadas na hora, nunca juntadas.
  - `connections` da proposta = total de vínculos das fichas; a fila é ordenada por ele.

- [ ] **Step 1: Escrever os testes que falham**

```go
func TestStrongMatchMergesWithConfidence(t *testing.T) {
	// 2 fichas "ANA RARA PEIXOTO"/"123456" em empresas diferentes; nome com 1 fragmento distinto
	// → mesma entidade; identity_confidence == 1 - 1.0/1e6; 1 identity_decision kind=auto
}
func TestNameOnlyNeverMerges(t *testing.T)        // mesmo nome, fragmentos "111111" e "222222" → entidades diferentes
func TestMissingFragmentNeverMerges(t *testing.T) // mesmo nome, ambos sem fragmento → entidades diferentes
func TestCommonNameGoesToQueueNotMerged(t *testing.T) {
	// threshold 2; "JOSE DA SILVA" com 3 fragmentos distintos; 2 fichas com "123456"
	// → entidades diferentes; 1 identity_proposal pending com as 2 fichas
}
func TestMergeRepointsRelationships(t *testing.T) // após juntar, os vínculos das 2 fichas têm o mesmo src_entity_id
func TestSplitSurvivesNextResolve(t *testing.T)   // Split e depois Resolve de nova coleta → continuam separadas
func TestRejectedProposalIsNeverReproposed(t *testing.T)
func TestConfirmedProposalSurvivesRecollection(t *testing.T)
func TestCrossSourceStrongMatch(t *testing.T)     // ficha Receita "***222333**" e ficha TSE CPF "11122233344", mesmo nome → juntadas
func TestPossibleSameListsNameOnlyCandidates(t *testing.T)
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 3: Implementar a migração e o `Resolver`**

`Resolve` trabalha por conjuntos em SQL, restrito aos `name_norm` tocados pela coleta, para rodar na escala nacional.

- [ ] **Step 4: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: identity resolution with analyst decisions"`

---

## Fase 2 — Receita e consultas ao mapa

### Task 7: Conector da Receita — carga

**Files:**
- Create: `internal/connector/receita/receita.go`, `fetch.go`, `load.go`, `load_test.go`, `internal/db/migrations/0003_receita_staging.sql`
- Create: `testdata/receita/Empresas.csv`, `Estabelecimentos.csv`, `Socios.csv`, `testdata/receita/README.md`

**Interfaces:**
- Consumes: `connector.Connector`, `docs/sources/receita.md`, `identity.NormalizeName`, `identity.CPFFragment`.
- Produces: `func New(baseURL string) connector.Connector` com `Key() == "receita"`, registrado em `init`. Grava: entidades `company` (chave `cnpj_basico`), `company_version` com dados da matriz (`matriz_filial = 1`; filiais não são carregadas), fichas de sócio PF com `context_key = cnpj_basico`, entidades `foreign` para sócio estrangeiro (`natural_key = "receita:" + cnpj_basico + ":" + name_norm`), vínculos `partner_of` (sócio → empresa, `attrs.role` = código de qualificação, `valid_from` = data de entrada), `administrator_of` quando a qualificação é de administração (lista de códigos em `docs/sources/receita.md`) e `legal_rep_of` (representante → sócio). `record_ref` = `"<arquivo>:<linha>"`.

- [ ] **Step 1: Montar o recorte real de teste**

A partir de `data/raw/receita/`, extrair para `testdata/receita/` cerca de 30 empresas reais que formem: uma cadeia de 3 níveis de PJ sócia de PJ, uma pessoa sócia de 3 empresas, um sócio estrangeiro, uma empresa de sócio único. Acrescentar ao final 4 linhas malformadas feitas à mão (aspas soltas, byte nulo, coluna faltando, data `00000000`) e documentar em `testdata/receita/README.md` quais CNPJs e por quê.

- [ ] **Step 2: Escrever os testes que falham**

```go
func TestLoadCreatesCompaniesPartnersAndProvenance(t *testing.T) {
	// após Load do recorte: nº de entidades company == empresas do recorte;
	// todo relationship tem first_collection_id == col.ID e record_ref no formato "Socios.csv:<n>"
}
func TestPJPartnerLinksCompanyToCompany(t *testing.T)   // sócio PJ → src é a entidade company de CNPJ conhecido
func TestPersonPartnerCreatesOneRecordPerCompany(t *testing.T) // a pessoa de 3 empresas → 3 person_record
func TestMalformedRowsAreSkippedAndCounted(t *testing.T) { // Review Focus 3
	// stats["rows_skipped"] == 4; Load retorna nil; demais linhas carregadas
}
func TestLoadTwiceSameCollectionDateIsIdempotent(t *testing.T) { // Review Focus 4
	// duas coletas do mesmo recorte: contagens de entity, person_record, relationship iguais às da primeira
}
func TestZeroDateBecomesNullValidFrom(t *testing.T)
func TestSliceScopeLoadsOnlyRequestedCNPJsAndNeighbors(t *testing.T) // Scope{CNPJs:[x], Depth:1} → x, seus sócios PJ e participadas
```

- [ ] **Step 3: Rodar e ver falhar** — Run: `make test` → FAIL.

- [ ] **Step 4: Implementar `Fetch` e `Load`**

`Fetch`: baixa cada zip com retomada, grava em `rawstore`, pula o que já existe com mesmo tamanho. `Load`: descompacta em fluxo, converte Latin-1, valida cada linha em Go (pulando e contando as inválidas) e envia as válidas por `pgx.CopyFrom` para tabelas de preparação `UNLOGGED`; depois, instruções SQL por conjunto fazem o upsert em `entity`, `company_version`, `person_record` e `relationship`, obedecendo às chaves naturais da Tarefa 3. Em recorte, as tabelas de preparação são filtradas por alcance a partir dos CNPJs pedidos antes do upsert.

- [ ] **Step 5: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: Receita connector bulk load"`

### Task 8: Histórico entre coletas

**Files:**
- Create: `internal/claims/close.go`, `internal/claims/close_test.go`
- Modify: `cmd/geoint/main.go` (liga `Hooks.CloseUnseen`)

**Interfaces:**
- Consumes: `collection.LastDone`, `sources.Source`.
- Produces:

```go
var ErrShrunkCollection = errors.New("collection shrank beyond guard; nothing closed")
// Encerra vínculos e versões da fonte não vistos na coleta atual. MinRatio padrão 0.9.
func (s *Store) CloseUnseen(ctx context.Context, src sources.Source, cur collection.Collection, minRatio float64) (closed int64, err error)
```

Regras: age só sobre linhas da fonte com `closed_collection_id IS NULL` e `last_seen_collection_id < cur.ID`. Grava `closed_collection_id = cur.ID`, `valid_to_after = ` data de referência da coleta anterior concluída, `valid_to_before = cur.ReferenceDate`. Se a coleta atual é recorte, só considera linhas cuja empresa está no recorte. Se o total de linhas vistas na coleta atual for menor que `minRatio` × o total aberto antes dela (no mesmo alcance), devolve `ErrShrunkCollection` e não encerra nada. Sem coleta anterior concluída: não faz nada.

- [ ] **Step 1: Escrever os testes que falham**

```go
func TestPartnerMissingInNextCollectionIsClosedWithInterval(t *testing.T) {
	// c1 ref 2026-10-01 com sócio X; c2 ref 2026-11-01 sem X
	// → vínculo de X: closed_collection_id=c2, valid_to_after=2026-10-01, valid_to_before=2026-11-01; linha ainda existe
}
func TestPartnerStillPresentStaysOpen(t *testing.T)
func TestShrunkCollectionClosesNothing(t *testing.T) { // Review Focus 1
	// c2 com 50% das linhas de c1 → errors.Is(err, ErrShrunkCollection); 0 linhas encerradas
}
func TestSliceCollectionDoesNotCloseOutsideSlice(t *testing.T)
func TestFirstCollectionClosesNothing(t *testing.T)
func TestReappearingPartnerGetsNewOpenRow(t *testing.T) // saiu em c2, voltou em c3 → 2 linhas, a antiga segue encerrada
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 3: Implementar `CloseUnseen` e ligá-lo ao executor; `ErrShrunkCollection` faz a coleta terminar como `failed`**
- [ ] **Step 4: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: close unseen claims between snapshots with shrink guard"`

### Task 9: Consultas ao mapa

**Files:**
- Create: `internal/graph/graph.go`, `neighbors.go`, `paths.go`, `control.go`, `graph_test.go`, `internal/graph/fixture_test.go`

**Interfaces:**
- Consumes: tabelas da Tarefa 3; `PATHS_STRATEGY` de `docs/spikes/2026-11-receita-path-queries.md`; códigos de natureza jurídica de fundos de `docs/sources/receita.md`.
- Produces:

```go
type EntityRef struct{ ID int64; Kind claims.EntityKind; Name, NaturalKey string }
type Edge struct {
	RelationshipID int64; Type claims.RelType; Src, Dst EntityRef; Attrs map[string]any
	Kind claims.ClaimKind; Confidence *float64
	IdentityConfidence *float64 // menor confiança de identidade das fichas de pessoa envolvidas; nil se não há pessoa
	ValidFrom, ValidToAfter, ValidToBefore *time.Time
	SourceKey string; CollectionID int64; Stale bool
}
type Options struct{ RelTypes []claims.RelType; At *time.Time; IncludeClosed bool }
type PathOptions struct{ Options; MaxDepth, HubDegree, MaxPaths int } // padrões 6, 500, 20; MaxDepth máximo 6
type Hop struct{ Edge Edge; Forward bool }
type PathsResult struct{ Paths [][]Hop; StoppedHubs []HubStop; Truncated bool }
type HubStop struct{ Entity EntityRef; Degree int }
type ControlMode string // "documented" | "corporate_links"
type ControlOptions struct{ Mode ControlMode; MaxPartners int } // padrão 50
type Member struct{ Entity EntityRef; Depth int; Via []int64 }
type Stop struct{ Entity EntityRef; Reason string } // fund | foreign | too_many_partners
type ControlResult struct{ Root EntityRef; Controllers []EntityRef; Members []Member; Edges []Edge; Stops []Stop }

func New(p *pgxpool.Pool) *Graph
func (g *Graph) Neighbors(ctx context.Context, id int64, o Options, cursor string, limit int) (edges []Edge, next string, err error)
func (g *Graph) Paths(ctx context.Context, a, b int64, o PathOptions, progress func(float64)) (PathsResult, error)
func (g *Graph) ControlGroup(ctx context.Context, companyID int64, o ControlOptions, progress func(float64)) (ControlResult, error)
```

Regras:
- `Stale` = a fonte do vínculo está vencida (última coleta concluída mais antiga que `StaleAfter`).
- Por padrão só vínculos abertos; `At` filtra por validade na data; `IncludeClosed` inclui encerrados.
- `Paths`: caminhos mais curtos, tratando vínculos como não direcionados, pela estratégia registrada no relatório do teste. Um nó intermediário com grau maior que `HubDegree` não é expandido e entra em `StoppedHubs`; as pontas `a` e `b` são sempre expandidas.
- `ControlGroup` modo `documented`: segue só `controls`, `shareholder_of` com `attrs.percent > 50`, e `partner_of` quando a empresa tem exatamente um sócio aberto.
- `ControlGroup` modo `corporate_links`: segue todo `partner_of` e `shareholder_of`. `administrator_of` entra em `Edges` mas não propaga. Para, registrando em `Stops`, em: empresa com natureza jurídica de fundo (`fund`), entidade `foreign` (`foreign`), empresa com mais de `MaxPartners` sócios abertos (`too_many_partners`).
- Em ambos: sobe até não haver mais controlador, depois desce de cada controlador final. As paradas são avaliadas na subida e na descida.

- [ ] **Step 1: Escrever o grafo de teste (`fixture_test.go`)**

`func seed(t *testing.T, p *pgxpool.Pool) ids` monta: pessoa P1 sócia única de H (holding); H sócia de A e B; A sócia de C; pessoa P2 sócia de C e doadora (`donated_to`) do político POL; POL `candidate_for` partido PT1, que tem 600 outros candidatos (nó gigante); fundo F sócio de A; estrangeira X sócia de A; empresa W com 60 sócios pessoa, sendo sócia de A; um vínculo encerrado P3 → A.

- [ ] **Step 2: Escrever os testes que falham**

```go
func TestNeighborsReturnsOpenEdgesWithProvenance(t *testing.T) // vizinhos de A: H, C, W, F, X; sem P3; todo Edge com CollectionID != 0
func TestNeighborsIncludeClosedShowsInterval(t *testing.T)     // com IncludeClosed: P3 aparece com ValidToBefore != nil
func TestNeighborsAtDateFiltersByValidity(t *testing.T)
func TestPathsFindsPoliticianToCompany(t *testing.T) {
	// Paths(POL, H): 1 caminho de 4 saltos POL←P2→C←A←H; cada Hop com Edge.Kind preenchido
}
func TestPathsDoesNotExpandHubAndReportsIt(t *testing.T) { // Review Focus 2
	// Paths(POL, outroCandidatoDoPT1): Paths vazio; StoppedHubs contém PT1 com Degree 601
}
func TestPathsRespectsMaxDepth(t *testing.T)                   // MaxDepth 2 → POL–H não encontrado
func TestPathsCarriesIdentityConfidence(t *testing.T)          // P2 com identity_confidence 0.99 → Hop.Edge.IdentityConfidence == 0.99
func TestControlDocumentedFollowsOnlySupportedControl(t *testing.T) {
	// ControlGroup(H, documented): Controllers == [P1]; Members não inclui A, B, C (sem percentual, vários sócios)
}
func TestControlCorporateLinksStopsAtFundForeignAndCrowd(t *testing.T) {
	// ControlGroup(C, corporate_links): Controllers contém P1 e P2; Members contém H, A, B, C;
	// Stops == {F: fund, X: foreign, W: too_many_partners}
}
func TestControlAdministratorDoesNotPropagate(t *testing.T)
```

- [ ] **Step 3: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 4: Implementar `Neighbors`, `Paths` e `ControlGroup`**
- [ ] **Step 5: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 6: Commit** — `git add -A && git commit -m "feat: neighbors, paths and control group queries"`

---

## Fase 3 — API

### Task 10: Tarefas demoradas

**Files:**
- Create: `internal/db/migrations/0004_jobs.sql`, `internal/jobs/jobs.go`, `internal/jobs/jobs_test.go`
- Modify: `cmd/geoint/main.go` (o `worker` executa tarefas)

**Interfaces:**
- Consumes: `queue.Client`, `graph.Paths`, `graph.ControlGroup`.
- Produces:

```go
type Job struct {
	ID uuid.UUID; Type string; Params json.RawMessage; Status string // queued | running | done | failed
	Progress float64; Result json.RawMessage; Error, CreatedBy string; CreatedAt time.Time; FinishedAt *time.Time
}
type Handler func(ctx context.Context, params json.RawMessage, progress func(float64)) (any, error)
var ErrNotFound = errors.New("job not found")
func New(p *pgxpool.Pool, q *queue.Client) *Service
func (s *Service) Register(typ string, h Handler)
func (s *Service) Enqueue(ctx context.Context, typ string, params any, user string) (uuid.UUID, error)
func (s *Service) Get(ctx context.Context, id uuid.UUID, user string, admin bool) (Job, error)
func (s *Service) RunWorker(ctx context.Context) error
```

Tipos registrados no `worker`: `paths` com params `{"a":int,"b":int,"max_depth":int,"rel_types":[...],"at":"AAAA-MM-DD"}` e `control_group` com `{"company_id":int,"mode":"documented"|"corporate_links"}`. Tempo limite por tarefa: 5 minutos, depois `failed` com erro `"timeout"`.

- [ ] **Step 1: Escrever os testes que falham**

```go
func TestEnqueueRunAndGetResult(t *testing.T)          // handler devolve {"n":1}; Get → Status "done", Result {"n":1}, Progress 1
func TestHandlerErrorMarksFailed(t *testing.T)         // Status "failed", Error com a mensagem
func TestGetByAnotherUserIsNotFound(t *testing.T)      // errors.Is(err, ErrNotFound); admin enxerga
func TestUnknownTypeIsRejectedAtEnqueue(t *testing.T)
func TestTimeoutMarksFailed(t *testing.T)              // limite reduzido no teste; Error == "timeout"
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 3: Implementar** (publica em `jobs.run.<tipo>`; o progresso é gravado na linha no máximo 1 vez por segundo).
- [ ] **Step 4: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: background jobs for long graph queries"`

### Task 11: Login separado, papéis e auditoria

**Files:**
- Create: `internal/auth/auth.go`, `internal/auth/auth_test.go`, `internal/audit/audit.go`, `internal/audit/audit_test.go`, `internal/db/migrations/0005_audit.sql`
- Create: `deploy/keycloak/realm-geoint.json`
- Modify: `deploy/compose.yml` (Keycloak importa o realm na subida)

**Interfaces:**
- Produces:

```go
// internal/auth
type User struct{ Sub, Name string; Roles []string }
type Verifier interface{ Verify(ctx context.Context, rawToken string) (User, error) }
func NewOIDCVerifier(ctx context.Context, issuer, audience string) (Verifier, error)
func Middleware(v Verifier) func(http.Handler) http.Handler // 401 sem token ou token inválido
func RequireRole(role string) func(http.Handler) http.Handler // 403
func FromContext(ctx context.Context) (User, bool)
func (u User) Has(role string) bool

// internal/audit
func Log(ctx context.Context, p *pgxpool.Pool, user, action string, entityID *int64, detail map[string]any) error
```

Realm `geoint`; cliente público `geoint-web` com PKCE e redirecionamento `http://localhost:5173/*`; papéis de realm `analyst` e `admin` mapeados na claim `roles`; audiência `geoint-api`. O arquivo do realm não contém usuários nem senhas: o README explica como criar o primeiro usuário pelo console do Keycloak.

- [ ] **Step 1: Escrever os testes que falham**

```go
func TestMissingTokenIs401(t *testing.T)
func TestInvalidTokenIs401NotServerError(t *testing.T) // Verifier devolve erro → 401, corpo {"error":"unauthorized"}
func TestValidTokenPutsUserInContext(t *testing.T)
func TestRequireRoleForbidsOtherRoles(t *testing.T)    // usuário só analyst em rota admin → 403
func TestExpiredTokenIs401(t *testing.T)               // token assinado por chave de teste com exp no passado
func TestWrongAudienceIs401(t *testing.T)
func TestAuditLogWritesRow(t *testing.T)               // 1 linha com user, action, entity_id, at
```

Os testes de expiração e audiência usam um emissor OIDC falso em `httptest` com chave RSA gerada no teste.

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 3: Implementar com `go-oidc`; `audit_log` também recebe o gatilho `forbid_delete()`**
- [ ] **Step 4: Rodar e ver passar** — Run: `make test` → PASS. Run: `make up`, abrir `http://localhost:8081/realms/geoint/.well-known/openid-configuration` → JSON com `issuer`.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: OIDC auth, roles and audit log"`

### Task 12: Rotas da API

**Files:**
- Create: `internal/api/router.go`, `search.go`, `entities.go`, `jobs.go`, `identity.go`, `sources.go`, `json.go`, `api_test.go`
- Create: `internal/freshness/freshness.go`, `internal/freshness/freshness_test.go`
- Create: `docs/api.md`
- Modify: `internal/api/health.go`, `cmd/geoint/main.go`

**Interfaces:**
- Consumes: `graph.*`, `jobs.Service`, `identity.Resolver`, `auth.*`, `audit.Log`, `queue.Client`, `rawstore.Store`.
- Produces:

```go
// internal/freshness
type Status string // fresh | stale | never | failed
type SourceStatus struct {
	Key, Name string; Status Status; LastCollectedAt, NextExpectedAt *time.Time
	LastReferenceDate *time.Time; LastError string; LastStats map[string]int64
}
func New(p *pgxpool.Pool, now func() time.Time) *Service
func (s *Service) List(ctx context.Context) ([]SourceStatus, error)

// internal/api
type Deps struct{ Pool *pgxpool.Pool; Graph *graph.Graph; Jobs *jobs.Service; Identity *identity.Resolver
	Fresh *freshness.Service; Queue *queue.Client; Verifier auth.Verifier }
```

`failed` = a coleta mais recente falhou. `stale` = última concluída mais antiga que `StaleAfter`. `never` = nenhuma concluída.

Rotas (todas sob `auth.Middleware`, exceto `/healthz`; JSON; erros como `{"error":"<código>"}`):

| Rota | Papel | Resposta |
|---|---|---|
| `GET /api/v1/search?q=&kind=&limit=` | analyst | `{"items":[{"id","kind","name","natural_key"}]}`; `limit` padrão 20, máx. 50 |
| `GET /api/v1/entities/{id}` | analyst | `{"entity":EntityRef,"versions":[...],"records":[{"id","source_key","context_key","identity_confidence"}],"possible_same":[Candidate]}` |
| `GET /api/v1/entities/{id}/relationships?state=open\|closed\|all&type=&at=&cursor=&limit=` | analyst | `{"items":[Edge],"next_cursor"}`; `limit` padrão e máx. 200 |
| `GET /api/v1/relationships/{id}/provenance` | analyst | `{"source":{"key","name"},"collection":{"id","reference_date","collected_at","stale"},"record_ref","raw_key","raw_sha256"}` |
| `POST /api/v1/jobs` `{"type","params"}` | analyst | `202 {"id"}` |
| `GET /api/v1/jobs/{id}` | dono ou admin | `Job` |
| `GET /api/v1/identity/proposals?status=&cursor=` | analyst | `{"items":[{"id","name","records":[...],"connections","reason"}],"next_cursor"}` |
| `POST /api/v1/identity/proposals/{id}/decision` `{"confirm":bool}` | analyst | `204` |
| `POST /api/v1/identity/merge` · `/split` `{"record_ids":[...],"reason"}` | analyst | `{"entity_id"}` |
| `GET /api/v1/sources` | analyst | `[SourceStatus]` |
| `POST /api/v1/sources/{key}/collect` `{"cnpjs":[...],"depth":n}` | admin | `202` |

Busca: se `q`, sem pontuação, tem 8 ou 14 dígitos → busca exata por `cnpj_basico`; se tem só dígitos e menos de 8 → prefixo; caso contrário `NormalizeName(q)` com similaridade trigram, ordenado por similaridade. `q` com menos de 3 caracteres úteis → `400 {"error":"query_too_short"}`. Toda leitura de entidade, vínculo, caminho ou grupo grava `audit.Log`. `docs/api.md` documenta as rotas acima.

- [ ] **Step 1: Escrever os testes que falham (com `Verifier` falso e o grafo de teste da Tarefa 9)**

```go
func TestEveryRouteExceptHealthRequiresToken(t *testing.T) // percorre a tabela de rotas sem token → 401 em todas
func TestSearchNormalizesInput(t *testing.T) { // Review Focus 5
	// empresa "AÇÚCAR MODELO LTDA", cnpj_basico 12345678; cada q abaixo devolve essa entidade em items[0]:
	// "açúcar modelo", "ACUCAR MODELO", "12.345.678/0001-90", "12345678000190", "12345678"
}
func TestSearchTooShortIs400(t *testing.T)
func TestEntityRelationshipsPaginates(t *testing.T)        // 450 vínculos → 200, 200, 50; next_cursor vazio no fim
func TestProvenanceReturnsSourceAndCollection(t *testing.T) // record_ref e reference_date iguais aos gravados
func TestProvenanceMarksStaleCollection(t *testing.T)      // relógio 60 dias após a coleta da Receita → stale true
func TestJobRoundTripPaths(t *testing.T)                   // POST paths → 202; GET até done; result.Paths tem 1 caminho
func TestJobOfAnotherUserIs404(t *testing.T)
func TestCollectRequiresAdmin(t *testing.T)                // analyst → 403; admin → 202 e mensagem em ingest.request.receita
func TestProposalDecisionAppliesMerge(t *testing.T)
func TestEntityReadIsAudited(t *testing.T)                 // 1 linha em audit_log com action "entity.read"
func TestFreshnessStatuses(t *testing.T)                   // never, fresh, stale e failed conforme coletas e relógio
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 3: Implementar `freshness` e as rotas** (limite de corpo de 1 MB; tempo limite de 10 s por requisição; CORS só para `WEB_ORIGIN`).
- [ ] **Step 4: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: HTTP API for search, map, jobs, identity and sources"`

### Task 13: Agendador de coletas

**Files:**
- Create: `internal/freshness/scheduler.go`, `internal/freshness/scheduler_test.go`
- Modify: `cmd/geoint/main.go` (o `worker` roda o agendador a cada hora)

**Interfaces:**
- Consumes: `freshness.Service`, `queue.Client`, `sources.All`.
- Produces: `func (s *Service) Tick(ctx context.Context, q *queue.Client) (requested []string, err error)`

Pede coleta completa de uma base quando: nunca foi coletada, ou a última concluída é mais antiga que `PublishEvery`, e não há coleta `running` nem pedido feito nas últimas 24 h. Base cuja última coleta falhou é pedida de novo no máximo 1 vez por dia. Desligável com `SCHEDULER_ENABLED=false` (padrão `true`).

- [ ] **Step 1: Escrever os testes que falham**

```go
func TestTickRequestsNeverCollectedSources(t *testing.T) // banco vazio → requested == todas as 5 chaves
func TestTickSkipsFreshSource(t *testing.T)
func TestTickSkipsWhileRunning(t *testing.T)
func TestTickDoesNotRequestTwiceWithin24h(t *testing.T)
func TestTickRetriesFailedOncePerDay(t *testing.T)
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 3: Implementar `Tick` e o laço no `worker`**
- [ ] **Step 4: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: collection scheduler"`

---

## Fase 4 — Demais bases

Cada conector segue o molde da Tarefa 7: confirmar o formato e registrar em `docs/sources/<fonte>.md`, montar recorte real em `testdata/<fonte>/` com linhas malformadas acrescentadas, testes, implementação. Todo conector precisa passar nestes três testes, com o mesmo nome: `TestMalformedRowsAreSkippedAndCounted`, `TestLoadTwiceIsIdempotent`, `TestEveryClaimHasProvenance`.

### Task 14: Conector do TSE

**Files:**
- Create: `internal/connector/tse/tse.go`, `fetch.go`, `load.go`, `load_test.go`, `testdata/tse/*`, `docs/sources/tse.md`

**Interfaces:**
- Produces: `func New(baseURL string, years []int) connector.Connector`, `Key() == "tse"`, anos padrão `2018, 2020, 2022, 2024, 2026`, todos os cargos. Grava: entidades `party` (`natural_key` = sigla); fichas de candidato (`context_key` = `"cand:<ano>:<SQ_CANDIDATO>"`); `candidate_for` (pessoa → partido; `attrs`: `year, office, uf, city, result`); doadores PF como fichas (`context_key` = `"donor:<ano>:<SQ_CANDIDATO>"`), doadores PJ como entidade `company` por `CNPJBase`; `donated_to` (doador → candidato; `attrs`: `year, amount, date, origin`; `valid_from` = data da receita). Doações do mesmo doador ao mesmo candidato no mesmo ano são linhas distintas por `record_ref`.

- [ ] **Step 1: Confirmar formato e registrar em `docs/sources/tse.md`**

Esperado, a confirmar por ano: `consulta_cand_<ano>.zip` e `prestacao_de_contas_eleitorais_candidatos_<ano>.zip`, CSV com cabeçalho, `;`, Latin-1, aspas. Registrar, ano a ano, como CPF de candidato e CPF/CNPJ de doador são publicados (completo, mascarado ou ausente) e o nome exato das colunas usadas.

- [ ] **Step 2: Montar `testdata/tse/` com um ano de CPF completo e um ano de CPF mascarado ou ausente**
- [ ] **Step 3: Escrever os testes que falham**

```go
func TestCandidateLinkedToParty(t *testing.T)               // candidate_for com attrs.year, office, uf do arquivo
func TestPersonDonorCreatesRecordAndDonation(t *testing.T)  // attrs.amount == valor do arquivo; valid_from == data
func TestCompanyDonorLinksToCompanyEntity(t *testing.T)     // src é entidade company de cnpj_basico conhecido
func TestMaskedOrMissingCPFYieldsEmptyFragment(t *testing.T) // cpf_fragment vazio; ficha criada; nada juntado
func TestSameCandidateAcrossYearsMergesOnlyWithFragment(t *testing.T) // após Resolve: juntam se há fragmento igual; senão ficam separados
func TestRepeatedDonationsAreSeparateRows(t *testing.T)
// + os três testes obrigatórios
```

- [ ] **Step 4: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 5: Implementar** (cabeçalho lido por nome de coluna, não por posição, com mapa de nomes por ano vindo de `docs/sources/tse.md`).
- [ ] **Step 6: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 7: Commit** — `git add -A && git commit -m "feat: TSE connector"`

### Task 15: Conectores da Câmara e do Senado

**Files:**
- Create: `internal/connector/camara/{camara.go,load_test.go}`, `internal/connector/senado/{senado.go,load_test.go}`, `testdata/camara/*`, `testdata/senado/*`, `docs/sources/camara.md`, `docs/sources/senado.md`

**Interfaces:**
- Produces: `camara.New(baseURL string, legislatures []int)` e `senado.New(baseURL string, legislatures []int)`; padrão: legislatura atual e as duas anteriores (55, 56, 57). Gravam: entidades `public_body` (`natural_key` `"camara"` e `"senado"`), `party`, fichas de parlamentar (`context_key` = `"dep:<id>"` / `"sen:<código>"`, com CPF se a base publicar), `holds_mandate` (pessoa → casa; `attrs`: `legislature, uf, party`; `valid_from`/fim conforme a base). Mandato encerrado na fonte grava `valid_to_after == valid_to_before ==` data de fim.

- [ ] **Step 1: Confirmar as APIs e registrar nos dois `docs/sources/*.md`** (rotas, paginação, limite de requisições, campos de CPF e de início e fim de mandato). As respostas brutas da API são salvas em `rawstore` como vieram.
- [ ] **Step 2: Gravar respostas reais de uma legislatura em `testdata/` e servir por `httptest` nos testes**
- [ ] **Step 3: Escrever os testes que falham**

```go
func TestDeputyMandateLinkedToChamber(t *testing.T)   // holds_mandate com attrs.legislature, uf, party
func TestEndedMandateHasExactEndDate(t *testing.T)
func TestPartyChangeMidTermCreatesTwoRows(t *testing.T) // troca de partido: linha antiga encerrada na data, nova aberta
func TestPaginationFollowsAllPages(t *testing.T)
func TestRateLimitResponseIsRetried(t *testing.T)     // 429 e depois 200 → sucesso, 2 chamadas
// + os três testes obrigatórios, em cada pacote
```

- [ ] **Step 4: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 5: Implementar os dois conectores**
- [ ] **Step 6: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 7: Commit** — `git add -A && git commit -m "feat: Câmara and Senado connectors"`

### Task 16: Conector da CVM

**Files:**
- Create: `internal/connector/cvm/{cvm.go,load.go,load_test.go}`, `testdata/cvm/*`, `docs/sources/cvm.md`

**Interfaces:**
- Produces: `cvm.New(baseURL string)`, `Key() == "cvm"`. Grava: empresas abertas ligadas à entidade `company` por `CNPJBase` (cria se não existir), `shareholder_of` (acionista → companhia; `attrs.percent` = participação no capital total, `attrs.role` = `"shareholder"`), e `controls` quando a fonte declara o acionista como controlador. Acionista PJ por CNPJ; PF como ficha (`context_key` = `"cvm:<cnpj_basico>"`); estrangeiro como `foreign`.

- [ ] **Step 1: Confirmar os conjuntos de dados e registrar em `docs/sources/cvm.md`** (cadastro de companhias abertas e posição acionária do Formulário de Referência: nomes dos arquivos, colunas de percentual e de controlador, codificação).
- [ ] **Step 2: Montar `testdata/cvm/` com uma companhia de controlador declarado e uma sem controlador**
- [ ] **Step 3: Escrever os testes que falham**

```go
func TestShareholderPercentIsStored(t *testing.T)            // attrs.percent == valor do arquivo
func TestDeclaredControllerCreatesControlsEdge(t *testing.T)
func TestCompanyMatchesReceitaEntityByCNPJ(t *testing.T)     // empresa já carregada pela Receita → mesma entity.id
func TestDocumentedControlUsesCVMPercent(t *testing.T) {
	// graph.ControlGroup(companhia, documented) → Controllers contém o acionista com percent > 50
}
// + os três testes obrigatórios
```

- [ ] **Step 4: Rodar e ver falhar** — Run: `make test` → FAIL.
- [ ] **Step 5: Implementar**
- [ ] **Step 6: Rodar e ver passar** — Run: `make test` → PASS.
- [ ] **Step 7: Commit** — `git add -A && git commit -m "feat: CVM connector"`

---

## Fase 5 — Bancada

Textos da interface em português. Vínculo de fato e de inferência têm aparência diferente em toda tela: fato com linha contínua e etiqueta "fato"; inferência com linha tracejada e etiqueta "inferência · NN%". Vínculo que depende de identidade inferida mostra "identidade NN%". Dado de base vencida mostra a etiqueta "vencido".

### Task 17: Aplicação web, login, busca e página da entidade

**Files:**
- Create: `web/package.json`, `vite.config.ts`, `tsconfig.json`, `index.html`, `deploy/Dockerfile.web`
- Create: `web/src/main.tsx`, `App.tsx`, `auth.tsx`, `api/client.ts`, `api/types.ts`
- Create: `web/src/pages/SearchPage.tsx`, `EntityPage.tsx`
- Create: `web/src/components/EdgeBadge.tsx`, `ProvenancePopover.tsx`, `Timeline.tsx`, `StaleTag.tsx`
- Create: testes `*.test.tsx` ao lado de cada componente e página
- Modify: `Makefile` (`test-web`), `deploy/compose.yml` (serviço `web`), `.github/workflows/ci.yml` (`make test-web` e `npm audit --audit-level=high`)

**Interfaces:**
- Consumes: rotas da Tarefa 12 (`docs/api.md`).
- Produces: `api/types.ts` com `EntityRef`, `Edge`, `Provenance`, `Job`, `SourceStatus`, `Proposal` espelhando o JSON da API. `api/client.ts`: `apiGet<T>(path)`, `apiPost<T>(path, body)` que anexam o token e, em 401, refazem o login. Rotas: `/` busca, `/entidade/:id`. Componentes reutilizados nas Tarefas 18 e 19: `<EdgeBadge edge />`, `<ProvenancePopover relationshipId />`, `<StaleTag />`.

- [ ] **Step 1: Escrever os testes que falham (Vitest + Testing Library, API simulada)**

```ts
test('EdgeBadge mostra "fato" para fato', ...)                       // texto "fato", sem porcentagem
test('EdgeBadge mostra "inferência · 80%" para inferência 0.8', ...)
test('EdgeBadge mostra "identidade 99%" quando identity_confidence = 0.99', ...)
test('EdgeBadge mostra "vencido" quando stale', ...)
test('busca com menos de 3 caracteres não chama a API e mostra aviso', ...)
test('busca lista resultados com tipo e CNPJ formatado', ...)        // 12345678 → "12.345.678"
test('página da entidade separa vínculos atuais de encerrados', ...) // encerrado mostra "saiu entre 01/10/2026 e 01/11/2026"
test('clicar na fonte abre base, registro, data de coleta', ...)     // ProvenancePopover chama /relationships/{id}/provenance
test('página da entidade lista "possível mesma pessoa" sem juntar', ...)
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test-web` → FAIL.
- [ ] **Step 3: Implementar** (`react-oidc-context` com o cliente `geoint-web`; token só em memória, nunca em `localStorage`; TanStack Query para as chamadas).
- [ ] **Step 4: Rodar e ver passar** — Run: `make test-web` → PASS. Run: `make up`, entrar em `http://localhost:5173`, autenticar e buscar uma empresa do recorte.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: web app with login, search and entity page"`

### Task 18: Explorador do mapa, caminhos e grupo de controle

**Files:**
- Create: `web/src/pages/MapPage.tsx`, `PathsPage.tsx`, `ControlGroupPage.tsx`
- Create: `web/src/components/GraphView.tsx`, `JobProgress.tsx`, `EntityPicker.tsx`, `graphStyle.ts` e testes ao lado

**Interfaces:**
- Consumes: `EdgeBadge`, `ProvenancePopover`, `apiGet`, `apiPost`, rotas de vizinhos e de tarefas.
- Produces: rotas `/mapa/:id`, `/caminhos`, `/grupo/:id`. `useJob(id)` consulta `GET /jobs/{id}` a cada 2 s até `done` ou `failed`. `toElements(edges: Edge[])` converte vínculos em elementos do Cytoscape.

- [ ] **Step 1: Escrever os testes que falham**

```ts
test('toElements cria um nó por entidade e uma aresta por vínculo, sem duplicar nós', ...)
test('toElements marca aresta de inferência com classe "inference" e de fato com "fact"', ...)
test('expandir um nó busca vizinhos e acrescenta ao mapa sem apagar o que havia', ...)
test('filtro por tipo de vínculo e por data refaz a consulta com type e at', ...)
test('caminhos: envia a tarefa, mostra progresso e lista cada elo com EdgeBadge', ...)
test('caminhos: mostra "parou em PT1 (601 vínculos)" quando há stopped_hubs', ...)
test('caminhos: tarefa failed mostra o erro e botão de tentar de novo', ...)
test('grupo: alterna entre "Controle documentado" e "Vínculos societários" e reenvia a tarefa', ...)
test('grupo: vínculo sem percentual mostra "sócio, participação não informada"', ...)
test('grupo: lista paradas com motivo em português', ...) // fund → "fundo de investimento", foreign → "sócio estrangeiro", too_many_partners → "muitos sócios"
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test-web` → FAIL.
- [ ] **Step 3: Implementar** (Cytoscape com layout `cose`; expansão em páginas de 200; aviso quando um nó tem mais vizinhos do que os exibidos).
- [ ] **Step 4: Rodar e ver passar** — Run: `make test-web` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: map explorer, paths and control group screens"`

### Task 19: Fila de identidades e situação das bases

**Files:**
- Create: `web/src/pages/IdentityQueuePage.tsx`, `SourcesPage.tsx` e testes ao lado
- Modify: `web/src/App.tsx` (menu), `web/src/pages/EntityPage.tsx` (ações de juntar e separar)

**Interfaces:**
- Consumes: rotas `/identity/*` e `/sources`.
- Produces: rotas `/identidades` e `/bases`.

- [ ] **Step 1: Escrever os testes que falham**

```ts
test('fila mostra nome, fichas com base e contexto, e número de vínculos', ...)
test('confirmar envia {confirm:true} e remove o item da lista', ...)
test('rejeitar envia {confirm:false} e remove o item da lista', ...)
test('separar ficha na página da entidade exige motivo antes de enviar', ...)
test('bases: mostra "em dia", "vencida", "nunca coletada" e "falhou" conforme status', ...)
test('bases: falha mostra a mensagem de erro da última coleta', ...)
test('bases: botão "Coletar agora" só aparece para admin', ...)
```

- [ ] **Step 2: Rodar e ver falhar** — Run: `make test-web` → FAIL.
- [ ] **Step 3: Implementar**
- [ ] **Step 4: Rodar e ver passar** — Run: `make test-web` → PASS.
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat: identity queue and source status screens"`

---

## Fase 6 — Entrega

### Task 20: Teste de ponta a ponta, carga nacional e verificação das metas

**Files:**
- Create: `web/e2e/workbench.spec.ts`, `web/playwright.config.ts`, `deploy/compose.e2e.yml`, `scripts/e2e-seed.sh`
- Create: `scripts/measure.sh`, `docs/performance/2026-national-load.md`
- Modify: `README.md`, `Makefile` (`e2e`, `measure`)

**Interfaces:**
- Consumes: tudo.
- Produces: `make e2e`; `make measure`; README com instalação, primeiro usuário no Keycloak, comandos de coleta, requisitos de máquina.

- [ ] **Step 1: Escrever o teste de ponta a ponta que falha**

`make e2e` sobe a pilha com `compose.e2e.yml` (realm de teste com usuário `analista`/senha de teste e `SCHEDULER_ENABLED=false`), roda `geoint migrate`, carrega os recortes de `testdata/` por `geoint collect` com `baseURL` apontando para um servidor de arquivos local, e executa o Playwright:

```ts
test('analista busca empresa, navega, acha caminho e vê a fonte', async ({ page }) => {
  // login → buscar CNPJ do recorte → página da entidade lista sócios
  // → abrir fonte de um vínculo: mostra "Receita Federal" e data de coleta
  // → /caminhos entre o político e a empresa do recorte: ao menos 1 caminho listado
  // → /grupo/:id em "Vínculos societários": controlador final esperado aparece
  // → /bases: Receita "em dia"
});
test('sem login, qualquer rota redireciona para o Keycloak', ...)
```

- [ ] **Step 2: Rodar e ver falhar; corrigir o que o teste revelar; rodar até passar** — Run: `make e2e` → PASS.

- [ ] **Step 3: Carga nacional**

Run: `make up && geoint migrate`, depois `geoint collect receita`, `tse`, `camara`, `senado`, `cvm` (a Receita reaproveita `data/raw/receita/` da Tarefa 1). Acompanhar em `/bases`. Esperado: as cinco bases "em dia"; Receita em ≤ 12 h.

- [ ] **Step 4: Medir as metas com `scripts/measure.sh` e registrar em `docs/performance/2026-national-load.md`**

Mesmas medidas M1 a M4 da Tarefa 1, agora pela API autenticada, mais: página de entidade p95 ≤ 1 s; tamanho do banco; tamanho da fila de identidades; contagem de entidades, fichas e vínculos por base; linhas puladas por base. Se alguma meta falhar, não declarar a etapa concluída: registrar os números e devolver ao Rafael.

- [ ] **Step 5: Conferir os critérios de sucesso do desenho (seção 2), um a um, na base nacional, e anotar o resultado de cada um no mesmo documento**

- [ ] **Step 6: README e commit**

```bash
git add -A && git commit -m "test: end-to-end flow, national load results and usage docs"
```
