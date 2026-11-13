# API HTTP

A API do `geoint api` serve o front-end e quem mais tiver um token válido. Tudo é
JSON em `snake_case`. Todos os exemplos abaixo usam dados inventados.

## Autenticação e papéis

- Toda rota sob `/api/v1` exige `Authorization: Bearer <access token>` emitido pelo
  Keycloak do realm `geoint` (emissor `OIDC_ISSUER`, audiência `OIDC_AUDIENCE`). O
  token nunca é aceito na query string nem em cookie.
- Só `GET /healthz` é público.
- Papéis (comparação exata): todas as rotas exigem `analyst`; `POST
  /api/v1/sources/{key}/collect` exige `admin`. No realm, `admin` é um papel
  composto que contém `analyst`, então o token de um administrador traz os dois.
  Um token só com `admin` (sem `analyst`) recebe 403 nas rotas de analista.
- `GET /api/v1/jobs/{id}`: só o dono do job, ou um `admin`. Job de outra pessoa
  responde 404, igual a um job inexistente.
- Se o Keycloak estiver fora do ar, a API sobe mesmo assim e responde `503
  {"error":"auth_unavailable"}` (com `Retry-After`) até conseguir falar com ele.
- Só o formato compacto de JWS (`header.payload.assinatura`) é aceito, com uma
  assinatura RS256 ou ES256; o `kid` vem do cabeçalho protegido.
- Chaves de assinatura: buscadas no JWKS do Keycloak no primeiro uso, de novo
  quando chega um `kid` desconhecido (no máximo uma busca a cada 30 s; a cada
  3 s enquanto nenhuma chave foi carregada) e, em segundo plano, quando as
  chaves em cache passam de 15 min — assim uma chave removida do Keycloak deixa
  de valer. Se a busca falha, as chaves em cache continuam valendo; chaves que
  nenhuma busca confirmou por 24 h deixam de valer (503 até uma busca dar certo).
- Um token com `kid` ainda desconhecido, recusado só porque a última busca de
  chaves foi há menos de 30 s, recebe `503 auth_unavailable` com `Retry-After`
  (não 401): pode ser uma chave recém-rotacionada, e o cliente não deve
  desconectar o usuário. Tokens de chaves já em cache são verificados sem
  esperar por isso.

## Comportamento comum

- `Host`: a API só responde a requisições cujo cabeçalho `Host` nomeia
  `localhost`, `127.0.0.1`, `[::1]` ou `api` (o nome do serviço no compose),
  com qualquer porta, ou um dos nomes da variável `API_ALLOWED_HOSTS` (lista
  separada por vírgulas de nomes de host ou IPs, sem esquema, porta nem
  curinga). Qualquer outro `Host` — `/healthz` e preflight incluídos — recebe
  `421 {"error":"misdirected_request"}`. Defesa contra *DNS rebinding*: a API
  escuta só no loopback, e uma página de outro site que apontasse um nome seu
  para 127.0.0.1 chegaria com esse nome no `Host`.
- Cabeçalhos em toda resposta: `Content-Type: application/json; charset=utf-8`,
  `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`.
- CORS: só a origem exata de `WEB_ORIGIN` (padrão `http://localhost:5173`), nunca
  `*`, sem credenciais; métodos `GET, POST, OPTIONS`; cabeçalhos `Authorization,
  Content-Type`. O preflight (`OPTIONS`) da origem permitida é respondido com
  204 antes da autenticação; de outra origem ele não é respondido como
  preflight (segue como requisição comum, 405 ou 401) e vem sem cabeçalhos CORS.
- Corpo de requisição: no máximo 1 MiB. JSON com campo desconhecido, dado depois
  do objeto ou tipo errado → 400.
- Tempo limite: 10 s por requisição (autenticação incluída) → 504.
- Datas sem hora (`valid_from`, `valid_to_after`, `valid_to_before`,
  `reference_date`, `observed_date`, `last_reference_date`,
  `last_observed_date`, `first_seen_reference_date`, ...) vêm como `AAAA-MM-DD`; instantes
  (`collected_at`, `created_at`, `finished_at`, `recorded_at`,
  `last_collected_at`, `next_expected_at`, `last_checked_at`) em RFC 3339, UTC.
- Nomes: todo nome de pessoa ou empresa que sai da API passa por mascaramento de
  CPF — qualquer sequência de exatamente 11 dígitos vira `***NNNNNN**` (os 6
  dígitos do meio, como a Receita publica). Fragmentos de CPF de registros
  aparecem só em `cpf_masked`, já nessa forma.
