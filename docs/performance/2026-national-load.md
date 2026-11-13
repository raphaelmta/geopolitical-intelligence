# Carga nacional e metas da etapa 1 (medição de 2026-11-12 e 13)

## Resumo em uma página

**Uma meta FALHOU: a carga nacional da Receita levou 26 h 20 min, e a meta é de no máximo 12 h.** Mesmo sem contar a resolução de identidades e a reconstrução da busca, a carga da Receita propriamente dita (baixar, ler e gravar) levou 11 h 32 min. Por isso a etapa 1 **não** está concluída do ponto de vista das metas; a decisão é do Rafael (veja "O que decidir", abaixo).

O que foi carregado, numa pilha separada (`geoint-national`, volumes novos), uma fonte de cada vez:

| Fonte | Duração total | O que entrou |
|---|---|---|
| Receita Federal, edição 2026-10 (publicada em 2026-10-13) | **26 h 20 min** | 172,8 milhões de linhas lidas, 0 descartadas; 70,5 milhões de empresas, 27,9 milhões de fichas de pessoa, 50,6 milhões de vínculos |
| TSE (eleições 2018 a 2026) | 2 h 53 min | 6,75 milhões de linhas, 0 descartadas; 3,0 milhões de fichas, 5,5 milhões de vínculos |
| Câmara (legislaturas 55 a 57) | 1 h 03 min | 1.252 deputados, 4.358 mandatos |
| Senado (legislaturas 55 a 57) | 46 min | 740 linhas, 660 mandatos |
| CVM (2025 e 2026) | 45 min | 708 companhias, 9.997 vínculos de controle e participação |

As cinco bases terminaram "em dia" e sem nenhuma coleta falha. Banco: **95,7 GB** (mais 32 GB de WAL); arquivos brutos: 11,7 GB.

Metas de consulta (pela API autenticada, como a Bancada usa), **todas cumpridas**, com folga:

| Meta | Alvo | Primeira passada após reinício (p95 / pior) | Segunda passada (p95 / pior) | Resultado |
|---|---|---|---|---|
| Busca por nome ou CNPJ (M1, 80 termos × 3 escopos) | p95 ≤ 1 s | 0,098 s / 0,420 s | 0,078 s / 0,428 s | passou |
| Página de entidade (220 entidades) | p95 ≤ 1 s | 0,013 s / 0,572 s | 0,008 s / 0,009 s | passou |
| Vizinhos, primeira página (M2, 220 entidades) | p95 ≤ 2 s | 0,213 s / 0,512 s | 0,015 s / 0,028 s | passou |
| Caminhos até profundidade 6 (M3, 31 pares) | cada ≤ 120 s | pior 20,0 s | pior 1,24 s | passou |
| Grupo de controle, dois modos (M4, 20 empresas) | cada ≤ 120 s | pior 8,8 s | pior 0,58 s | passou |
| Carga nacional da Receita | ≤ 12 h | **26 h 20 min** | — | **FALHOU** |

Dos **7 critérios de sucesso da seção 2** do desenho, conferidos um a um na base nacional: **4 passaram, 1 em parte** (o comando único de carga é um comando por fonte), **1 falhou** (a meta de tempo da carga, acima) e **1 não pôde ser verificado** (vínculo encerrado com período: precisa de uma segunda edição da Receita, que ainda não saiu). Duas verificações adicionais (fato e inferência; identidades) estão à parte e não entram nessa conta. Detalhes abaixo.

### O que decidir

1. **A meta de 12 h da Receita.** Onde o tempo foi: gravar os 50,6 milhões de vínculos (8 h 24 min, cada vez mais lento à medida que a tabela cresce), resolver identidades (14 h 07 min) e reconstruir a busca (41 min). As saídas possíveis, nenhuma feita nesta tarefa: (a) guardar o banco num volume sem cópia-na-escrita (o disco desta máquina é btrfs com compressão; veja "Ambiente"), (b) gravar os vínculos em paralelo (a carga usa cerca de 1 núcleo de 6), (c) criar os índices da tabela de vínculos depois da carga inicial, (d) refazer a resolução de identidades da primeira carga como uma operação em lote, sem reapontar vínculo por vínculo. Ou (e) aceitar a meta como "carga inicial em até 30 h, cargas mensais medidas à parte". A recarga mensal **não foi medida** (a edição 2026-11 ainda não saiu).
2. **O limiar de junção automática (hoje 100).** Com 100, a resolução uniu sozinha 5,79 milhões de grupos de fichas (soma dos contadores das cinco coletas) e deixou **42.716 propostas** na fila do analista. Quantos desses grupos juntam, por engano, duas pessoas diferentes é uma **estimativa de ordem de grandeza: dezenas a poucas centenas de pares de fichas** (cálculo e hipóteses em "Identidade"; não é um teto). Subir para 300 corta a fila para cerca de 14.500 e aumenta essa estimativa em cerca de 70 %.
3. **A fila de identidades** abre em 3,3 s na primeira página (não há meta para ela, mas é a tela mais lenta medida).

