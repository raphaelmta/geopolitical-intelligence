# Spike: Receita nacional no PostgreSQL e busca de caminhos

Data: 2026-11-11. Teste descartável (Tarefa 1 do plano da etapa 1). O código
(`spike/`) foi apagado depois das medições; ficam este documento e
`docs/sources/receita.md`.

## Decisão

**As metas foram atingidas** com o desenho final abaixo. A busca por nome
**aceita duas perdas**:

- não acha **substring no meio de palavra**: `BRAS` não acha `PETROBRAS`;
- não tem **tolerância a erro de digitação**.

As outras perdas estão listadas em "Perdas conhecidas".

PATHS_STRATEGY = bfs

Busca por nome: bloco `SEARCH_STRATEGY` na seção "Busca por nome — segunda
medição".

| Medida | Meta | Resultado (desenho final) | Situação |
|---|---|---|---|
| M1 busca por nome: 70 termos (50 de M1, 10 prefixos digitados, 10 nomes parciais) × 3 escopos | p95 ≤ 1 s | frio: p95 0,092 s, máx. 0,405 s; quente: p95 0,080 s, máx. 0,396 s | passou |
| M1, grupo extra de 10 termos adversos × 3 escopos (não pedido no brief) | — | máx. 0,616 s (frio) | abaixo de 1 s |
| M2 vizinhos (200 aleatórios + 20 de maior grau, página de 200) | p95 ≤ 2 s | p95 0,285 s; máx. 1,41 s | passou |
| M3a caminhos, CTE recursiva (25 pares) | cada ≤ 120 s | máx. 0,117 s | passou |
| M3b caminhos, BFS bidirecional em Go (25 pares) | cada ≤ 120 s | máx. 0,215 s | passou |
| M4 grupo de controle (20 empresas) | cada ≤ 120 s | máx. 0,068 s | passou |
| M5 carga completa do desenho final (bruto + grafo + tabelas de busca) | ≤ 12 h | 2.545 s (42 min 25 s), dos quais 819 s são as tabelas de busca; banco final com 76.506.568.383 bytes (71 GiB) | passou |

M2, M3 e M4 foram medidos na primeira carga, que tinha o mesmo grafo. M1 e
M5 foram medidos na carga final. Histórico: na primeira medição M1 falhou
(busca por substring com trigramas, p95 de 2,04 s). Rafael escolheu busca por
palavras no PostgreSQL. O desenho final saiu de cinco iterações, resumidas na
seção de busca.

## Ambiente

- Máquina de referência: 6 núcleos, 31 GB de RAM, NVMe, Fedora, Docker.
- PostgreSQL 18.6 (imagem `postgres:18`), projeto Compose `geoint-spike`,
  volume nomeado, porta publicada só em `127.0.0.1`.
- Configuração: `shared_buffers=8GB`, `effective_cache_size=20GB`,
  `maintenance_work_mem=2GB`, `work_mem=256MB` (1–2 GB nas etapas de
  derivação), `max_wal_size=64GB`, `checkpoint_timeout=30min`,
  `wal_level=minimal`, `synchronous_commit=off`, `max_parallel_workers=6`,
  `max_parallel_workers_per_gather=4`, `max_parallel_maintenance_workers=4`,
  `random_page_cost=1.1`, `jit=off`, `autovacuum=off`. `fsync` ficou ligado.
- Dados: Receita de **2026-10**, a mais recente publicada em 2026-11-11 (não
  havia 2026-11). Foram baixados os 37 arquivos do mês, 7.808.752.942 bytes
  (7,27 GiB), em 37 min 26 s, e `unzip -t` passou em todos. A carga usa
  Empresas, Sócios e Estabelecimentos; Simples e as tabelas de domínio foram
  baixados mas não carregados no grafo. Detalhes em `docs/sources/receita.md`.
- Medições feitas por programas Go (pgx v5) num contêiner na mesma rede
  Docker, uma consulta por vez. O tempo inclui as idas e voltas ao banco e a
  leitura de todas as linhas.

## Modelo carregado (desenho final)

- Tabelas brutas `UNLOGGED`, com todas as colunas `text`, carregadas com
  `COPY … WITH (FORMAT csv, DELIMITER ';', QUOTE '"', ENCODING 'LATIN1')` a
  partir de `unzip -p | tr -d '\000'`, 4 arquivos em paralelo.
- `s_company(cnpj_basico, name, legal_nature)`.
- `s_partner`: uma linha por registro de Sócios, com a chave do nó de
  origem:
  - PJ → empresa, pelo `cnpj_basico` do sócio;
  - PF → (nome normalizado, 6 dígitos visíveis do CPF);
  - estrangeiro → (nome normalizado, documento, que vem vazio).
- `s_node(id, entity_id uuid, kind, key, name_norm)`: `C` empresa, `P`
  pessoa física, `E` estrangeiro. `entity_id = gen_random_uuid()`.
- `s_edge(src, dst, rel, src_kind)`: sócio → empresa, com `rel` = código de
  qualificação. Índices em `s_edge(src)` e `s_edge(dst)`, com `INCLUDE`.
- `s_degree(id, degree)`: número de arestas de cada nó.
- Tabelas de busca: `search_company`, `search_person` e
  `search_frequent_token` (ver `SEARCH_STRATEGY`). **Não há** índice trigram.
- O representante legal **não** virou aresta neste teste.

## M5 — carga (desenho final)

| Fase | Tempo (s) |
|---|---|
| schema | 0 |
| COPY Empresas (10 arquivos) | 34 |
| COPY Sócios (10 arquivos) | 16 |
| COPY Estabelecimentos (10 arquivos) | 122 |
| derivar `s_company` | 94 |
| derivar `s_partner` | 75 |
| derivar `s_node` (normalização, `entity_id`, chave primária, índice único) | 977 |
| derivar `s_edge` | 101 |
| índices de `s_edge` | 15 |
| `s_degree` | 35 |
| `VACUUM ANALYZE` do banco | 257 |
| tabelas de busca (construção completa e troca; detalhe na seção de busca) | 819 |
| **Total** | **2.545 (42 min 25 s)** |

Ressalva: entre o COPY e parte da derivação de `s_node`, rodou em paralelo
uma leitura completa dos CSV em Python, para conferir aspas e quebras de
linha. Ela disputou CPU e disco com a carga, então o tempo acima pode estar
um pouco acima do real (não medido quanto). O download (37 min 26 s) não
está no total.

Tamanho do banco no fim: **76.506.568.383 bytes (71 GiB)**. Tamanho total
de cada tabela, com índices:

| Tabela | Linhas | Bytes |
|---|---|---|
| search_company | 70.517.432 | 16.671.383.552 |
| r_estabelecimentos | 73.809.379 | 15.333.736.448 |
| s_node | 88.825.331 | 14.549.245.952 |
| r_empresas | 70.517.438 | 6.800.490.496 |
| s_company | 70.517.438 | 5.778.767.872 |
| s_partner | 28.495.961 | 4.265.246.720 |
| s_edge | 28.495.961 | 3.796.623.360 |
| search_person | 18.307.222 | 3.722.641.408 |
| r_socios | 28.495.961 | 3.217.014.784 |
| s_degree (nós com ≥ 1 aresta) | 35.393.694 | 2.363.121.664 |
| search_frequent_token | 462 | 73.728 |

Nós por tipo: 70.517.438 empresas, 18.296.000 pessoas físicas e 11.893
estrangeiros. Registros de Sócios por tipo:

- 742.695 PJ;
- 27.740.460 PF, todos com CPF mascarado no padrão `***NNNNNN**`;
- 12.806 estrangeiros, todos sem documento.