- Auditoria: cada leitura de entidade, vínculos, proveniência, busca, propostas de
  identidade e resultado de job grava uma linha em `audit_log` **antes** da
  resposta; se a gravação falhar, a requisição falha com 500 e nada é devolvido.
  Cada escrita (job, decisão, merge, split, coleta) grava a linha **antes** de
  executar a ação; se a gravação falhar, a resposta é 500 e a ação não é
  executada. O registro de auditoria de ações de escrita registra a tentativa:
  se a ação falhar depois (ou o cliente desistir), a linha fica. O detalhe
  guarda só ids, valores enumerados e contagens — nunca o texto buscado,
  nomes, CPFs, tokens ou justificativas.

### Erros

Sempre `{"error":"<código>"}`, sem texto interno, SQL ou pilha.

| Código | Status | Quando |
|---|---|---|
| `bad_request` | 400 | id, parâmetro, cursor ou corpo JSON malformado |
| `query_too_short` | 400 | busca com menos de 3 caracteres úteis |
| `unauthorized` | 401 | token ausente, malformado ou recusado |
| `forbidden` | 403 | papel insuficiente |
| `not_found` | 404 | entidade, vínculo, job, proposta, registro ou fonte inexistente; rota desconhecida |
| `method_not_allowed` | 405 | método errado numa rota existente |
| `misdirected_request` | 421 | cabeçalho `Host` fora da lista permitida (ver "Comportamento comum") |
| `conflict` | 409 | proposta já decidida, ou confirmação de uma com menos de dois registros ainda livres; os dados mudaram desde que a tela foi carregada (campos `expected_*`, `entity_ids`, `entity_id` das escritas de identidade); coleta de fonte sem conector; já existe uma coleta em andamento ou um pedido aguardando início |
| `body_too_large` | 413 | corpo acima de 1 MiB |
| `internal` | 500 | erro inesperado (registrado no log do servidor) |
| `auth_unavailable` | 503 | provedor de identidade fora do ar, chaves sem confirmação há 24 h, ou `kid` desconhecido enquanto a nova busca de chaves aguarda o intervalo (com `Retry-After`) |
| `timeout` | 504 | a requisição passou de 10 s |

## Fato e inferência

Todo vínculo (`edge`) diz de onde veio e quanto vale:

| Campo | Significado |
|---|---|
| `kind` | `fact` (lido de uma fonte) ou `inference` (derivado) |
| `confidence` | `null` para fatos; entre 0 e 1 para inferências |
| `identity_confidence` | presente quando o vínculo cita registros de pessoa: a menor confiança de que esses registros são a pessoa da entidade (junção automática: sempre < 1; decisão de analista: 1) |
| `identity_basis` | sempre presente: em que se apoia a identidade das pontas que são pessoas — `none` (nenhum registro de pessoa, ou cada pessoa envolvida tem um único registro), `auto` (alguma pessoa envolvida junta mais de um registro, unidos automaticamente por nome + trecho de CPF: inferência), `analyst` (alguma pessoa envolvida junta mais de um registro e ao menos um foi colocado por decisão de analista — junção ou confirmação). Com as duas situações, vale `analyst`. Ver abaixo |
| `closed` | sempre presente: `true` quando o vínculo foi fechado (a fonte deixou de listá-lo ou declarou o fim); não é preciso deduzir pelas datas |
| `source_key`, `collection_id` | fonte e coleta em que o vínculo foi lido pela primeira vez |
| `stale` | a fonte está desatualizada: a última coleta completa concluída é mais velha que o prazo da fonte (ou não há nenhuma). A idade conta da data de observação (`observed_date`, a publicação da edição) para fontes publicadas em edições (Receita) e da data de referência para as demais — a mesma regra de `GET /api/v1/sources` |
| `attrs` | detalhes do vínculo como a fonte os deu (tabela abaixo); só chaves conhecidas e valores simples |
| `valid_from`, `valid_to_after`, `valid_to_before` | período de validade; ver "Vínculos fechados" |

Exemplo de `edge`:

```json
{
  "relationship_id": 501,
  "type": "partner_of",
  "src": {"id": 11, "kind": "person", "name": "PESSOA FICTICIA UM", "natural_key": ""},
  "dst": {"id": 12, "kind": "company", "name": "EMPRESA FICTICIA A LTDA", "natural_key": "90000002"},
  "attrs": {"role": "22"},
  "kind": "fact",
  "confidence": null,
  "identity_confidence": 0.99,
  "valid_from": "2020-01-01",
  "valid_to_after": null,
  "valid_to_before": null,
  "source_key": "receita",
  "collection_id": 7,
  "stale": false,
  "closed": false,
  "identity_basis": "auto"
}
```