## Ambiente

| Item | Valor |
|---|---|
| Datas | carga de 2026-11-12 09:10 UTC a 2026-11-13 17:01 UTC; medições em 2026-11-13 17:16–17:30 UTC |
| Código | commits `067fbf2` (imagens usadas na carga; o código Go é o mesmo de `72d6f69`) e `f38de51` (ferramenta de medição) |
| Máquina | 6 núcleos, 31 GB de memória, 7 GB de swap, NVMe de 929 GB (727 GB livres no início), Fedora |
| Sistema de arquivos do Docker | **btrfs**, `rw,relatime,compress=zstd:1,ssd,discard=async,space_cache=v2`, com cópia-na-escrita (CoW). **Todos os tempos abaixo foram medidos sob btrfs com CoW e compressão zstd.** |
| PostgreSQL | 18.6, perfil de 31 GB de [`deploy/TUNING.md`](../../deploy/TUNING.md): shared_buffers 8GB, effective_cache_size 20GB, maintenance_work_mem 2GB, work_mem 64MB, max_wal_size 32GB, checkpoint_timeout 30min, random_page_cost 1.1, effective_io_concurrency 200, paralelismo 6/4/4, max_connections 60, autovacuum 4 workers / 1GB / cost_limit 2000, jit off, wal_compression lz4, fsync/synchronous_commit/full_page_writes ligados; `/dev/shm` 12 GB (`PG_SHM_SIZE=12g`) |
| Pilha | projeto `geoint-national`, `.env.national` próprio, `SCHEDULER_ENABLED=false`, limiar de identidade 100 (padrão); só coletas pedidas à mão; Receita fixada com `--ref 2026-10-31` |
| Outros contêineres | `smartcheck-mongodb` rodou durante o download e a leitura da Receita (09:10–09:49 UTC de 2026-11-12) e foi parado a pedido do dono; depois disso, só a pilha medida. Memória disponível no início: 21 GB |
| Uso da máquina durante a carga | a carga usa um processo do PostgreSQL de cada vez (cerca de 1 núcleo de 6; CPU ~72 % ociosa), limitada por E/S (iowait até ~38 %); swap do host em uso ~6,4 GB |

## Carga por fonte

### Receita Federal (edição 2026-10)

| Fase | Duração | Detalhe |
|---|---|---|
| Download (36 arquivos, 7,3 GB) | 23 min 34 s | ≈ 5,2 MB/s (o ensaio levou 37 min) |
| Leitura e cópia para as tabelas de trabalho | 2 min 35 s | 172.822.778 linhas, 0 descartadas |
| Estatísticas das tabelas de trabalho | 17 s | |
| Empresas | 1 h 55 min 38 s | 70.517.438 entidades em 7 min 47 s; **70.517.437 versões cadastrais em 1 h 37 min 16 s** |
| Pessoas | 41 min 54 s | 27.947.247 fichas (gravação 37 min 59 s); 12.763 estrangeiros |
| Sócios | 3 min 31 s | 28.495.961 linhas preparadas |
| **Vínculos** (1.024 blocos) | **8 h 23 min 45 s** | 50.636.180 vínculos; ver abaixo |
| ANALYZE | 25 s | |
| Fechamento do que sumiu | 0 s | primeira carga: nada a fechar |
| **Resolução de identidades** | **14 h 06 min 53 s** | 797 lotes de 20.000 nomes, 64 s por lote, constante do início ao fim |
| Reconstrução da busca | 41 min 24 s | 70,5 milhões de empresas e 18,4 milhões de pessoas |
| **Total** | **26 h 20 min 01 s** | do pedido (09:10:02) ao fim (11:30:03 do dia seguinte) |