Todas as linhas de Sócios viraram aresta. Um `cnpj_basico` aparece duas vezes
em Empresas, e um sócio PJ aponta para um `cnpj_basico` que não está em
Empresas.

Histórico: a primeira carga, com índice GIN trigram (258 s) e sem
`entity_id` nem tabelas de busca, levou 1.690 s e ocupava 55 GB. Esse desenho
foi abandonado.

## Forma do grafo

Medido na primeira carga; os dados são os mesmos na carga final.

O grafo da Receita sozinha é **muito esparso e fragmentado**. 53,4 milhões de
empresas não têm nenhum sócio no arquivo, e o grau médio dos nós com pelo
menos uma aresta é 1,61.

Distribuição de grau (35.393.694 nós com ≥ 1 aresta):

| p50 | p90 | p99 | p99,9 | p99,99 | máximo | grau > 500 | grau > 1000 |
|---|---|---|---|---|---|---|---|
| 1 | 2 | 6 | 16 | 56 | 3.728 | 111 nós | 32 nós |

Os 20 nós de maior grau. Os nomes de pessoas físicas foram omitidos porque
são dados pessoais.

| # | Grau | Tipo | Nome normalizado |
|---|---|---|---|
| 1 | 3.728 | P | *(pessoa física, nome omitido)* |
| 2 | 3.027 | P | *(pessoa física, nome omitido)* |
| 3 | 2.586 | C | P & V SERVICOS MEDICOS LTDA |
| 4 | 2.219 | P | *(pessoa física, nome omitido)* |
| 5 | 1.753 | P | *(pessoa física, nome omitido)* |
| 6 | 1.611 | C | MEDICALMAIS SERVICOS EM SAUDE LTDA |
| 7 | 1.487 | C | ALTA SAUDE - PRONTO SOCORRO CLINICA MEDICA E CIRURGICA LTDA |
| 8 | 1.452 | C | HELPMED SAUDE LTDA |
| 9 | 1.357 | C | BEST INTERMEDIACOES SMART |
| 10 | 1.343 | C | SMPV SERVICOS MEDICOS E DIAGNOSTICOS POR IMAGEM LTDA |
| 11 | 1.329 | C | CONSORCIO ALSOLAR |
| 12 | 1.292 | P | *(pessoa física, nome omitido)* |
| 13 | 1.235 | C | SOLIDA SAUDE SERVICOS MEDICOS LTDA |
| 14 | 1.132 | C | KROS SAUDE SAO PAULO CLINICA MEDICA E CIRURGICA LTDA |
| 15 | 1.131 | P | *(pessoa física, nome omitido)* |
| 16 | 1.128 | P | *(pessoa física, nome omitido)* |
| 17 | 1.117 | C | HW SERVICOS MEDICOS LTDA |
| 18 | 1.107 | C | EBES SISTEMAS DE ENERGIA SA |
| 19 | 1.103 | C | AVDV ESTETICA LTDA - FALIDO |
| 20 | 1.089 | P | *(nome vazio, fragmento de CPF `000000`)*: registros sem nome fundidos num nó falso |

Entre os maiores graus aparecem dois grupos:

- empresas de serviços médicos que têm centenas de PJs de médicos como
  sócias;
- 7 pessoas físicas que figuram em mais de 1.000 empresas cada (hipótese não
  verificada: sócios de "sociedades de profissionais" ou procuradores).

O nó 20 é um artefato. Junta 1.777 registros PF sem nome (671 nós `P` com
nome vazio), e a carga real precisa tratar isso.

### Homônimos

**8** nomes de sócio PF têm mais de 1.000 fragmentos de CPF distintos cada,
de 1.016 a 1.972. Os nomes não estão listados aqui. Todos são combinações
muito comuns de prenome com sobrenome do tipo "DA SILVA" ou "DOS SANTOS".
Esses 8 nomes somam 10.688 fragmentos distintos: 1.972 + 1.473 + 1.343 +
1.327 + 1.268 + 1.193 + 1.096 + 1.016.

## M2 — vizinhos

Primeira página (200) dos vizinhos com nome e tipo, mais o grau total.

| Conjunto | p50 | p95 | máx. |
|---|---|---|---|
| 200 nós aleatórios | 0,006 s | 0,010 s | 0,017 s |
| 20 de maior grau | 0,297 s | 1,11 s | 1,41 s |
| Todos (220) | 0,006 s | **0,285 s** | 1,41 s |

## M3 — caminhos (profundidade ≤ 6)

- **M3a**: CTE recursiva, grafo tratado como não direcionado, checagem de
  ciclo pelo vetor do caminho, para no primeiro caminho encontrado, sem poda
  de nós de grau alto. Limite de 120 s por consulta.
- **M3b**: BFS bidirecional em Go, expandindo sempre a fronteira menor, uma
  consulta `= ANY($1)` por direção da aresta e por bloco de até 100 mil ids;
  nós de grau > 500 não são expandidos (exceto as pontas), mas podem ser o
  ponto de encontro das duas buscas.

Pares do brief: 20 pares aleatórios (nós com ≥ 1 aresta) e 5 pares de
vizinhos de 5 nós sorteados com grau > 1000 (graus dos hubs: 2.586, 1.487,
1.073, 1.131, 1.753).

| Conjunto | M3a p50 / máx. | M3b p50 / máx. | caminhos achados |
|---|---|---|---|
| 20 aleatórios | 0,000 s / 0,117 s | 0,013 s / 0,215 s | 0 (nenhum par está ligado em até 6 passos) |
| 5 via hub | ≤ 0,002 s | ≤ 0,006 s | 5 de 5 (comprimento 2, pelo hub) |
| **25 do brief** | **máx. 0,117 s** | **máx. 0,215 s** | 5 |

Como o grafo é fragmentado, os pares aleatórios ficam em componentes
minúsculos e a busca se esgota com poucos nós (no máximo 475 visitados pelo
BFS). O teste do brief, sozinho, quase não exige nada. Por isso rodei um
**conjunto extra** (não previsto no brief) de 20 pares ligados com certeza:
pontas de passeios aleatórios de 6 passos sem repetir nó, 10 a partir de nós
aleatórios de grau ≥ 2 e 10 a partir de vizinhos dos 20 maiores hubs. Muitos
passeios morrem antes de 6 passos (5 chegaram a 6):

| Conjunto extra | M3a p50 / p95 / máx. | M3b p50 / p95 / máx. | achados |
|---|---|---|---|
| 20 pares de passeio | 0,001 s / 2,13 s / **> 120 s (1 estouro)** | 0,004 s / 0,057 s / 0,065 s | M3a 19 de 20; M3b 20 de 20 |

No par que estourou (pontas a 6 passos, perto de um hub), a CTE passou dos
120 s e foi cancelada; o BFS achou o caminho de comprimento 6 em 0,057 s
visitando 271 nós. A CTE também devolve o primeiro caminho, não o mais curto.
Os dois passam no critério do brief; o BFS é o único que aguentou o conjunto
extra, por isso a indicação `bfs`.

Correção durante o teste: na primeira rodada o BFS parava quando uma das
pontas só tinha hubs na fronteira (deixou de achar 1 dos 5 pares via hub).
Corrigido para a outra ponta continuar a busca; os números acima são da
rodada corrigida. A primeira rodada (M3b máx. 0,039 s, M3a máx. 1,25 s nos 25
pares) não muda a conclusão.

## M4 — grupo de controle