`identity_basis` e `identity_confidence` juntos: `identity_confidence: 1` com
`identity_basis: "analyst"` é uma junção feita por analista (decisão humana,
não documento); com `"none"` é uma pessoa de um único registro. Uma tela deve
mostrar a dependência sempre que `identity_basis` não for `none`. Resultados de
job gravados antes deste campo existir trazem `"unknown"` quando o vínculo cita
registro de pessoa (e `"none"` quando não cita); `closed` neles é deduzido das
datas. Esses resultados somem em até 30 dias (retenção dos jobs).

### Vínculos fechados

Regra de toda a plataforma: um vínculo vale no dia D quando `valid_from <= D <
valid_to_before`. `valid_to_before` é o primeiro dia em que o vínculo já não
vale; quando `valid_to_after` = `valid_to_before`, a fonte declarou o fim exato.
Quando são diferentes, o fim aconteceu em algum dia do intervalo (por exemplo,
entre duas coletas). `null` em ambos: vínculo aberto, ou fim desconhecido.

### `attrs` por tipo de vínculo

Só estas chaves saem da API; qualquer outra é descartada. Cada chave tem uma
espécie, e um valor que não tem a forma da sua espécie é descartado:

| Espécie | Chaves | Forma aceita | Máscara de CPF |
|---|---|---|---|
| texto livre | `ballot_name`, `office`, `city_name`, `origin`, `result`, `party` | qualquer texto | **sim** |
| identificador / código | `receipt_id`, `origin_code`, `office_code`, `city`, `described_by` | letras ASCII, dígitos e `:._#/-`, 1 a 64 caracteres | nunca |
| papel (`role`) | `role` | letras (com acento), dígitos, espaço e `:._#/-`, até 96 | nunca |
| data | `date`, `reference_date` | `AAAA-MM-DD` válida | nunca |
| decimal | `amount`, `percent`, `percent_on`, `percent_pn` | texto `-?dígitos[.dígitos]` | nunca |
| número | `year`, `items`, `legislature`, `form_version` | inteiro, sempre na forma canônica (`"007"` → `7`, `"-0"` → `0`; `"1.0"` é descartado) | nunca |
| enumerado | `status` (`[A-Za-z_]`, até 32), `uf` (2 letras maiúsculas) | | nunca |

Identificadores e códigos nunca são mascarados; textos livres passam pela
máscara de CPF. Assim um número de receita de 11 dígitos chega intacto
(`"receipt_id": "12345678901"`), enquanto um nome de urna com 11 dígitos seguidos
é mascarado. Só valores simples passam: objetos e listas são descartados, e
`null` também (ausente e `null` significam o mesmo: "não informado"). Números
que a fonte grava como texto (`amount`, `percent*`) continuam texto: decimais
exatos, nunca arredondados. Um texto vazio num identificador (p.ex. `city` numa
eleição estadual) é omitido.

| Vínculo (fonte) | Chave | Tipo | Significado |
|---|---|---|---|
| `partner_of`, `administrator_of`, `legal_rep_of` (receita) | `role` | texto | código de qualificação do sócio ou representante (tabela da Receita) |
| `candidate_for` (tse) | `year` | número | ano da eleição |
| | `office`, `office_code` | texto | cargo disputado (descrição e código do TSE) |
| | `uf` | texto | UF da candidatura |
| | `city`, `city_name` | texto | código TSE e nome do município (`city` ausente e `city_name` vazio em eleição estadual ou federal) |
| | `result` | texto | situação final do candidato no turno (p.ex. `ELEITO`, `NÃO ELEITO`); vazio se não publicada |
| | `ballot_name` | texto | nome de urna (público), mascarado como todo texto |
| | `role` | texto | `<ano>:<código do cargo>:<UF>:<município>` |
| `donated_to` (tse) | `year` | número | ano da eleição |
| | `amount` | texto decimal | soma das linhas do recibo, exata (p.ex. `"1500.50"`) |
| | `date` | texto `AAAA-MM-DD` | data da receita (a menor entre as linhas) |
| | `origin`, `origin_code` | texto | origem do recurso como publicada |
| | `receipt_id` | texto | número da receita (`SQ_RECEITA`) |
| | `items` | número | quantas linhas da prestação de contas compõem a receita |
| | `role` | texto | igual a `receipt_id` |
| `holds_mandate` (camara, senado) | `legislature` | número | número da legislatura |
| | `uf` | texto | UF do mandato |
| | `party` | texto | sigla do partido no trecho; vazio = sem partido |
| | `status` | texto | `exercicio` (titular em exercício), `licenciado` (titular licenciado; o assento continua dele), `suplente_exercicio` (suplente em exercício) |
| | `role` | texto | `<legislatura>:<partido>:<UF>:<status>` |
| `shareholder_of` (cvm) | `percent` | texto decimal | participação no capital total; **ausente = não informado** (nunca `"0"` inventado) |
| | `percent_on`, `percent_pn` | texto decimal | participação nas ações ordinárias / preferenciais; ausente = não informado |
| | `reference_date` | texto `AAAA-MM-DD` | data de referência do formulário da CVM |
| | `form_version` | número | versão do formulário |
| | `described_by` | texto | só em participações indiretas: CNPJ básico da companhia aberta cujo formulário descreveu a holding |
| | `role` | texto | `shareholder` |
| `controls` (cvm) | `reference_date`, `form_version`, `described_by` | como acima | |
| | `role` | texto | `controller` |