**Os vínculos ficam mais caros à medida que a tabela cresce.** Os 1.024 blocos têm o mesmo tamanho (cerca de 3,16 milhões de vínculos a cada 64 blocos), mas cada grupo de 64 levou, em minutos: 18,3; 17,7; 17,9; 19,2; 22,1; 25,5; 29,2; 31,8; 37,0; 38,5; 38,3; 37,2; 39,9; 41,0; 44,6; 45,5. O custo por vínculo cresceu cerca de 2,5 vezes: cada vínculo novo entra em todos os índices da tabela (inclusive o índice único da chave natural), e esses índices passam do tamanho da memória; cada inserção vira leitura e escrita aleatória no disco, que aqui é btrfs com CoW.

**Onde a resolução de identidades gasta o tempo.** Com o registro de comandos lentos ligado durante a resolução do TSE (mesmo código), 46 % do tempo foi o "reapontamento" dos vínculos de quem foi unido (`UPDATE relationship … SET src_entity_id`, cerca de 27 s por lote para ~20.000 linhas): cada linha alterada muda uma coluna indexada, então entra de novo em todos os índices da tabela de 50 milhões de linhas e dispara o gatilho de integridade. Não é um plano de consulta errado (os planos usam índices); é o custo de mexer em dezenas de milhões de linhas uma a uma. Não há correção pequena: é uma mudança de desenho (ver "Riscos e recomendações").

Picos durante a Receita: banco 89,4 GB ao fim; WAL no teto de 32 GB durante as gravações; memória do contêiner do PostgreSQL até 16,3 GiB; `/dev/shm` usado: até 2 MB nas amostras de minuto (7 MB no máximo em toda a carga das cinco fontes; o teto de 12 GB não foi necessário); disco usado em `/` de 198,7 para 281,6 GB.

**Uma busca de caminho durante a carga** (vigilância de esgotamento de conexões): foram feitos só dois pedidos de caminhos. Durante a leitura, um foi atendido em 19 ms, mas respondeu "entidade não encontrada" (as tabelas ainda estavam vazias). Durante a gravação de vínculos, entre entidades reais, outro levou 71 ms. Nesses dois momentos nada esperou por conexão. Não houve pedido durante as 14 h da resolução de identidades nem durante a reconstrução da busca.

### Demais fontes

| Fonte | Download/consulta | Leitura e gravação | Identidades | Busca | Total | Linhas lidas / descartadas |
|---|---|---|---|---|---|---|
| TSE | 2 min 19 s (10 arquivos, 4,0 GB, guardados pelo ETag/data do servidor) | 23 min 13 s (vínculos 14 min 25 s: 5.507.135) | 1 h 44 min 57 s | 43 min 32 s | 2 h 53 min 21 s | 6.754.243 / 0 |
| Câmara | ≈ 22 min 45 s (API, ≈ 4.000 requisições) | 42 s | 14 s | 39 min 19 s | 1 h 03 min 01 s | 3.084 / 0 |
| Senado | ≈ 4 min 44 s (6 respostas 503 passageiras, repetidas uma vez pelo conector) | 11 s | 3 s | 40 min 47 s | 45 min 44 s | 740 / 0 |
| CVM | < 1 s (2 arquivos, 16,7 MB) | 48 s | 51 s | 43 min 16 s | 44 min 55 s | 63.079 / 0 |

A reconstrução da busca é completa a cada coleta: cerca de 40 minutos, mesmo para uma fonte de 740 linhas. Durante esse tempo a busca serve as tabelas anteriores.

**Segunda coleta da Receita, mesma edição:** "nada novo" em 0,6 s, nenhuma coleta nova criada, `source_check` atualizado. Passou.

## Tamanho do banco e crescimento esperado