Grupo = todos os controladores acima (sócios PJ, recursivo) mais todas as
participadas abaixo (recursivo), com `UNION` para cortar ciclos e sem limite
de profundidade. 20 empresas: as 5 com mais participadas (1.610, 1.101,
1.099, 1.076, 1.072 participadas) e 15 sorteadas entre empresas que são sócias
de outras.

| p50 | p95 | máx. | tamanho dos grupos |
|---|---|---|---|
| 0,002 s | 0,029 s | 0,068 s | 1.072 a 1.610 (holdings); 1 a 23 (sorteadas) |

Segunda rodada (cache quente): máximo 0,006 s.

## Ressalvas

- A base é só a Receita. O grafo real da etapa 1 terá doações, mandatos e
  histórico, e ficará mais denso. Caminhos, vizinhos e busca devem ser
  medidos de novo com essas bases.
- Pessoas foram identificadas por (nome normalizado, 6 dígitos do CPF). Isso
  ainda funde homônimos que têm o mesmo fragmento e separa a mesma pessoa se
  o nome vier grafado de outro jeito.
- Sócios estrangeiros não têm documento, e homônimos estrangeiros foram
  fundidos.
- Todos os tempos foram medidos com uma consulta por vez, sem carga
  concorrente.

## Busca por nome — segunda medição

Data: 2026-11-11. Rafael decidiu resolver M1 dentro do PostgreSQL com busca
por palavras. As medições abaixo são do **desenho final**, numa base refeita
do zero com o mesmo projeto Compose `geoint-spike`, a mesma configuração e os
mesmos arquivos de `data/raw/receita/2026-10/`. O desenho usa apenas a
extensão `unaccent` e recursos do núcleo do PostgreSQL (`tsvector`, GIN,
btree).

### Como se chegou ao desenho (resumo)

1. **Substring com trigramas** (primeira medição). Falhou: p95 de 2,04 s a
   55,7 s, porque o GIN trigram cruza listas enormes de trigramas comuns.
2. **Palavras (`tsvector`) + nome exato + prefixo do nome inteiro, numa
   consulta só.** Falhou no grupo de prefixos (p95 1,22 s): o `ORDER BY`
   final obrigava a rodar o nível de palavras sempre.
3. **Duas etapas, palavras vazias fora, números de 5+ dígitos fora.**
   Rápido nos 50 termos, mas falhou em termos adversos: 6,7 s em `SERVICOS`
   só em pessoas. Com tabela única e índices parciais, a estatística mistura
   empresas e pessoas, e o planejador escolheu um plano ruim.
4. **Uma tabela por tipo + sem prefixo quando a última palavra é
   frequente.** Todos os tempos abaixo de 1 s, mas o nível "prefixo do nome
   inteiro" vinha em ordem alfabética e trazia resultados ruins primeiro. Para
   `PETROB`, a Petrobras não aparecia entre os 20; para `BANCO DO BRAS`, o
   Banco do Brasil também não (ver tabela de qualidade).
5. **Final: o nível de prefixo alfabético foi retirado.** Primeiro vem o nome
   exato; depois, palavras com prefixo na última palavra, em ordem de grau.

### Qualidade: desenho final × iteração 4

Foram 20 termos: os 10 prefixos digitados e 10 nomes parciais de empresas
(duas ou três palavras do nome, a última incompleta). Para os 13 termos com
uma entidade-alvo conhecida, a tabela dá a posição do alvo nos 20 primeiros.

| Termo | Alvo | Final: sem restrição / empresas | Iteração 4 (prefixo alfabético), sem restrição |
|---|---|---|---|
| `PETROB` | PETROLEO BRASILEIRO S A PETROBRAS | 1 / 1 | fora dos 20 |
| `ITAU UNIB` | ITAU UNIBANCO HOLDING S.A. | 2 / 2 (1º: ITAU UNIBANCO S.A.) | 17 |
| `ODEBRE` | empresa ODEBRECHT… | 3 / 1 (sem restrição: 2 pessoas antes) | 1 |
| `BRASILEIRO PETROB` | Petrobras | 1 / 1 | 1 |
| `BANCO DO BRAS` | BANCO DO BRASIL SA | 2 / 2 (1º: BANCO SANTANDER (BRASIL) S.A.) | fora dos 20 |
| `UNIBANCO HOLD` | ITAU UNIBANCO HOLDING S.A. | 1 / 1 | 3 |
| `COMPANHIA SIDERURGICA NAC` | COMPANHIA SIDERURGICA NACIONAL | 1 / 1 | 1 |
| `TELEFONICA BRAS` | TELEFONICA BRASIL S.A. | 1 / 1 | 1 |
| `MAGAZINE LUI` | MAGAZINE LUIZA S/A | 1 / 1 | 7 |
| `NATURA COSM` | NATURA COSMETICOS S/A | 1 / 1 | 1 |
| `GERDAU ACO` | GERDAU ACOS LONGOS S.A. | 2 / 2 (1º: COMERCIAL GERDAU ACOS PLANOS, mesmo grau 14) | 4 |
| `CAIXA ECONOMICA FED` | CAIXA ECONOMICA FEDERAL | 1 / 1 | 1 |
| `RAIA DROG` | RAIA DROGASIL S/A | 1 / 1 | 2 |

**Desenho final:** o alvo está nos 20 primeiros em 13 de 13 termos, nos
escopos "sem restrição" e "empresas". É o 1º em 9 de 13 sem restrição e em
10 de 13 em empresas. Quando não é o 1º, o que vem antes tem grau maior ou
igual: outras empresas ou, em `ODEBRE` sem restrição, pessoas com esse
sobrenome.

**Iteração 4**, medida na mesma base final: o alvo ficou fora dos 20 em 2
dos 13 termos.

Nos 7 termos genéricos (sem alvo único), comparei o maior grau entre os 20
resultados da iteração 4 com o grau do 1º resultado do desenho final:

| Termo | Iteração 4 (maior grau entre os 20) | Final (grau do 1º) |
|---|---|---|
| `OLIVEIRA SANT` | 3 | 67 |
| `JOSE DA SIL` | 5 | 188 |
| `CONSTRUT` | 2 | 451 |
| `COMERCIO DE ALIM` | 5 | 133 |
| `MARIA APARECIDA DA S` | 8 | 22 |
| `FRANCISCO DAS CHA` | 2 | 30 |
| `ASSOCIACAO DE MOR` | 1 (nome exato) | 1 (nome exato), depois 25 |

**Nome exato.** Os mesmos 10 nomes exatos existentes da medição anterior
foram rodados de novo no desenho final: 6 empresas conhecidas (PETROLEO
BRASILEIRO S A PETROBRAS, BANCO DO BRASIL SA, ITAU UNIBANCO HOLDING S.A.,
VALE S.A., HASEGAWA TRAVEL TURISMO LTDA, mais 1 empresário individual cujo
nome é omitido) e 4 nomes de pessoa, também omitidos. Dois desses nomes de
pessoa são muito comuns: um tem 1.972 pessoas e 462 empresas com o nome
exato, o outro tem 1.473 e 402. Em 10 de 10, sem restrição e no escopo do
próprio tipo, a 1ª posição é uma entidade com o nome exato. Para os dois
nomes muito comuns, os 20 resultados são todos homônimos exatos, escolhidos
por grau. Tempo máximo: 0,078 s.

**Conferência da ordem.** Para cada um dos 80 termos (70 + 10 adversos) nos
3 escopos, o desenho final devolveu **exatamente** as mesmas 20 entidades, na
mesma ordem, que uma consulta de referência. A referência ordena todas as
correspondências por `search_rank` antes do `LIMIT`. Foram 240 de 240 listas
idênticas, em duas rodadas. A referência é lenta (até 4,30 s), e por isso não
é o desenho.