Exemplo (`donated_to`):

```json
"attrs": {"year": 2024, "amount": "1500.50", "date": "2024-09-12", "origin": "Recursos de pessoas físicas",
          "origin_code": "10010100", "receipt_id": "12345678901", "items": 2, "role": "12345678901"}
```

## Rotas

### `GET /healthz` (público)

`200 {"status":"ok"}`.

### `GET /api/v1/search?q=&kind=&limit=` (analyst)

- `q`: até 200 caracteres. Com 8 ou 14 dígitos (pontuação ignorada) busca o CNPJ
  básico exato; caso contrário busca por nome, sem acentos nem caixa.
- `kind`: opcional, `person | company | party | public_body | foreign`.
- `limit`: padrão 20, máximo 50 (acima disso é reduzido); não numérico ou < 1 → 400.

```json
{"items": [{"id": 12, "kind": "company", "name": "EMPRESA FICTICIA A LTDA", "natural_key": "90000002"}]}
```

Sem resultados: `{"items": []}`. Limitações aceitas da busca por nome: casa
palavras inteiras (e prefixo só da última palavra digitada) — não encontra um
trecho no meio de uma palavra — e não tolera erros de digitação. Uma busca com
um CPF completo nunca confirma a associação entre CPF e nome. A auditoria guarda
só `kind` e a quantidade de resultados.

### `GET /api/v1/entities/{id}` (analyst)

```json
{
  "entity": {"id": 12, "kind": "company", "name": "EMPRESA FICTICIA A LTDA", "natural_key": "90000002"},
  "versions": [{
    "id": 3, "name": "EMPRESA FICTICIA A LTDA", "legal_nature": "2062", "size": "", "uf": "SP",
    "city": "7107", "cnae": "", "status": "02", "capital": "1000.00", "open": true,
    "first_collection_id": 7, "last_seen_collection_id": 9, "closed_collection_id": null,
    "first_seen_reference_date": "2026-09-01", "last_seen_reference_date": "2026-10-01",
    "closed_reference_date": null,
    "record_ref": "K3241.K03200Y0.D61010.EMPRECSV:15", "recorded_at": "2026-11-01T10:00:00Z"
  }],
  "records": [],
  "records_truncated": false,
  "possible_same": []
}
```

Página de uma pessoa (trechos):

```json
{
  "records": [{"id": 31, "entity_id": 40, "source_key": "receita", "context_key": "60000001",
               "name": "PESSOA FICTICIA", "cpf_masked": "***777777**", "identity_confidence": 1, "locked": false,
               "record_ref": "K3241.K03200Y0.D61010.SOCIOCSV:42", "first_collection_id": 7,
               "first_seen_reference_date": "2026-10-01"},
              {"id": 35, "entity_id": 40, "source_key": "tse", "context_key": "donor:2024:250000000001",
               "name": "PESSOA FICTICIA", "identity_confidence": 0.9996, "locked": false,
               "record_ref": "receitas_candidatos_2024_BRASIL.csv:1021", "first_collection_id": 9,
               "first_seen_reference_date": "2026-11-10"}],
  "possible_same": [{"entity_id": 41, "name": "PESSOA FICTICIA", "same_name": true, "records": 2,
                     "shared_fragment": true}]
}
```

- `versions`: versões cadastrais (empresas), abertas e fechadas, mais nova
  primeiro, até 100; `uf` e `city` como gravados (código do município da Receita).
  Quando valeu, sem datas inventadas: `first_seen_reference_date` e
  `last_seen_reference_date` são as datas de referência das coletas que viram a
  versão pela primeira e pela última vez; `closed_reference_date`, a da coleta
  que a fechou (`null` enquanto aberta). Os ids das coletas continuam;
  `record_ref` é a linha do arquivo da fonte de onde a versão foi lida.