| Item | Tamanho |
|---|---|
| Banco (pg_database_size) | 95,7 GB |
| Vínculos (`relationship`, com índices) | 24,5 GB |
| Versões cadastrais (`company_version`) | 19,0 GB |
| Entidades (`entity`) | 18,9 GB |
| Busca, empresas / pessoas | 15,3 GB / 3,8 GB |
| Fichas de pessoa (`person_record`) | 13,0 GB |
| Decisões automáticas de identidade | 1,2 GB |
| WAL | até 32 GB (o teto) |
| Volume do banco (dados + WAL) | 129,8 GB |
| Arquivos brutos (MinIO) | 11,7 GB: Receita 7,15 GB, TSE 3,96 GB, CVM 16 MB, Câmara 9 MB (2.542 respostas), Senado 1 MB |
| Disco usado em `/` | de 198,7 GB para 290 GB ao fim; pico de 310,7 GB durante uma reconstrução da busca (btrfs comprime, então o `df` mostra menos que a soma dos volumes) |

Contagens: entidades 70.517.438 empresas, 30.940.643 pessoas (das quais 20.284.429 têm fichas; as demais ficaram vazias quando suas fichas foram unidas a outra pessoa), 12.986 estrangeiros, 53 partidos, 2 órgãos. Fichas de pessoa: Receita 27.947.247, TSE 2.988.284, CVM 3.630, Câmara 1.251, Senado 231. Vínculos: sócio 28.495.653, administrador 21.378.974, representante legal 761.553, doação 4.405.346, candidatura 1.101.789, mandato Câmara 517 abertos e 3.841 encerrados, mandato Senado 84 e 576, participação CVM 6.571, controle CVM 3.426. Versões cadastrais: 70.517.437, todas abertas.

Crescimento esperado: cada edição mensal da Receita guarda mais 7,3 GB de arquivos brutos e regrava `last_seen` em dezenas de milhões de linhas (linhas mortas que o autovacuum recicla; não medido). O TSE ainda guarda cerca de 4 GB por semana enquanto regerar os arquivos toda semana ([`docs/sources/tse.md`](../sources/tse.md)).

## Identidade

Pessoas são unidas automaticamente só quando têm o **mesmo nome e o mesmo trecho de CPF**, e o nome não é comum demais (até 100 trechos diferentes para o mesmo nome). Acima disso, o grupo vira proposta para um analista.

| Nomes por quantidade de trechos de CPF diferentes | Nomes | Fichas |
|---|---|---|
| nenhum trecho | 161.064 | 161.422 |
| 1 | 16.138.115 | 25.431.727 |
| 2 a 10 | 876.782 | 3.791.627 |
| 11 a 50 | 37.151 | 1.023.145 |
| 51 a 100 | 2.425 | 238.822 |
| 101 a 300 | 902 | 198.456 |
| 301 a 1.000 | 109 | 75.678 |
| mais de 1.000 | 10 | 19.766 |

Resultado com o limiar 100 (contadores da resolução): Receita 5.040.521 grupos unidos (9.565.982 fichas movidas) e 37.051 propostas; TSE 749.944 grupos (1.085.811 fichas) e 5.664 propostas novas; Câmara 1.226; CVM 2.312; Senado nenhum (não publica CPF). Ao todo, **42.716 propostas pendentes** (116.814 fichas). 27,2 % das pessoas com fichas juntam mais de uma ficha; 639.265 pessoas juntam fichas de mais de uma fonte.

O que outros limiares fariam (calculado por SQL sobre `person_record`, sem mudar nada):

| Limiar | Grupos unidos sozinhos | Propostas (grupos) | Grupos unidos entre fontes | Pares fundidos por engano, ordem de grandeza (espaço 10⁶ / 10⁵) |
|---|---|---|---|---|
| 50 | 5.469.355 | 84.632 | 634.078 | 11 / 112 |
| **100 (atual)** | **5.507.974** | **46.013** | **639.265** | **17 / 170** |
| 300 | 5.539.453 | 14.534 | 643.547 | 29 / 286 |
| 1.000 | 5.550.984 | 3.003 | 645.242 | 43 / 429 |

(Os grupos e propostas desta tabela são estimativas por nome e trecho, feitas por SQL. Os números reais são os da resolução, acima: 5,79 milhões de grupos e 42.716 propostas. A última coluna soma, para cada nome com n trechos dentro do limiar, n(n−1)/2 dividido pelo tamanho do espaço de trechos.)