Os 5 primeiros resultados por escopo (desenho final, cache quente) estão
abaixo, com o grau entre parênteses. Pessoas físicas e empresas cujo nome é
nome de pessoa aparecem só com o grau, porque o nome é dado pessoal.
Empresário individual, produtor rural, candidato, empresa individual
imobiliária e sociedade unipessoal de advocacia contam como empresas com
nome de pessoa. Nos quatro termos que são nomes de pessoa, todas as empresas
aparecem só com o grau.

**`OLIVEIRA SANT`** (`'oliveira' & 'sant':*`)
- sem restrição (20 resultados): [pessoa, grau 67]; [empresa, grau 35]; [pessoa, grau 33]; [pessoa, grau 32]; [pessoa, grau 29]
- empresas (20 resultados): [empresa, grau 35]; [empresa, grau 14]; [empresa com nome de pessoa, grau 14]; [empresa, grau 9]; [empresa, grau 8]
- pessoas (20 resultados): [pessoa, grau 67]; [pessoa, grau 33]; [pessoa, grau 32]; [pessoa, grau 29]; [pessoa, grau 27]

**`JOSE DA SIL`** (`'jose' & 'sil':*`)
- sem restrição (20 resultados): [pessoa, grau 188]; [pessoa, grau 93]; [pessoa, grau 78]; [pessoa, grau 64]; [pessoa, grau 60]
- empresas (20 resultados): [empresa, grau 27]; [empresa com nome de pessoa, grau 12]; [empresa com nome de pessoa, grau 11]; [empresa com nome de pessoa, grau 10]; [empresa com nome de pessoa, grau 10]
- pessoas (20 resultados): [pessoa, grau 188]; [pessoa, grau 93]; [pessoa, grau 78]; [pessoa, grau 64]; [pessoa, grau 60]

**`PETROB`** (`'petrob':*`)
- sem restrição (20 resultados; alvo na posição 1): PETROLEO BRASILEIRO S A PETROBRAS (308); FUNDACAO PETROBRAS DE SEGURIDADE SOCIAL PETROS (8); PETROBRAS INTERNACIONAL SA BRASPETRO (8); PETROBOM INDUSTRIA E COMERCIO DE PAES E PRODUTOS ALIMENTICIOS LTDA (7); PETROBRAS TRANSPORTE S.A - TRANSPETRO (7)
- empresas (20 resultados; alvo na posição 1): PETROLEO BRASILEIRO S A PETROBRAS (308); FUNDACAO PETROBRAS DE SEGURIDADE SOCIAL PETROS (8); PETROBRAS INTERNACIONAL SA BRASPETRO (8); PETROBOM INDUSTRIA E COMERCIO DE PAES E PRODUTOS ALIMENTICIOS LTDA (7); PETROBRAS TRANSPORTE S.A - TRANSPETRO (7)
- pessoas (3 resultados): [pessoa, grau 2]; [pessoa, grau 1]; [pessoa, grau 1]

**`ITAU UNIB`** (`'itau' & 'unib':*`)
- sem restrição (20 resultados; alvo na posição 2): ITAU UNIBANCO S.A. (220); ITAU UNIBANCO HOLDING S.A. (76); ITAU UNIBANCO VEICULOS ADMINISTRADORA DE CONSORCIOS LTDA. (14); FRANCO-NEVADA CORPORATION - ITAU UNIBANCO S.A. (11); ITAU UNIBANCO ASSET MANAGEMENT LTDA (11)
- empresas (20 resultados; alvo na posição 2): ITAU UNIBANCO S.A. (220); ITAU UNIBANCO HOLDING S.A. (76); ITAU UNIBANCO VEICULOS ADMINISTRADORA DE CONSORCIOS LTDA. (14); FRANCO-NEVADA CORPORATION - ITAU UNIBANCO S.A. (11); ITAU UNIBANCO ASSET MANAGEMENT LTDA (11)
- pessoas (0 resultados): nenhum

**`CONSTRUT`** (`'construt':*`)
- sem restrição (20 resultados): EVEN CONSTRUTORA E INCORPORADORA S/A (451); CURY CONSTRUTORA E INCORPORADORA S.A. (283); CONSTRUTORA METROCASA SA (230); PACAEMBU CONSTRUTORA S.A. (195); ALYA CONSTRUTORA S/A (146)
- empresas (20 resultados): EVEN CONSTRUTORA E INCORPORADORA S/A (451); CURY CONSTRUTORA E INCORPORADORA S.A. (283); CONSTRUTORA METROCASA SA (230); PACAEMBU CONSTRUTORA S.A. (195); ALYA CONSTRUTORA S/A (146)
- pessoas (2 resultados): [pessoa, grau 1]; [pessoa, grau 1]

**`COMERCIO DE ALIM`** (`'comercio' & 'alim':*`)
- sem restrição (20 resultados): GENNIUS STORES COMERCIO DE ALIMENTOS LTDA (133); KICHIPS - INDUSTRIA E COMERCIO PRODUTOS ALIMENTICIOS LTDA (83); CB MACEIO COMERCIO DE ALIMENTOS LTDA (39); CB MINAS COMERCIO DE ALIMENTOS LTDA (36); CB BOA VISTA COMERCIO DE ALIMENTOS LTDA (33)
- empresas (20 resultados): GENNIUS STORES COMERCIO DE ALIMENTOS LTDA (133); KICHIPS - INDUSTRIA E COMERCIO PRODUTOS ALIMENTICIOS LTDA (83); CB MACEIO COMERCIO DE ALIMENTOS LTDA (39); CB MINAS COMERCIO DE ALIMENTOS LTDA (36); CB BOA VISTA COMERCIO DE ALIMENTOS LTDA (33)
- pessoas (0 resultados): nenhum

**`MARIA APARECIDA DA S`** (`'maria' & 'aparecida'`)
- sem restrição (20 resultados): [pessoa, grau 22]; [pessoa, grau 22]; [pessoa, grau 19]; [pessoa, grau 18]; [pessoa, grau 17]
- empresas (20 resultados): [empresa com nome de pessoa, grau 15]; [empresa com nome de pessoa, grau 13]; [empresa com nome de pessoa, grau 10]; [empresa com nome de pessoa, grau 10]; [empresa com nome de pessoa, grau 10]
- pessoas (20 resultados): [pessoa, grau 22]; [pessoa, grau 22]; [pessoa, grau 19]; [pessoa, grau 18]; [pessoa, grau 17]

**`FRANCISCO DAS CHA`** (`'francisco' & 'cha':*`)
- sem restrição (20 resultados): [pessoa, grau 30]; [pessoa, grau 18]; [pessoa, grau 17]; [pessoa, grau 16]; [pessoa, grau 14]
- empresas (20 resultados): [empresa, grau 8]; [empresa, grau 4]; [empresa, grau 4]; [empresa, grau 4]; [empresa, grau 4]
- pessoas (20 resultados): [pessoa, grau 30]; [pessoa, grau 18]; [pessoa, grau 17]; [pessoa, grau 16]; [pessoa, grau 14]

**`ODEBRE`** (`'odebre':*`)
- sem restrição (20 resultados; alvo na posição 3): [pessoa, grau 22]; [pessoa, grau 17]; ODEBRECHT REALIZACOES IMOBILIARIAS S.A (16); [pessoa, grau 13]; [pessoa, grau 12]
- empresas (20 resultados; alvo na posição 1): ODEBRECHT REALIZACOES IMOBILIARIAS S.A (16); ODEBRECHT PLANTAS INDUSTRIAIS E PARTICIPACOES S/A (9); ODEBRECHT QUIMICA SA (9); ODEBRECHT SERVICOS DE OLEO E GAS LTDA (9); ODEBRECHT PARCERIAS AGRICOLAS LTDA. (8)
- pessoas (20 resultados): [pessoa, grau 22]; [pessoa, grau 17]; [pessoa, grau 13]; [pessoa, grau 12]; [pessoa, grau 11]