- `records` (pessoas): até 500 registros que compõem a entidade —
  `{"id","entity_id","source_key","context_key","name","cpf_masked","identity_confidence","locked","record_ref","first_collection_id","first_seen_reference_date"}`.
  `cpf_masked` é **omitido** quando o registro não tem fragmento de CPF.
  `locked`: um analista fixou o registro nessa entidade. Proveniência mínima do
  registro (o que o banco guarda para ele): `record_ref` (arquivo da fonte e
  linha ou elemento em que foi lido pela primeira vez, com máscara de CPF como
  os nomes), `first_collection_id` e `first_seen_reference_date` (a data de
  referência dessa coleta; `null` só se a coleta não existir).
- `possible_same` (pessoas): até 20 outras entidades que podem ser a mesma
  pessoa, com a base que o sistema tem (não há nota de confiança: nenhuma é
  calculada) — `same_name` (sempre `true`: algum registro tem o mesmo nome
  normalizado; é o motivo de aparecer), `records` (quantos registros a
  candidata tem) e `shared_fragment` (também o mesmo fragmento de CPF — o
  fragmento nunca é mostrado). Os com fragmento em comum vêm primeiro. Nunca são
  unidas automaticamente.
- id inexistente → 404.

### `GET /api/v1/entities/{id}/relationships?state=&type=&at=&cursor=&limit=` (analyst)

- `state`: `open` (padrão), `closed` ou `all`.
- `type`: repetido (`type=a&type=b`) ou separado por vírgula; tipos válidos:
  `partner_of, administrator_of, legal_rep_of, donated_to, candidate_for,
  holds_mandate, controls, shareholder_of`.
- `at` (`AAAA-MM-DD`): só vínculos válidos nessa data, abertos ou fechados (com
  `state=closed`, só os fechados válidos nela).
- `limit`: padrão e máximo 200. `cursor`: o `next_cursor` da página anterior.

```json
{"items": [ /* edges */ ], "next_cursor": "MTIz"}
```

`next_cursor` vem `""` na última página.

### `GET /api/v1/relationships/{id}/provenance` (analyst)

```json
{
  "relationship_id": 501,
  "kind": "fact",
  "confidence": null,
  "record_ref": "K3241.K03200Y0.D61010.SOCIOCSV:42",
  "recorded_at": "2026-11-01T10:00:00Z",
  "last_seen_collection_id": 7,
  "closed_collection_id": null,
  "source": {"key": "receita", "name": "Receita Federal — CNPJ"},
  "collection": {
    "id": 7, "reference_date": "2026-10-01", "observed_date": "2026-10-13",
    "collected_at": "2026-11-01T10:30:00Z", "status": "done", "full_scope": true, "stale": false
  },
  "last_seen_collection": {
    "id": 7, "reference_date": "2026-10-01", "observed_date": "2026-10-13", "collected_at": "2026-11-01T10:30:00Z"
  },
  "raw_key": "raw/receita/2026-10-01/Socios0.zip",
  "raw_sha256": "9f2c0d1e5b7a4c3d2e1f0a9b8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d",
  "raw_size": 251000000,
  "source_url": "https://arquivos.receitafederal.gov.br/public.php/webdav/Dados/Cadastros/CNPJ/2026-10/Socios0.zip",
  "source_note": "Arquivo do compartilhamento público da Receita Federal (Nextcloud): ..."
}
```

`record_ref` é o nome do arquivo de dados dentro do pacote da fonte, `:` e o
número da linha. `collection.collected_at` é o fim da coleta e pode ser `null`
(coleta ainda em andamento ou que falhou). `stale` é calculado como no mapa: a fonte está desatualizada hoje. `raw_key` (objeto
preservado no armazenamento bruto), `raw_sha256` e `raw_size` identificam o
arquivo de onde o vínculo foi lido (tabela `collection_file`, ligada pela parte
do `record_ref` antes do último `:`); só faltam em coletas anteriores ao registro
dos arquivos.

- `collection`: a coleta que leu o vínculo pela primeira vez; `observed_date` é
  a data até a qual o estado da fonte foi de fato observado (para a Receita, a
  publicação da edição; para as outras fontes, o dia em que a coleta começou).
- `last_seen_collection`: a coleta que viu o vínculo pela última vez (`id`,
  `reference_date`, `observed_date`, `collected_at`), para mostrar "confirmado
  pela última vez em ..."; `null` quando nenhuma está registrada. O id continua
  também em `last_seen_collection_id`.