O trecho de CPF que as fontes publicam tem seis dígitos, e um deles indica a região fiscal. Por isso duas pessoas diferentes com o mesmo nome podem ter o mesmo trecho. Somando todos os nomes unidos sozinhos, a conta dá **algo da ordem de dezenas a poucas centenas de pares de fichas de pessoas diferentes fundidos por engano** com o limiar 100: 17 pares se o espaço efetivo de trechos for de 10⁶, 170 se for de 10⁵. É uma **estimativa de ordem de grandeza, não um intervalo garantido**, e se apoia em três hipóteses:

- os trechos se espalham por igual no espaço;
- o número de trechos diferentes de um nome é o número de pessoas com esse nome;
- 10⁵ é o pior caso.

Nenhuma das três é exata. Homônimos se concentram por região e por época de emissão do CPF, o que encolhe o espaço efetivo, e uma pessoa sem trecho não entra na conta. **170 não é um teto.** A conta mede pares de fichas fundidas, não pessoas. As diferenças entre limiares, na tabela, servem como comparação relativa.

Cada junção automática aparece na Bancada como inferência ("identidade" com confiança menor que 1), nunca como fato, e pode ser desfeita por um analista.

**Recomendação:** manter 100 por ora (baixar depois não desfaz junções; subir só junta mais). Se a fila de 42.716 for grande demais para revisar, 300 corta a fila em dois terços e aumenta os pares fundidos por engano em cerca de 70 % (na mesma ordem de grandeza, pelas mesmas hipóteses). Abaixo de 100 o ganho é pequeno (cerca de um terço a menos, pela mesma conta) e a fila dobra.

## Metas M1 a M4 e página de entidade

Medidas pela API autenticada (usuário temporário com papel `analyst`, login por código de autorização + PKCE no Keycloak, como a Bancada), uma requisição por vez, com as configurações acima, depois de `VACUUM (ANALYZE)` nas quatro tabelas grandes (2 min 29 s, feito à mão como dono do banco; a plataforma não faz isso sozinha depois de uma carga, então os números refletem estatísticas e mapas de visibilidade em dia) e de um reinício limpo da pilha. "Primeira passada" é a primeira depois do reinício do PostgreSQL (o cache do sistema operacional não foi esvaziado: sem root, "frio" é aproximado). Amostras escolhidas por SQL determinístico (sementes fixas), guardadas como ids em `results/geoint-national/measure/samples/` (fora do git).

**M1 — busca.** 25 termos fixos do ensaio, 25 nomes sorteados (as duas primeiras palavras de entidades sorteadas), 10 prefixos digitados, 10 nomes parciais, 10 CNPJs; mais 10 termos adversos (fora da meta, como no ensaio); cada um em três escopos (todos, empresas, pessoas). "LTDA" e "AB" respondem 400 "consulta curta demais" por desenho e contam como resposta.

| Classe (escopo todos) | 1ª passada p50 / p95 / máx. (s) | 2ª passada p50 / p95 / máx. (s) |
|---|---|---|
| termos fixos | 0,039 / 0,132 / 0,151 | 0,006 / 0,071 / 0,088 |
| nomes sorteados | 0,014 / 0,091 / 0,097 | 0,007 / 0,059 / 0,075 |
| prefixos digitados | 0,044 / 0,420 / 0,420 | 0,038 / 0,428 / 0,428 |
| nomes parciais | 0,031 / 0,197 / 0,197 | 0,020 / 0,185 / 0,185 |
| CNPJ | 0,008 / 0,009 / 0,009 | 0,002 / 0,003 / 0,003 |
| adversos (extra) | 0,034 / 0,651 / 0,651 | 0,006 / 0,621 / 0,621 |
| **240 buscas da meta (3 escopos)** | **0,009 / 0,098 / 0,420** | **0,004 / 0,078 / 0,428** |

**Página de entidade** (200 entidades sorteadas + as 20 de maior grau): 1ª passada p50 0,009 s, p95 0,013 s, máx. 0,572 s; 2ª passada p95 0,008 s, máx. 0,009 s. Passou.

**M2 — vizinhos** (primeira página de 200, as mesmas 220 entidades): 1ª passada p95 0,213 s (as 20 de maior grau: p50 0,236 s, máx. 0,512 s); 2ª passada p95 0,015 s, máx. 0,028 s. Passou.

**M3 — caminhos até profundidade 6**, como tarefas em segundo plano (o tempo inclui a espera do pedido, com consultas a cada 100 ms):