**`ASSOCIACAO DE MOR`** (`'associacao' & 'mor':*`)
- sem restrição (20 resultados): ASSOCIACAO DE MOR (1); ASSOCIACAO DOS MORADORES E PRODUTORES RURAIS DO POVOADO OLHO D'AGUA DO TABUADO (25); ASSOCIACAO DOS PROPRIETARIOS E MORADORES DO RESIDENCIAL CHACARAS RIO VERDINHO APMRV (25); ASSOCIACAO DE MORADORES DA COMUNIDADE RURAL DO VALE DOS IPES (19); ASSOCIACAO DE MORADORES E EMPRESARIOS DA AVENIDA DAS CORUJAS E ADJACENCIAS - AMEC (17)
- empresas (20 resultados): ASSOCIACAO DE MOR (1); ASSOCIACAO DOS MORADORES E PRODUTORES RURAIS DO POVOADO OLHO D'AGUA DO TABUADO (25); ASSOCIACAO DOS PROPRIETARIOS E MORADORES DO RESIDENCIAL CHACARAS RIO VERDINHO APMRV (25); ASSOCIACAO DE MORADORES DA COMUNIDADE RURAL DO VALE DOS IPES (19); ASSOCIACAO DE MORADORES E EMPRESARIOS DA AVENIDA DAS CORUJAS E ADJACENCIAS - AMEC (17)
- pessoas (0 resultados): nenhum

**`BRASILEIRO PETROB`** (`'brasileiro' & 'petrob':*`)
- sem restrição (1 resultados; alvo na posição 1): PETROLEO BRASILEIRO S A PETROBRAS (308)
- empresas (1 resultados; alvo na posição 1): PETROLEO BRASILEIRO S A PETROBRAS (308)
- pessoas (0 resultados): nenhum

**`BANCO DO BRAS`** (`'banco' & 'bras':*`)
- sem restrição (20 resultados; alvo na posição 2): BANCO SANTANDER (BRASIL) S.A. (72); BANCO DO BRASIL SA (43); BANCO ABC BRASIL S.A. (28); BANCO SANTANDER BRASIL S/A (18); CAIXA DE PREVIDENCIA DOS FUNCIONARIOS DO BANCO DO BRASIL - PREVI (18)
- empresas (20 resultados; alvo na posição 2): BANCO SANTANDER (BRASIL) S.A. (72); BANCO DO BRASIL SA (43); BANCO ABC BRASIL S.A. (28); BANCO SANTANDER BRASIL S/A (18); CAIXA DE PREVIDENCIA DOS FUNCIONARIOS DO BANCO DO BRASIL - PREVI (18)
- pessoas (0 resultados): nenhum

**`UNIBANCO HOLD`** (`'unibanco' & 'hold':*`)
- sem restrição (20 resultados; alvo na posição 1): ITAU UNIBANCO HOLDING S.A. (76); UNIBANCO HOLDINGS S A (4); AEGON BRAZIL HOLDING II B.V. - ITAU UNIBANCO S.A. (0); AFFINITY INVESTMENTS HOLDINGS LTD - ITAU UNIBANCO S.A. (0); AIRCASTLE HOLDING CORPORATION LIMITED - ITAU UNIBANCO S.A. (0)
- empresas (20 resultados; alvo na posição 1): ITAU UNIBANCO HOLDING S.A. (76); UNIBANCO HOLDINGS S A (4); AEGON BRAZIL HOLDING II B.V. - ITAU UNIBANCO S.A. (0); AFFINITY INVESTMENTS HOLDINGS LTD - ITAU UNIBANCO S.A. (0); AIRCASTLE HOLDING CORPORATION LIMITED - ITAU UNIBANCO S.A. (0)
- pessoas (0 resultados): nenhum

**`COMPANHIA SIDERURGICA NAC`** (`'companhia' & 'siderurgica' & 'nac':*`)
- sem restrição (3 resultados; alvo na posição 1): COMPANHIA SIDERURGICA NACIONAL (19); ABB/ ELETRO PAINEL - COMPANHIA SIDERURGICA NACIONAL 2010 (4); CAIXA BENEFICENTE DOS EMPREGADOS DA COMPANHIA SIDERURGICA NACIONAL - CBS (3)
- empresas (3 resultados; alvo na posição 1): COMPANHIA SIDERURGICA NACIONAL (19); ABB/ ELETRO PAINEL - COMPANHIA SIDERURGICA NACIONAL 2010 (4); CAIXA BENEFICENTE DOS EMPREGADOS DA COMPANHIA SIDERURGICA NACIONAL - CBS (3)
- pessoas (0 resultados): nenhum

**`TELEFONICA BRAS`** (`'telefonica' & 'bras':*`)
- sem restrição (20 resultados; alvo na posição 1): TELEFONICA BRASIL S.A. (132); TELEFONICA FACTORING DO BRASIL LTDA (6); HORIZON TELEFONICA BRASIL LTDA (4); TELEFONICA CLOUD E TECNOLOGIA DO BRASIL S.A. (4); TELEFONICA DATA BRASIL HOLDING SA (4)
- empresas (20 resultados; alvo na posição 1): TELEFONICA BRASIL S.A. (132); TELEFONICA FACTORING DO BRASIL LTDA (6); HORIZON TELEFONICA BRASIL LTDA (4); TELEFONICA CLOUD E TECNOLOGIA DO BRASIL S.A. (4); TELEFONICA DATA BRASIL HOLDING SA (4)
- pessoas (0 resultados): nenhum

**`MAGAZINE LUI`** (`'magazine' & 'lui':*`)
- sem restrição (20 resultados; alvo na posição 1): MAGAZINE LUIZA S/A (19); CASA ALVES MAGAZINE LUIZOTE LTDA (5); AN ET LUI COSMETICOS MAGAZINE LTDA (2); COOPERATIVA DE CREDITO MUTUO DOS EMPREGADOS DO MAGAZINE LUIZA, EMPRESAS CONTROLADAS E COLIGADAS - COOPLUIZA (2); LOJAS MAGAZINE LUIZA - SUL LTDA (2)
- empresas (20 resultados; alvo na posição 1): MAGAZINE LUIZA S/A (19); CASA ALVES MAGAZINE LUIZOTE LTDA (5); AN ET LUI COSMETICOS MAGAZINE LTDA (2); COOPERATIVA DE CREDITO MUTUO DOS EMPREGADOS DO MAGAZINE LUIZA, EMPRESAS CONTROLADAS E COLIGADAS - COOPLUIZA (2); LOJAS MAGAZINE LUIZA - SUL LTDA (2)
- pessoas (0 resultados): nenhum

**`NATURA COSM`** (`'natura' & 'cosm':*`)
- sem restrição (20 resultados; alvo na posição 1): NATURA COSMETICOS S/A (16); INDUSTRIA E COMERCIO DE COSMETICOS NATURA LTDA (4); ARTE NATURA INDUSTRIA E COMERCIO ARTESANAL DE COSMETICA LTDA (2); CHRISTY BELL IN NATURA-COMERCIO DE COSMETICOS LTDA (2); NATURA MIL COSMETICOS E PRODUTOS NATURAIS LTDA (2)
- empresas (20 resultados; alvo na posição 1): NATURA COSMETICOS S/A (16); INDUSTRIA E COMERCIO DE COSMETICOS NATURA LTDA (4); ARTE NATURA INDUSTRIA E COMERCIO ARTESANAL DE COSMETICA LTDA (2); CHRISTY BELL IN NATURA-COMERCIO DE COSMETICOS LTDA (2); NATURA MIL COSMETICOS E PRODUTOS NATURAIS LTDA (2)
- pessoas (0 resultados): nenhum