- `source_url`: de onde o arquivo foi baixado (para fontes de API, a URL da
  requisição), sem credenciais; sempre presente, `null` quando não há arquivo
  registrado ou o arquivo foi registrado antes das URLs. Na Receita é o endereço
  WebDAV do compartilhamento público, que só abre com o código público do
  compartilhamento como usuário (ele nunca é guardado); por isso vem também
  `source_note`, um texto fixo dizendo como chegar ao arquivo pelo navegador.
  `source_note` é omitido nas outras fontes.
- Não há rota que sirva o arquivo bruto: os arquivos preservados trazem CPFs
  completos onde a fonte os publica, e servi-los depende da revisão da LGPD.

### `POST /api/v1/jobs` (analyst)

Corpo `{"type": "...", "params": {...}}`; os parâmetros são validados antes de
enfileirar.

- `paths`: `{"a": 11, "b": 12, "max_depth": 6, "rel_types": ["partner_of"], "at": "2024-01-01"}`
  (`max_depth` 0..6, 0 = padrão; `rel_types` e `at` opcionais).
- `control_group`: `{"company_id": 12, "mode": "documented" | "corporate_links"}`.
- As chaves de `params` são exatas: chave repetida (também só com caixa
  diferente, como `"a"` e `"A"`), chave escrita em outra caixa (`"MODE"`) ou
  desconhecida → 400.

Resposta `202 {"id": "0193b6c2-7f00-7000-8000-000000000001"}`.

### `GET /api/v1/jobs/{id}` (dono ou admin)

```json
{
  "id": "0193b6c2-7f00-7000-8000-000000000001",
  "type": "paths",
  "params": {"a": 11, "b": 12},
  "status": "done",
  "progress": 1,
  "result": {"paths": [[{"edge": { /* edge */ }, "forward": true}]], "stopped_hubs": [], "truncated": false},
  "error": "",
  "created_at": "2026-11-11T12:00:00Z",
  "finished_at": "2026-11-11T12:00:02Z"
}
```

- `status`: `queued | running | done | failed`; `result` é `null` até `done`.
- `control_group` devolve `{"root","controllers","members":[{"entity","depth","via"}],"edges","stops":[{"entity","reason"}]}`.
- Os resultados passam pelo mesmo formato da API (nomes mascarados, datas `AAAA-MM-DD`, mesmos `attrs` dos vínculos).
- `error` só traz mensagens conhecidas (`timeout`, `entity not found`,
  `interrupted`, `expired`, `result too large`, `internal error`, ...); qualquer
  outra aparece como `failed`.
- id que não é UUID → 400; job inexistente ou de outra pessoa → 404.
- Retenção: um job concluído (`done` ou `failed`) é apagado 30 dias depois de
  terminar (até 10 000 por minuto); depois disso o id responde 404. A auditoria
  continua registrando quem pediu o quê.
- Cada vínculo do resultado tem `closed` e `identity_basis`, como nas outras
  rotas (ver "Fato e inferência").

### `GET /api/v1/identity/proposals?status=&cursor=` (analyst)

`status`: `pending` (padrão), `confirmed`, `rejected` ou `obsolete`. 50 por página,
mais conectadas primeiro.

```json
{
  "items": [{
    "id": 9,
    "status": "pending",
    "name": "PESSOA REPETIDA FICTICIA",
    "records": [
      {"id": 31, "entity_id": 40, "source_key": "receita", "context_key": "60000001",
       "name": "PESSOA REPETIDA FICTICIA", "cpf_masked": "***777777**", "identity_confidence": 1, "locked": false,
       "free": true},
      {"id": 32, "entity_id": 41, "source_key": "receita", "context_key": "60000002",
       "name": "PESSOA REPETIDA FICTICIA", "cpf_masked": "***777777**", "identity_confidence": 1, "locked": false,
       "free": true}
    ],
    "connections": 3,
    "reason": "150 distinct CPF fragments share this name (automatic merge up to 100)",
    "reason_code": "common_name",
    "target_entity": {
      "entity": {"id": 40, "kind": "person", "name": "PESSOA REPETIDA FICTICIA", "natural_key": ""},
      "other_records": [{"id": 30, "source_key": "receita", "context_key": "60000003"}],
      "other_records_truncated": false
    }
  }],
  "next_cursor": ""
}
```