| Grupo | Pares | Achados | 1ª passada pior (s) | 2ª passada pior (s) | Hubs não expandidos |
|---|---|---|---|---|---|
| deputado/senador × empresa de maior grau | 8 | 3 | 20,0 | 1,24 | até 46 por pedido |
| deputado/senador × empresa sorteada | 7 | 0 | 0,42 | 0,32 | até 4 |
| deputado/senador × empresa de um doador dele | 5 | 5 | 0,84 | 0,32 | até 7 |
| pontas de passeios aleatórios | 6 | 6 | 0,11 | 0,12 | 0 |
| pares sorteados | 5 | 0 | 0,11 | 0,11 | 0 |

Nenhum pedido falhou nem foi truncado. Os passeios aleatórios morreram quase todos no primeiro passo: o grafo é muito fragmentado (4 de 6 passeios pararam com 1 passo).

**M4 — grupo de controle**, nos dois modos: holdings (as 5 com mais participações) pior 4,2 s ("controle documentado") e 8,8 s ("vínculos societários") na 1ª passada; companhias da CVM pior 2,3 s; demais abaixo de 1,4 s. 2ª passada: pior 0,58 s. Passou.

## Critérios de sucesso, um a um

Dos 7 critérios da seção 2: **4 passaram, 1 em parte, 1 falhou, 1 não verificável**.

| Critério (seção 2 do desenho) | Como foi conferido | Resultado | Situação |
|---|---|---|---|
| Num Docker limpo, um comando sobe tudo e outro inicia a carga | `make up` (subiu e migrou em volumes novos) e `make collect SOURCE=…` | sobe com um comando; a carga é **um comando por fonte** (cinco), por desenho: o agendador nunca faz a primeira coleta | parcial |
| Base nacional carregada e consultas nas metas | carga acima; M1–M4 e página de entidade | consultas: todas as metas cumpridas; carga da Receita: 26 h 20 min contra 12 h | **falhou** (meta de carga) |
| Dado o CNPJ, ver sócios e administradores, as outras empresas de cada um e seguir a cadeia | API: busca `33000167` → PETROLEO BRASILEIRO S A PETROBRAS; vínculos; vínculos dos administradores; um nível adiante | 332 vínculos (308 sócio, 9 administrador, 5 representante, 7 participação e 3 controle da CVM); para administradores, as outras empresas (0 a 2 cada) e as empresas seguintes (até 62 vínculos) | passou |
| Dado um político e uma empresa, achar os caminhos | tarefa de caminhos pela API entre o deputado federal ANTONIO GOULART DOS REIS e MRV ENGENHARIA E PARTICIPACOES SA | 3 caminhos de 4 elos (doações do TSE e administração na Receita ou participação na CVM); cada elo com tipo, fonte e "fato"; as pessoas do meio têm identidade "automática" (inferência) | passou |
| Cada conexão abre a fonte: base, registro, link, data de coleta | `GET /relationships/{id}/provenance` de um vínculo da Petrobras; SHA-256 refeito | base Receita, arquivo `…SOCIOCSV` e linha, coleta de referência 2026-10-01 publicada em 2026-10-13, data de coleta, endereço do arquivo na Receita e a nota de como abri-lo; o mesmo arquivo baixado de novo da Receita tem **exatamente** o SHA-256 guardado (`0c74c031…7633e6`) e o mesmo tamanho | passou |
| Uma tela mostra a situação de cada base | `GET /sources` (o que a tela Bases mostra) | as cinco "em dia", com referência, publicação (Receita) e hora da coleta | passou |
| Sócio que sai continua consultável com o período | — | só uma edição da Receita foi carregada (a 2026-11 não saiu): **não verificável nesta carga**. Os mandatos encerrados da Câmara (3.841) e do Senado (576) mostram períodos, mas não são o caso do critério | não verificável |

### Outras verificações (seção 3 do desenho; fora da conta dos critérios)

| O que foi conferido | Como | O que se observou |
|---|---|---|
| Fato e inferência com aparência diferente; nada mostrado como certo sem ser | vínculos da Petrobras e dos caminhos, pela API | os 332 vínculos são "fato"; 15 deles dependem de uma junção automática de identidade e vêm marcados como "automática"; nenhuma inferência nem junção automática com confiança 1 |
| Homônimos não fundidos sem aviso | contadores da resolução, `GET /identity/proposals` | toda junção automática vem marcada como inferência (confiança < 1), com o motivo; 42.716 propostas pendentes para um analista; estimativa de pares fundidos por engano da ordem de dezenas a poucas centenas (veja "Identidade"); a fila abre, e a primeira página (50 propostas) levou 3,3 s. Não há meta de tempo para essa tela |