**`GERDAU ACO`** (`'gerdau' & 'aco':*`)
- sem restrição (20 resultados; alvo na posição 2): COMERCIAL GERDAU ACOS PLANOS LTDA (14); GERDAU ACOS LONGOS S.A. (14); GERDAU ACOMINAS S/A (13); GERDAU SUMMIT ACOS FUNDIDOS E FORJADOS S.A. (9); GERDAU ACOS ESPECIAIS S.A. (7)
- empresas (20 resultados; alvo na posição 2): COMERCIAL GERDAU ACOS PLANOS LTDA (14); GERDAU ACOS LONGOS S.A. (14); GERDAU ACOMINAS S/A (13); GERDAU SUMMIT ACOS FUNDIDOS E FORJADOS S.A. (9); GERDAU ACOS ESPECIAIS S.A. (7)
- pessoas (0 resultados): nenhum

**`CAIXA ECONOMICA FED`** (`'caixa' & 'economica' & 'fed':*`)
- sem restrição (20 resultados; alvo na posição 1): CAIXA ECONOMICA FEDERAL (42); ASSOCIACAO DO PESSOAL DA CAIXA ECONOMICA FEDERAL DE MINAS GERAIS - APCEF/MG (15); ASSOCIACAO DOS GESTORES DA CAIXA ECONOMICA FEDERAL DE BELO HORIZONTE - AGECEF/BH (9); UNEICEF UNIAO NACIONAL DOS EMPREGADOS E APOSENTADOS DA CAIXA ECONOMICA FEDERAL (6); COOPERATIVA DE CREDITO MUTUO DOS EMPREGADOS NA CAIXA ECONOMICA FEDERAL NO ESTADO DE SAO PAULO - APCEFCREDI-SP (3)
- empresas (20 resultados; alvo na posição 1): CAIXA ECONOMICA FEDERAL (42); ASSOCIACAO DO PESSOAL DA CAIXA ECONOMICA FEDERAL DE MINAS GERAIS - APCEF/MG (15); ASSOCIACAO DOS GESTORES DA CAIXA ECONOMICA FEDERAL DE BELO HORIZONTE - AGECEF/BH (9); UNEICEF UNIAO NACIONAL DOS EMPREGADOS E APOSENTADOS DA CAIXA ECONOMICA FEDERAL (6); COOPERATIVA DE CREDITO MUTUO DOS EMPREGADOS NA CAIXA ECONOMICA FEDERAL NO ESTADO DE SAO PAULO - APCEFCREDI-SP (3)
- pessoas (0 resultados): nenhum

**`RAIA DROG`** (`'raia' & 'drog':*`)
- sem restrição (4 resultados; alvo na posição 1): RAIA DROGASIL S/A (19); RAIA DROGASIL FARMACIA DE MANIPULACAO LTDA (3); DROGA RAIA LTDA (2); GREMIO RECREATIVO DOS FUNCIONARIOS DA DROGA RAIA (1)
- empresas (4 resultados; alvo na posição 1): RAIA DROGASIL S/A (19); RAIA DROGASIL FARMACIA DE MANIPULACAO LTDA (3); DROGA RAIA LTDA (2); GREMIO RECREATIVO DOS FUNCIONARIOS DA DROGA RAIA (1)
- pessoas (0 resultados): nenhum

### Tempos do desenho final

Termos medidos:

- **50 de M1**: os mesmos da primeira medição. São 25 fixos (`MARIA`,
  `JOSE DA SILVA`, `JOAO`, `ANA PAULA`, `SOUZA`, `FRANCISCO DAS CHAGAS`,
  `ANTONIO CARLOS`, `OLIVEIRA SANTOS`, `LUIZ INACIO`, `PEREIRA`,
  `PETROBRAS`, `VALE S A`, `ITAU UNIBANCO`, `ODEBRECHT`, `JBS`,
  `CONSTRUTORA`, `COMERCIO DE ALIMENTOS`, `TRANSPORTES`, `PARTICIPACOES`,
  `HOLDING`, `AGROPECUARIA`, `IGREJA`, `ASSOCIACAO DE MORADORES`,
  `FUNDO DE INVESTIMENTO`, `LTDA`) e 25 sorteados de nomes reais (as duas
  primeiras palavras de nós sorteados; não listados, por serem dados
  pessoais).
- **10 prefixos digitados**: `OLIVEIRA SANT`, `JOSE DA SIL`, `PETROB`,
  `ITAU UNIB`, `CONSTRUT`, `COMERCIO DE ALIM`, `MARIA APARECIDA DA S`,
  `FRANCISCO DAS CHA`, `ODEBRE`, `ASSOCIACAO DE MOR`.
- **10 nomes parciais**: os 10 da tabela de qualidade, de
  `BRASILEIRO PETROB` a `RAIA DROG`.
- **10 adversos**, extra, para forçar palavras frequentes: `SILVA`,
  `SANTOS`, `SIL`, `OLIVEIRA SILVA SANTOS`, `COMERCIO E SERVICOS`,
  `ELEICAO 2016 VEREADOR`, `JOSE MARIA DA SILVA SANTOS`, `AB`, `DE SOUZA`,
  `SERVICOS`.

Cada termo foi rodado em 3 escopos: sem restrição, só pessoas, só empresas.
O tempo de cada busca inclui três coisas: a normalização (uma ida ao banco),
a etapa A e, quando necessária, a etapa B. A lista de palavras frequentes foi
carregada uma vez, no início do programa, e fica fora do tempo.

- **Frio**: para cada escopo, reinício limpo do PostgreSQL
  (`docker stop -t 600`), leitura de cerca de 25 GB de outras tabelas (para
  empurrar os arquivos de busca para fora do cache do sistema operacional),
  novo reinício e uma passada. Sem root não dá para limpar o cache do
  sistema operacional, então "frio" é uma aproximação. Nenhum desligamento
  sujo.
- **Quente**: segunda passada seguida.

| Grupo | Escopo | Frio p50 / p95 / máx. (s) | Quente p50 / p95 / máx. (s) |
|---|---|---|---|
| 50 de M1 | sem restrição | 0,008 / 0,074 / 0,107 | 0,002 / 0,056 / 0,080 |
| 50 de M1 | pessoas | 0,003 / 0,018 / 0,040 | 0,001 / 0,011 / 0,018 |
| 50 de M1 | empresas | 0,007 / 0,064 / 0,074 | 0,002 / 0,055 / 0,060 |
| 10 prefixos | sem restrição | 0,013 / 0,405 / 0,405 | 0,012 / 0,396 / 0,396 |
| 10 prefixos | pessoas | 0,002 / 0,072 / 0,072 | 0,001 / 0,056 / 0,056 |
| 10 prefixos | empresas | 0,011 / 0,362 / 0,362 | 0,011 / 0,332 / 0,332 |
| 10 nomes parciais | sem restrição | 0,018 / 0,173 / 0,173 | 0,014 / 0,150 / 0,150 |
| 10 nomes parciais | pessoas | 0,003 / 0,018 / 0,018 | 0,001 / 0,014 / 0,014 |
| 10 nomes parciais | empresas | 0,017 / 0,161 / 0,161 | 0,013 / 0,147 / 0,147 |
| **70 termos × 3 escopos** | todos | **0,006 / 0,092 / 0,405** | **0,002 / 0,080 / 0,396** |
| 10 adversos (extra) | sem restrição | 0,003 / 0,616 / 0,616 | 0,002 / 0,566 / 0,566 |
| 10 adversos (extra) | pessoas | 0,001 / 0,145 / 0,145 | 0,000 / 0,127 / 0,127 |
| 10 adversos (extra) | empresas | 0,002 / 0,473 / 0,473 | 0,001 / 0,443 / 0,443 |