`target_entity` é a entidade em que a confirmação juntaria os registros (a do
registro livre de menor id) e os outros registros que ela já tem, para o
analista ver com quem está juntando. Cada linha de `records` traz o registro
(`id`, `entity_id`), de onde ele vem (`source_key`, `context_key`), o nome
mascarado, `cpf_masked` (omitido quando não há fragmento), `identity_confidence`,
`locked` e `free` — nada mais de pessoal. `free`: o registro está ligado a uma
entidade e não foi fixado por decisão de analista, então a decisão age sobre ele
(a confirmação o move para `target_entity`); `free: false` → "não será movido".
`reason` diz por que a junção não foi automática, em texto livre (inglês, pode
mudar); `reason_code` é a forma estável, para a tela decidir o que mostrar:

| `reason_code` | Quando |
|---|---|
| `common_name` | o nome tem mais trechos de CPF diferentes do que o limiar de junção automática permite |
| `impure_target` | os registros entrariam numa entidade montada por um analista |
| `ambiguous_key` | um analista já separou registros com esse nome e trecho de CPF |
| `other` | qualquer outro texto (nenhum hoje) |

### `POST /api/v1/identity/proposals/{id}/decision` (analyst)

Corpo `{"confirm": true}` (juntar) ou `{"confirm": false}` (manter separados).
`204` sem corpo. Proposta já decidida → 409; confirmar (`confirm: true`) uma
proposta com menos de dois registros ainda livres → 409 (recusar continua
possível: separa e fixa os que restam). Inexistente → 404.

Campos opcionais, para a decisão valer só sobre o que a tela mostrou:

```json
{"confirm": true, "expected_target_entity_id": 40, "expected_record_ids": [31, 32]}
```

- `expected_target_entity_id` (inteiro ou `null`): `target_entity.entity.id` da
  listagem; `null` = a tela não mostrou entidade alvo (nenhuma confirmação
  combina com isso). Ignorado quando `confirm` é `false`.
- `expected_record_ids` (lista de inteiros > 0, até 10000): os registros com
  `free: true` da listagem, em qualquer ordem — os que a decisão vai mover
  (confirmar) ou separar e fixar (rejeitar). `null` equivale a ausente.

Quando presentes, são conferidos na mesma transação da decisão, depois da trava
de identidade, contra a entidade em que ela juntaria e o conjunto de registros
livres naquele instante. Qualquer diferença → 409 `conflict` (os dados mudaram
desde que a tela foi carregada) e nada é feito: nenhuma decisão gravada, nenhum
registro movido, a proposta continua pendente. Ausentes → comportamento antigo.
Valor malformado (texto, 0, negativo) → 400. A tentativa é auditada com os
valores esperados (só ids).

### `POST /api/v1/identity/merge` e `POST /api/v1/identity/split` (analyst)

Corpo `{"record_ids": [31, 32], "reason": "mesmo endereço e sócios em comum"}`.

- merge: 2 a 100 ids; junta os registros na entidade do de menor id.
- split: 1 a 100 ids; move os registros para uma entidade nova.
- `reason`: obrigatório, 1 a 500 caracteres; fica na decisão de identidade, não
  na auditoria.
- merge, opcional `entity_ids` (1 a 10 ids > 0): as entidades em que a tela
  mostrou os registros. O servidor confere, na transação do merge e depois da
  trava, que cada registro de `record_ids` está numa dessas entidades, que cada
  uma delas tem registro em `record_ids` e que `record_ids` traz **todos** os
  registros de pessoa delas (o resultado é uma entidade com tudo dos dois lados,
  nada fica para trás nem vem de outra entidade).
- split, opcional `entity_id` (> 0): todos os registros têm de estar nessa
  entidade, e ela tem de ficar com pelo menos um outro registro (separar todos
  os registros de uma entidade não faz sentido).
- `entity_ids` no split, `entity_id` no merge, lista vazia ou com mais de 10 →
  400. Diferença na conferência → 409 `conflict` (os dados mudaram desde que a
  tela foi carregada), sem nada feito; a tentativa fica na auditoria com os ids
  esperados. Ausentes → comportamento antigo.

Resposta `{"entity_id": 40}`. Registro inexistente → 404.

### `GET /api/v1/sources` (analyst)

```json
[{
  "key": "receita",
  "name": "Receita Federal — CNPJ",
  "collectable": true,
  "editions": true,
  "status": "fresh",
  "last_collected_at": "2026-11-01T10:30:00Z",
  "next_expected_at": "2026-10-31T00:00:00Z",
  "last_reference_date": "2026-10-01",
  "last_observed_date": "2026-10-13",
  "last_checked_at": "2026-11-05T06:00:12Z",
  "last_error": "",
  "last_stats": {"rows_read": 1000, "rows_skipped": 0},
  "pending_request_at": null
}]
```