Não usamos a Bancada num navegador nesta medição (o teste de ponta a ponta continua cobrindo as telas com dados de exemplo); todas as verificações acima usam as mesmas rotas da API que as telas chamam.

## O que não foi medido, e por quê

- **Recarga mensal da Receita** (fechar o que sumiu em escala nacional, regravação de `last_seen`, inchaço, limite pela data de publicação): a edição 2026-11 não estava publicada em 2026-11-13 (a pasta mais nova do compartilhamento é 2026-10). Única referência: o ensaio médio, 9 min 30 s para 11,27 milhões de linhas; uma estimativa linear para a base inteira não é confiável, dado que o custo por linha cresceu 2,5 vezes durante esta carga.
- **Vínculo encerrado com período** (critério da seção 2): pelo mesmo motivo.
- **Cópia de segurança** (`make backup`) da base carregada: não feita (pararia a pilha por horas; não pedida).
- **"Frio" de verdade**: o cache do sistema operacional não foi esvaziado (sem root).
- **Bancada num navegador** sobre a base nacional: não rodada (opcional).

## Problemas encontrados e correções feitas

Antes da carga (commits `086e6a4`, `067fbf2`, `72d6f69`): o agendador não marca mais como falha uma coleta viva só por ter mais de 24 h; `volume-init` tolera arquivos que somem durante a varredura (achado na própria pilha nacional: o atalho "dono da raiz correto = convertido" era inseguro e foi retirado); a conferência de início de carga testa o login das funções do banco em vez de exigir uma conexão aberta; TSE e CVM guardam o arquivo bruto pela identidade que o servidor informa (ETag, data, tamanho); `GET /sources` diz quais bases são publicadas em edições.

Durante a carga: nenhuma falha de coleta e nenhuma correção de código. A primeira versão do resumo da medição contava as recusas "consulta curta demais" como erro; corrigido na ferramenta de medição. Divergências do README corrigidas: `make down` antes da pilha nacional só se a pilha de uso estiver no ar; `PG_SHM_SIZE=12g` recomendado no `.env.national`; tempos estimados trocados pelos medidos.

## Riscos e recomendações

1. **A carga inicial da Receita não cabe em 12 h nesta máquina** (26 h 20 min). Seguimentos sugeridos, não feitos: (a) dados do PostgreSQL num diretório sem CoW (`chattr +C` antes do `initdb`) ou em ext4/xfs, e medir de novo; (b) gravar os blocos de vínculos em paralelo (hoje um núcleo); (c) carregar a tabela de vínculos sem índices na primeira carga e criá-los depois; (d) na primeira carga, resolver identidades antes de gravar os vínculos (ou reapontar em lote), em vez de reapontar 10 milhões de fichas vínculo a vínculo; (e) analisar, pelos tempos por bloco no log do worker, se o custo continua crescendo numa segunda edição.
2. **Toda coleta reconstrói a busca inteira** (~40 min, mesmo para o Senado). Com o agendador ligado, TSE, Câmara, Senado e CVM semanais custam cerca de 3 h de reconstrução por semana. Uma reconstrução incremental ou em lote semanal resolveria.
3. **Identidade**: com o limiar atual, a estimativa de pares de fichas fundidos por engano é da ordem de dezenas a poucas centenas, e não é um teto (veja "Identidade"); a fila tem 42.716 propostas. Decidir o limiar antes de abrir para outras pessoas.
4. **Fila de identidades lenta** (3,3 s na primeira página): investigar o plano da listagem.
5. **Entidades de pessoa vazias**: 10,7 milhões de entidades de pessoa ficaram sem ficha depois das junções (não aparecem na busca; ocupam espaço).
6. **TSE regera quase todos os arquivos toda semana** (8 de 10 na semana medida): ~4 GB de brutos por semana.
7. A pilha `geoint-national` ficou parada com os volumes (banco de 130 GB e brutos de 11,7 GB) — veja o README para religar ou apagar.