Nenhuma das 240 buscas de cada rodada passou de 1 s, e nenhuma deu erro.

Mais lentas (frio):

- `ASSOCIACAO DE MOR`: 0,405 s sem restrição, 0,362 s em empresas;
- `MAGAZINE LUI`: 0,173 s;
- entre os 50 de M1: `ANA PAULA`, 0,107 s;
- adversos: `OLIVEIRA SILVA SANTOS`, 0,616 s, e
  `JOSE MARIA DA SILVA SANTOS`, 0,435 s.

Comportamentos observados:

- `LTDA` só tem palavras vazias, então devolve apenas o nome exato (1
  empresa).
- `MARIA`, palavra frequente, não ganha prefixo; vem o 1º por grau, uma
  pessoa de grau 2.219.
- `B D` e `AB` casam palavras inteiras.

### Tempo de construção e tamanho (desenho final)

Medido na fase "tabelas de busca" da carga final, a partir de `s_node` e
`s_degree` já carregados:

| Passo | Tempo (s) | Tamanho (bytes) |
|---|---|---|
| `search_ranked` (ordenação de 88.824.654 nomes) | 112,5 | temporária, apagada |
| `search_company_new` (70.517.432 linhas, inclui o cálculo de `tokens`) | 270,2 | 10.261.889.024 |
| `search_person_new` (18.307.222 linhas) | 52,6 | 2.340.945.920 |
| chave primária empresas / pessoas | 113,9 / 30,6 | 1.583.964.160 / 411.230.208 |
| btree `name_norm` empresas / pessoas | 47,7 / 7,4 | 3.919.994.880 / 769.097.728 |
| GIN `tokens` empresas / pessoas | 52,5 / 10,1 | 902.668.288 / 200.687.616 |
| `VACUUM ANALYZE` empresas / pessoas | 24,5 / 1,1 | — |
| `search_frequent_token_new` (462 palavras) e chave primária | 88,6 | 65.536 |
| troca por renomeação (uma transação) | 0,002 | — |
| **Fase inteira** | **819** | **20.390.543.360 (18,99 GiB)**, dos quais 5.792.448.512 (5,39 GiB) são os índices btree + GIN de busca |

### Perdas conhecidas

1. **Substring no meio da palavra não casa.** `BRAS` não acha `PETROBRAS`.
   Casam palavras inteiras e o prefixo da última palavra digitada.
2. **Sem tolerância a erro de digitação** (`OLIVERA` não acha `OLIVEIRA`) e
   sem sinônimos ou abreviações (`CIA` ≠ `COMPANHIA`).
3. **Palavras vazias nunca casam.** Um termo feito só delas (`LTDA`, `DE`)
   devolve apenas nomes exatamente iguais ao termo.
4. **Sem prefixo em palavra frequente.** Se a última palavra digitada está
   em `search_frequent_token`, ela casa só inteira (`MARIA` não acha
   `MARIANA`).
5. **Sem prefixo na última palavra se ela tiver menos de 3 caracteres ou for
   palavra vazia.** Em `MARIA APARECIDA DA S`, o `S` é descartado.
6. **Números de 5 ou mais dígitos não são indexados nem buscados** (CPF no
   nome de empresário individual, CNPJ, telefone). É intencional, por
   privacidade; busca por CNPJ deve ser consulta direta por chave.
7. **A ordem das palavras é ignorada**: `SANTOS OLIVEIRA` = `OLIVEIRA SANTOS`.
8. **A ordem é por grau, não por relevância.** Homônimos em massa enchem os
   20 primeiros com os de maior grau. Exemplo: um nome comum que tem 1.972
   pessoas e 462 empresas com o nome exato. Paginação não foi medida.
9. **Não medido:** consultas concorrentes, buscas durante a reconstrução e
   tabelas particionadas por tipo no lugar de duas tabelas.

### SEARCH_STRATEGY

````text
SEARCH_STRATEGY

1. Definições
   grau(entidade)   = no produto: número de relações abertas da entidade em
                      todas as fontes. Neste teste só havia Sócios: grau =
                      número de registros de Sócios em que a entidade aparece
                      como empresa ou como sócio (s_degree.degree; 0 se nenhum).
   ordem de ranking = grau DESC, name_norm ASC (collation "C", ordem de
                      bytes), entity_id ASC. search_rank = posição nessa
                      ordem, numeração única para empresas e pessoas.
   chave de desempate = name_norm (C) e depois entity_id.
   entity_id        = uuid da entidade (no teste, gen_random_uuid() por nó).

2. Normalização (índice e consulta usam a mesma expressão, como rodada):
   CREATE EXTENSION IF NOT EXISTS unaccent;
   CREATE FUNCTION search_norm(t text) RETURNS text
   LANGUAGE sql IMMUTABLE PARALLEL SAFE
   RETURN nullif(btrim(regexp_replace(upper(public.unaccent(
            'public.unaccent'::regdictionary, coalesce(t, ''))),
            '\s+', ' ', 'g')), '');
   Entidades com search_norm(nome) nulo ficam fora das tabelas de busca
   (677 em 2026-10).

3. Palavras do índice:
   CREATE FUNCTION search_tokens(name_norm text) RETURNS tsvector
   LANGUAGE sql IMMUTABLE PARALLEL SAFE
   RETURN array_to_tsvector(ARRAY(
     SELECT DISTINCT t
     FROM unnest(array_remove(regexp_split_to_array(lower(name_norm),
                 '[^a-z0-9]+'), '')) AS t
     WHERE t NOT IN ('de','da','do','das','dos','e','a','o','em',
                     'ltda','me','epp','eireli','s','sa')
       AND t !~ '^[0-9]{5,}$'));
   (palavras vazias: as 15 da lista; números de 5+ dígitos: fora)

4. Construção completa (sempre do zero; ver item 9). SQL como rodado:
   SET maintenance_work_mem = '2GB'; SET work_mem = '1GB';
   CREATE UNLOGGED TABLE search_ranked AS
     SELECT row_number() OVER (ORDER BY coalesce(d.degree, 0) DESC,
              n.name_norm COLLATE "C", n.entity_id) AS search_rank,
            n.entity_id, n.kind, n.name_norm COLLATE "C" AS name_norm,
            coalesce(d.degree, 0) AS grau
     FROM s_node n LEFT JOIN s_degree d ON d.id = n.id
     WHERE n.name_norm IS NOT NULL;
   CREATE TABLE search_company_new AS
     SELECT search_rank, entity_id, name_norm, grau,
            search_tokens(name_norm) AS tokens
     FROM search_ranked WHERE kind = 'C' ORDER BY search_rank;
   CREATE TABLE search_person_new AS
     SELECT search_rank, entity_id, kind, name_norm, grau,
            search_tokens(name_norm) AS tokens
     FROM search_ranked WHERE kind <> 'C' ORDER BY search_rank;
     -- kind: 'P' pessoa física, 'E' estrangeiro
   DROP TABLE search_ranked;
   (O ORDER BY search_rank no CREATE TABLE AS é obrigatório: a etapa B
    depende de a tabela estar gravada fisicamente nessa ordem.)
   Colunas resultantes: search_rank bigint, entity_id uuid, [kind "char"],
   name_norm text COLLATE "C", grau integer, tokens tsvector.