(Exemplo visto por um `admin`. Para um analista, `last_error` não vem.)

- `status`: `failed` (a coleta terminada mais recente falhou e nenhuma
  verificação bem-sucedida veio depois dela), `never` (nenhuma
  coleta completa concluída), `stale` (a última coleta completa concluída é
  mais velha que o prazo da fonte — contado de `last_observed_date` para a
  Receita, publicada em edições, e de `last_reference_date` para as demais),
  senão `fresh`. Uma coleta em andamento não muda o status.
- `last_collected_at`, `last_reference_date` e `last_observed_date`: da última
  coleta completa concluída; `null` quando `never`. Para a Receita, a data de
  referência é o primeiro dia do mês da edição e `last_observed_date` a data de
  publicação dela (o arquivo de dados mais novo da edição); para as outras
  fontes, `last_observed_date` é o dia em que a coleta começou.
- `next_expected_at`: data de referência + periodicidade da fonte (Receita: 30
  dias; referência 2026-10-01 → 2026-10-31T00:00:00Z), à meia-noite UTC. É
  quando se espera que a fonte publique dados novos — o agendador passa a
  procurá-los a partir daí —, não uma promessa da fonte nem o momento em que a
  fonte fica `stale` (isso depende do prazo de validade, contado como acima).
  Pode estar no passado enquanto a fonte não publica. `null` quando `never`.
- `last_checked_at`: última vez em que a fonte foi confirmada em dia — o fim da
  última coleta completa concluída ou uma verificação posterior que não achou
  edição nova (nesse caso não se cria coleta). Uma verificação assim, depois de
  uma tentativa que falhou, tira a fonte de `failed`. `null` quando `never`.
- `collectable`: a fonte tem conector, então pode ser coletada (hoje todas: `receita`, `tse`, `camara`, `senado` e `cvm`).
- `editions`: a fonte é publicada em edições datadas (hoje só a `receita`): a
  data de referência dá nome à edição e o prazo de validade conta de
  `last_observed_date` (a publicação da edição). `false` nas demais.
- `last_error`: só para quem tem o papel `admin` (ausente para analistas), em
  todas as fontes: o texto do erro enquanto `failed`, com CPFs mascarados e até
  300 caracteres; `""` quando nada falhou.
- `pending_request_at`: hora (RFC 3339, UTC) do pedido de coleta completa já
  enfileirado que ainda não começou (nenhuma coleta completa começou, nem uma
  verificação sem novidade foi registrada, depois dele; vale por até 72 h), para
  a tela mostrar "coleta pedida às HH:MM, aguardando início"; `null` quando não há.
- `last_stats`: contadores da última coleta concluída (completa ou recorte).

### `POST /api/v1/sources/{key}/collect` (admin)

Enfileira uma coleta para o worker, como `geoint collect`, com data de referência
de hoje (UTC).

- Só corpo ausente/vazio ou `{}` pede coleta completa.
- `{"cnpjs": ["12.345.678/0001-90", "87654321"], "depth": 1}`: recorte — até 1000
  CNPJs (8 ou 14 caracteres, pontuação ignorada; guarda-se a raiz de 8) e `depth`
  de 0 a 3. Lista vazia, `null` em `cnpjs` ou `depth`, `depth` sem `cnpjs` ou
  CNPJ inválido → 400 (um engano nunca vira coleta nacional). Recorte só existe
  para a `receita`: pedido com `cnpjs`/`depth` para outra fonte (p.ex. `tse`) →
  400, sem auditoria nem mensagem.
- Fonte fora do catálogo → 404; fonte sem conector (`collectable: false`) → 409,
  sem auditoria nem mensagem. Resposta `202 {}`.
- O pedido é registrado (`collection_request`, com o `sub` de quem pediu) pelo
  mesmo caminho do agendador, e conta para ele: depois de um pedido manual o
  agendador não pede a mesma fonte por 24 h.
- Coleta completa: já existe uma coleta em andamento da fonte (há menos de
  24 h), ou um pedido completo aguardando início (o de `pending_request_at`, ou
  um registrado há menos de 10 min ainda não enfileirado) → 409 `conflict`. A
  tentativa é auditada; nada é enfileirado. Assim que a coleta pedida começa (ou
  uma verificação não acha edição nova), um novo pedido é aceito na hora — o
  intervalo de 24 h vale só para o agendador.
- Recorte (`cnpjs`) nunca é barrado por isso, nem barra pedidos completos.