5. Índices (DDL como rodada):
   ALTER TABLE search_company_new ADD CONSTRAINT search_company_new_pkey
     PRIMARY KEY (search_rank);
   ALTER TABLE search_person_new ADD CONSTRAINT search_person_new_pkey
     PRIMARY KEY (search_rank);
   CREATE INDEX search_company_new_name ON search_company_new (name_norm);
   CREATE INDEX search_person_new_name  ON search_person_new  (name_norm);
   CREATE INDEX search_company_new_tokens ON search_company_new
     USING gin (tokens);
   CREATE INDEX search_person_new_tokens  ON search_person_new
     USING gin (tokens);
   VACUUM ANALYZE search_company_new;
   VACUUM ANALYZE search_person_new;
   Uma tabela por tipo é obrigatória: com tabela única e índices parciais,
   a estatística mistura os tipos e gerou planos de 5 a 7 s.

6. Palavras frequentes (como rodado: limiar sobre a SOMA das duas tabelas):
   CREATE TABLE search_frequent_token_new AS
     SELECT word AS token, sum(ndoc)::bigint AS ndoc
     FROM (SELECT word, ndoc FROM ts_stat('SELECT tokens FROM search_company_new')
           UNION ALL
           SELECT word, ndoc FROM ts_stat('SELECT tokens FROM search_person_new')) s
     GROUP BY word HAVING sum(ndoc) >= 100000;
   ALTER TABLE search_frequent_token_new
     ADD CONSTRAINT search_frequent_token_new_pkey PRIMARY KEY (token);
   (462 palavras em 2026-10)

7. Troca (uma transação):
   BEGIN;
   DROP TABLE IF EXISTS search_company, search_person, search_frequent_token;
   ALTER TABLE search_company_new RENAME TO search_company;
   ALTER TABLE search_person_new RENAME TO search_person;
   ALTER TABLE search_frequent_token_new RENAME TO search_frequent_token;
   ALTER INDEX search_company_new_pkey RENAME TO search_company_pkey;
   ALTER INDEX search_person_new_pkey RENAME TO search_person_pkey;
   ALTER INDEX search_frequent_token_new_pkey
     RENAME TO search_frequent_token_pkey;
   ALTER INDEX search_company_new_name RENAME TO search_company_name;
   ALTER INDEX search_person_new_name RENAME TO search_person_name;
   ALTER INDEX search_company_new_tokens RENAME TO search_company_tokens;
   ALTER INDEX search_person_new_tokens RENAME TO search_person_tokens;
   COMMIT;

8. Cache de palavras frequentes na aplicação:
   SELECT token FROM search_frequent_token;  -- carregar num conjunto em
   memória ao iniciar e depois de cada reconstrução. Não é ida ao banco por
   busca. No teste foi carregado uma vez no início, fora dos tempos medidos.

9. Sincronização: as tabelas de busca são dados derivados, reconstruídos
   POR INTEIRO (itens 4 a 7) ao fim de toda coleta de qualquer fonte. Não há
   atualização incremental, então a ordem física nunca se degrada.
   Tempo medido da reconstrução: 819 s (88,8 milhões de nomes); a troca
   levou 0,002 s (2,3 ms somando os comandos da transação).

10. Sessão de busca (parâmetros usados nas medições):
   SET plan_cache_mode = force_custom_plan;
   SET max_parallel_workers_per_gather = 0;
   SET synchronize_seqscans = off;
   (Sem paralelismo e sem varredura sincronizada, a etapa B lê a tabela do
    início e na ordem física, que é a ordem de search_rank.)

11. Preparo do termo:
   a. q = SELECT search_norm($termo)   -- ida ao banco, incluída nos tempos
      Se q for nulo, ou tiver menos de 2 caracteres sem contar espaços:
      devolver lista vazia.
   b. palavras digitadas = regexp_split_to_array(lower(q), '[^a-z0-9]+')
      sem elementos vazios, na ordem digitada.
   c. palavras mantidas = digitadas menos palavras vazias (lista do item 3)
      e menos números de 5+ dígitos (^[0-9]{5,}$).
   d. tsq = palavras mantidas, cada uma entre aspas simples, unidas por
      ' & '. A ÚLTIMA palavra DIGITADA recebe ':*' só se foi mantida, tem
      >= 3 caracteres e não está no cache de palavras frequentes.
      Se nenhuma palavra foi mantida: tsq vazio, e não há etapa B.
   As palavras só têm [a-z0-9], então não há aspas a escapar no tsquery.

12. Etapa A, nome exato (sempre). $1 = q.
   Sem restrição de tipo:
     SELECT * FROM (
       (SELECT search_rank, entity_id, 'C'::"char" AS kind, name_norm, grau
          FROM search_company WHERE name_norm = $1
         ORDER BY search_rank LIMIT 20)
       UNION ALL
       (SELECT search_rank, entity_id, kind, name_norm, grau
          FROM search_person WHERE name_norm = $1
         ORDER BY search_rank LIMIT 20)
     ) r ORDER BY search_rank LIMIT 20;
   Só empresas:
     SELECT search_rank, entity_id, 'C'::"char" AS kind, name_norm, grau
       FROM search_company WHERE name_norm = $1
      ORDER BY search_rank LIMIT 20;
   Só pessoas:
     SELECT search_rank, entity_id, kind, name_norm, grau
       FROM search_person WHERE name_norm = $1
      ORDER BY search_rank LIMIT 20;

13. Etapa B, palavras (só se a etapa A devolveu n < 20 linhas e tsq não é
   vazio). $1 = tsq, $2 = q, $3 = 20 - n.
   Sem restrição de tipo:
     SELECT * FROM (
       (SELECT search_rank, entity_id, 'C'::"char" AS kind, name_norm, grau
          FROM search_company
         WHERE tokens @@ $1::tsquery AND name_norm <> $2 LIMIT $3)
       UNION ALL
       (SELECT search_rank, entity_id, kind, name_norm, grau
          FROM search_person
         WHERE tokens @@ $1::tsquery AND name_norm <> $2 LIMIT $3)
     ) r ORDER BY search_rank LIMIT $3;
   Só empresas:
     SELECT search_rank, entity_id, 'C'::"char" AS kind, name_norm, grau
       FROM search_company
      WHERE tokens @@ $1::tsquery AND name_norm <> $2 LIMIT $3;
   Só pessoas:
     SELECT search_rank, entity_id, kind, name_norm, grau
       FROM search_person
      WHERE tokens @@ $1::tsquery AND name_norm <> $2 LIMIT $3;
   NÃO pôr ORDER BY search_rank dentro dos ramos. O LIMIT corta a leitura
   cedo, e as linhas já saem em ordem de search_rank, porque a tabela está
   gravada nessa ordem e é lida do início (item 10). Na referência com
   ORDER BY interno, o resultado foi o mesmo em 240 de 240 casos, mas o
   tempo chegou a 4,30 s.

14. Resultado = linhas da etapa A seguidas das linhas da etapa B, no máximo
   20, cada uma com entity_id, kind, name_norm e grau.
   Ordem final: nomes exatos primeiro; dentro de cada etapa, grau DESC,
   name_norm ASC (C) e entity_id ASC, que é a ordem de search_rank. Sem
   restrição de tipo, empresas e pessoas se misturam por search_rank.

15. Tamanho mínimo do termo: 2 caracteres, sem contar espaços, depois da
   normalização. Prefixo só na última palavra digitada, com 3 ou mais
   caracteres.
````

M1: PASS
