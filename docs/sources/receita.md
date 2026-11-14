# Receita Federal — Dados Abertos do CNPJ

Conferido em 2026-11-11, baixando e abrindo os arquivos de 2026-10 (a
edição mais recente publicada nessa data; ainda não havia pasta 2026-11).

## Onde ficam os arquivos

- A página antiga da Receita
  (`https://www.gov.br/receitafederal/pt-br/assuntos/orientacao-tributaria/cadastros/consultas/dados-publicos-cnpj`)
  hoje redireciona para uma tela de login e não serve mais como ponto de
  entrada.
- O endereço antigo `https://arquivos.receitafederal.gov.br/dados/cnpj/dados_abertos_cnpj/`
  responde 404.
- `https://arquivos.receitafederal.gov.br/` é um Nextcloud (SERPRO) que
  redireciona para o compartilhamento público
  `https://arquivos.receitafederal.gov.br/index.php/s/gn672Ad4CF8N6TK`.
  Os dados do CNPJ ficam em **Dados > Cadastros > CNPJ**, uma pasta por mês
  (`AAAA-MM`). Em 2026-11-11 havia de `2023-05` a `2026-10`, além de um
  `cnpj.tar.gz` de 64 GB (de 2026-01-28, histórico agregado; não usado).
- O catálogo oficial também aponta para o conjunto
  `https://dados.gov.br/dados/conjuntos-dados/cadastro-nacional-da-pessoa-juridica---cnpj`.
- Dicionário de dados (PDF oficial):
  `https://www.gov.br/receitafederal/dados/cnpj-metadados.pdf` (responde 200).

### Como baixar sem navegador

O compartilhamento aceita WebDAV público, com o token do compartilhamento
como usuário e senha vazia:

```bash
# listar meses / arquivos de um mês (PROPFIND, Depth: 1)
curl -X PROPFIND -u "gn672Ad4CF8N6TK:" -H "Depth: 1" \
  https://arquivos.receitafederal.gov.br/public.php/webdav/Dados/Cadastros/CNPJ/2026-10/
# baixar um arquivo, com retomada
curl -C - -u "gn672Ad4CF8N6TK:" -o Socios0.zip \
  https://arquivos.receitafederal.gov.br/public.php/webdav/Dados/Cadastros/CNPJ/2026-10/Socios0.zip
```

O PROPFIND devolve `getcontentlength` de cada arquivo, usado para conferir o
tamanho do download. O token pode mudar; se mudar, abrir
`https://arquivos.receitafederal.gov.br/` e copiar o código que aparece depois
de `/index.php/s/`.

Velocidade observada: 3 a 4 MB/s. Os 37 arquivos de 2026-10 (todos os do
mês: Empresas, Estabelecimentos, Sócios, Simples e as tabelas de domínio),
7.808.752.942 bytes (7,81 GB = 7,27 GiB), levaram 37 min 26 s. Os arquivos de
2026-10 foram publicados em 2026-10-13. O `cnpj.tar.gz` da pasta raiz **não**
foi baixado.

## Arquivos de cada mês (2026-10)

37 arquivos `.zip`, cada um com **um único** CSV dentro (nome interno do tipo
`K3241.K03200Y0.D61010.EMPRECSV`, sem extensão `.csv`).

| Arquivo | Tamanho do zip | Observação |
|---|---|---|
| `Empresas0.zip` … `Empresas9.zip` | 1,39 GB no total | `Empresas0` sozinho tem 571 MB (2,35 GB descompactado); os demais ~90 MB |
| `Estabelecimentos0.zip` … `Estabelecimentos9.zip` | 5,42 GB no total | `Estabelecimentos0` tem 2,28 GB; os demais ~340 MB |
| `Socios0.zip` … `Socios9.zip` | 0,69 GB no total | `Socios0` tem 251 MB (1,06 GB descompactado); os demais ~49 MB |
| `Simples.zip` | 311 MB | opção pelo Simples/MEI |
| `Cnaes.zip`, `Motivos.zip`, `Municipios.zip`, `Naturezas.zip`, `Paises.zip`, `Qualificacoes.zip` | < 50 KB cada | tabelas de domínio |

As partes **não** têm tamanhos parecidos: a parte 0 de cada grupo concentra
boa parte das linhas. Paralelizar a carga por arquivo deixa a parte 0 como
gargalo.

Cópia local: `data/raw/receita/2026-10/`, com os 37 zips e um `MANIFEST.txt`
(nome e tamanho de cada arquivo segundo o servidor), 38 arquivos ao todo.
`unzip -t` passou nos 37 zips.

## Formato dos CSV

Confirmado nos arquivos:

- **sem cabeçalho**;
- separador `;`;
- **todos** os campos entre aspas duplas (`"…"`), inclusive números;
- aspas dentro de um campo vêm **dobradas** (`""`), como no CSV padrão.
  Em 2026-10 havia 2.721 campos com aspas em Empresas, 11.744 em
  Estabelecimentos e nenhum em Sócios. Conferido lendo todos os arquivos dos
  três grupos com um leitor CSV padrão (Python `csv`): nenhuma linha com
  número de colunas errado;
- **quebra de linha dentro de campo entre aspas** existe: 7 campos em
  Estabelecimentos (nenhum em Empresas e Sócios). O leitor precisa ser um
  leitor CSV de verdade, não um `split` por linha;
- fim de linha **LF** (`\n`), sem `\r`;
- codificação **Latin-1** (ISO-8859-1); carregado com
  `COPY … WITH (FORMAT csv, DELIMITER ';', QUOTE '"', ENCODING 'LATIN1')`;
- datas como `AAAAMMDD` (`"0"` ou vazio quando não há);
- valores monetários com vírgula decimal (`"120000000000,00"`);
- **bytes NUL** existem: 2 campos de Estabelecimentos em 2026-10 (nenhum em
  Empresas e Sócios). O PostgreSQL recusa NUL em texto; a carga os remove com
  `tr -d '\000'`.

## Colunas, em ordem

### Empresas (7 colunas)

1. `cnpj_basico` — 8 primeiros caracteres do CNPJ
2. `razao_social`
3. `natureza_juridica` — código (tabela Naturezas)
4. `qualificacao_responsavel` — código (tabela Qualificacoes)
5. `capital_social` — vírgula decimal
6. `porte_empresa` — `00` não informado, `01` micro, `03` pequeno porte, `05` demais
7. `ente_federativo_responsavel` — só para natureza 1XXX

### Estabelecimentos (30 colunas)

1. `cnpj_basico` 2. `cnpj_ordem` 3. `cnpj_dv` 4. `identificador_matriz_filial`
(1 matriz, 2 filial) 5. `nome_fantasia` 6. `situacao_cadastral` (01 nula,
2 ativa, 3 suspensa, 4 inapta, 08 baixada) 7. `data_situacao_cadastral`
8. `motivo_situacao_cadastral` 9. `nome_cidade_exterior` 10. `pais`
11. `data_inicio_atividade` 12. `cnae_fiscal_principal`
13. `cnae_fiscal_secundaria` (lista separada por vírgula) 14. `tipo_logradouro`
15. `logradouro` 16. `numero` 17. `complemento` 18. `bairro` 19. `cep`
20. `uf` 21. `municipio` (código da tabela Municipios, não IBGE) 22. `ddd_1`
23. `telefone_1` 24. `ddd_2` 25. `telefone_2` 26. `ddd_fax` 27. `fax`
28. `correio_eletronico` 29. `situacao_especial` 30. `data_situacao_especial`

### Sócios (11 colunas) — confere com o esperado

1. `cnpj_basico`
2. `identificador_socio` — `1` pessoa jurídica, `2` pessoa física, `3` estrangeiro
3. `nome_socio` — nome da PF, razão social da PJ ou nome do estrangeiro
4. `cnpj_cpf_socio` — ver diferenças abaixo
5. `qualificacao_socio` — código (tabela Qualificacoes)
6. `data_entrada_sociedade` — `AAAAMMDD`
7. `pais` — código do país (só estrangeiro)
8. `representante_legal` — CPF mascarado `***NNNNNN**`; `***000000**` quando não há
9. `nome_representante`
10. `qualificacao_representante` — código; `00` quando não há
11. `faixa_etaria` — `0` não se aplica, `1` 0–12 anos, `2` 13–20, `3` 21–30,
    `4` 31–40, `5` 41–50, `6` 51–60, `7` 61–70, `8` 71–80, `9` mais de 80

### Tabelas de domínio (2 colunas: `codigo`, `descricao`)

`Cnaes`, `Motivos`, `Municipios`, `Naturezas`, `Paises`, `Qualificacoes`.

## Diferenças em relação ao esperado

1. **Endereço**: mudou para o Nextcloud descrito acima (as páginas e caminhos
   antigos não funcionam mais).
2. **`cnpj_cpf_socio` de sócio PJ tem só 8 caracteres** (o `cnpj_basico` do
   sócio), não o CNPJ de 14 dígitos. Verificado em 2026-10: todas as linhas
   com `identificador_socio = 1` têm 8 caracteres. A data da mudança (agosto
   de 2026) vem do README de um projeto comunitário (rictom/cnpj-sqlite) e
   está **não verificada**: não comparei com meses anteriores.
   Para ligar sócio PJ à empresa basta o `cnpj_basico`; quem precisar do CNPJ
   completo deve buscar a matriz em Estabelecimentos.
3. **Sócio estrangeiro (`3`)**: `cnpj_cpf_socio` vem vazio (o dicionário diz
   que estrangeiro não tem essa informação). Não há como separar homônimos
   estrangeiros por documento.
4. **CPF mascarado** confirmado: `***NNNNNN**` (os 6 dígitos do meio, posições
   4 a 9 do CPF). Todas as linhas PF de 2026-10 seguem o padrão.
5. **CNPJ alfanumérico**: segundo o título de uma notícia da Receita de
   julho de 2026 ("Receita Federal gera o primeiro CNPJ em formato
   alfanumérico"), o primeiro CNPJ alfanumérico já foi gerado. Só li o título
   da notícia (**não verificado**). Em 2026-10 nenhum `cnpj_basico` de
   Empresas tinha letras,
   mas **`cnpj_basico` deve ser texto**, não número (zeros à esquerda e
   letras).
6. O nome de empresário individual costuma trazer o CPF completo (11
   dígitos) no fim da razão social. Exemplo ilustrativo, inventado:
   `FULANA DE TAL 00000000000`. É dado pessoal publicado pela própria
   Receita; não deve ser exibido nem usado como chave sem decisão explícita.
7. **`cnpj_basico` repetido em Empresas**: 1 caso em 2026-10 (70.517.438
   linhas e 70.517.437 valores distintos). A carga deve deduplicar.
8. **Sócio PJ órfão**: 1 `cnpj_basico` de sócio PJ em 2026-10 não existe em
   Empresas. A carga deve criar a empresa só com a chave, ou registrar o
   órfão.
9. **Sócio PF sem nome**: 1.777 registros PF de 2026-10 vêm com o nome vazio.
   Se a pessoa for identificada por nome + CPF, todos eles se fundem num nó
   falso.

## Qualificações de sócio que indicam cargo de administração/direção

Tabela `Qualificacoes` de 2026-10 (texto oficial):

| Código | Descrição | Administração? |
|---|---|---|
| 05 | Administrador | sim |
| 08 | Conselheiro de Administração | sim |
| 10 | Diretor | sim |
| 16 | Presidente | sim |
| 28 | Sócio-Gerente | sim |
| 49 | Sócio-Administrador | sim |
| 50 | Empresário | sim (titular de empresário individual) |
| 65 | Titular Pessoa Física Residente ou Domiciliado no Brasil | sim (titular de unipessoal) |
| 70 | Administrador Residente ou Domiciliado no Exterior | sim |
| 71 | Conselheiro de Administração Residente ou Domiciliado no Exterior | sim |
| 72 | Diretor Residente ou Domiciliado no Exterior | sim |
| 73 | Presidente Residente ou Domiciliado no Exterior | sim |
| 74 | Sócio-Administrador Residente ou Domiciliado no Exterior | sim |
| 11 | Interventor | administração temporária (externa) |
| 13 | Liquidante | administração temporária (liquidação) |
| 64 | Administrador Judicial | administração temporária (judicial) |
| 17 | Procurador | representa, mas não é cargo de administração |
| 19 | Síndico (Condomínio) | administra condomínio |

Conjunto recomendado para "administrador/diretor":
`05, 08, 10, 16, 28, 49, 70, 71, 72, 73, 74`; mais `50, 65` se o titular de
empresa individual contar como administrador, e `11, 13, 64` marcados à parte
como administração temporária.

Tabela completa de qualificações (2026-10): 00 Não informada; 05 Administrador;
08 Conselheiro de Administração; 09 Curador; 10 Diretor; 11 Interventor;
12 Inventariante; 13 Liquidante; 14 Mãe; 15 Pai; 16 Presidente; 17 Procurador;
18 Secretário; 19 Síndico (Condomínio); 20 Sociedade Consorciada; 21 Sociedade
Filiada; 22 Sócio; 23 Sócio Capitalista; 24 Sócio Comanditado; 25 Sócio
Comanditário; 26 Sócio de Indústria; 28 Sócio-Gerente; 29 Sócio Incapaz ou
Relat.Incapaz (exceto menor); 30 Sócio Menor (Assistido/Representado);
31 Sócio Ostensivo; 32 Tabelião; 33 Tesoureiro; 34 Titular de Empresa
Individual Imobiliária; 35 Tutor; 37 Sócio Pessoa Jurídica Domiciliado no
Exterior; 38 Sócio Pessoa Física Residente no Exterior; 39 Diplomata;
40 Cônsul; 41 Representante de Organização Internacional; 42 Oficial de
Registro; 43 Responsável; 46 Ministro de Estado das Relações Exteriores;
47 Sócio Pessoa Física Residente no Brasil; 48 Sócio Pessoa Jurídica
Domiciliado no Brasil; 49 Sócio-Administrador; 50 Empresário; 51 Candidato a
cargo Político Eletivo; 52 Sócio com Capital; 53 Sócio sem Capital;
54 Fundador; 55 Sócio Comanditado Residente no Exterior; 56 Sócio Comanditário
Pessoa Física Residente no Exterior; 57 Sócio Comanditário Pessoa Jurídica
Domiciliado no Exterior; 58 Sócio Comanditário Incapaz; 59 Produtor Rural;
60 Cônsul Honorário; 61 Responsável indígena; 62 Representante da Instituição
Extraterritorial; 63 Cotas em Tesouraria; 64 Administrador Judicial;
65 Titular Pessoa Física Residente ou Domiciliado no Brasil; 66 Titular Pessoa
Física Residente ou Domiciliado no Exterior; 67 Titular Pessoa Física Incapaz
ou Relativamente Incapaz (exceto menor); 68 Titular Pessoa Física Menor
(Assistido/Representado); 69 Beneficiário Final; 70 Administrador Residente ou
Domiciliado no Exterior; 71 Conselheiro de Administração Residente ou
Domiciliado no Exterior; 72 Diretor Residente ou Domiciliado no Exterior;
73 Presidente Residente ou Domiciliado no Exterior; 74 Sócio-Administrador
Residente ou Domiciliado no Exterior; 75 Fundador Residente ou Domiciliado no
Exterior; 78 Titular Pessoa Jurídica Domiciliada no Brasil; 79 Titular Pessoa
Jurídica Domiciliada no Exterior.

### Códigos usados pela carga como administração (`administrator_of`)

O conector (`internal/connector/receita`, tabela `adminQualifications`) gera
`administrator_of`, além de `partner_of`, para os códigos marcados "sim" na
tabela acima:
`05, 08, 10, 16, 28, 49, 50, 65, 70, 71, 72, 73, 74`.
A administração temporária (`11, 13, 64`) **não** entra.

## Naturezas jurídicas de fundos

Tabela `Naturezas` de 2026-10:

| Código | Descrição | Fundo de investimento? |
|---|---|---|
| **2224** | Clube/Fundo de Investimento | **sim** — o código dos fundos de investimento (FIP, FII, FIA, FIDC etc.) |
| 3247 | Fundo Privado | possivelmente; fundos privados que não são de investimento regulado (ex.: fundos patrimoniais). Tratar à parte |
| 3328 | Plano de Benefícios de Previdência Complementar Fechada | não é fundo de investimento, mas é investidor institucional |
| 1287, 1295, 1309 | Fundo Público da Administração Indireta (Federal, Estadual/DF, Municipal) | não (fundos públicos) |
| 1317, 1325, 1333 | Fundo Público da Administração Direta (Federal, Estadual/DF, Municipal) | não (fundos públicos) |

Regra recomendada: **fundo de investimento = natureza `2224`**; `3247`
sinalizado como "fundo privado" para revisão.

Observações sobre a tabela `Naturezas`: não vem ordenada; há dois códigos de
"não informada" (`0000` e `8885`); há descrições em caixa alta
(`1350 ENTIDADE PÚBLICA SOB REGIME ESPECIAL`, `3310 DEMAIS CONDOMÍNIOS`).

## Como o conector carrega (Tarefa 7)

- **Data de referência = mês da edição**: o pedido traz uma data qualquer (o
  agendador e o `collect` sem `--ref` mandam o dia de hoje); antes de abrir a
  coleta, o conector lista as pastas `AAAA-MM` do compartilhamento e escolhe
  a **edição mais nova publicada até o mês pedido**. A data de referência da
  coleta é o primeiro dia desse mês (UTC): em 2026-11-11, por exemplo, a
  coleta vira 2026-10-01, porque a pasta 2026-11 ainda não existia. Só
  entram pastas com nome `AAAA-MM`; as demais (e arquivos) são ignoradas.
  Para fixar uma edição, peça uma data dentro do mês dela:
  `geoint collect receita --ref 2026-10-31` carrega a edição 2026-10 mesmo se
  a 2026-11 já tiver saído (`--ref` no futuro é recusado).
- **Data de observação = publicação da edição**: a coleta guarda também
  `observed_date`, o dia (UTC) do `getlastmodified` mais novo entre as partes
  de dados da edição escolhida: a data em que aquele retrato da base já
  existia. É ela, e não o primeiro dia do mês, que limita o fim de um vínculo
  que sumiu (veja "Encerramento" abaixo) e que conta o prazo de validade.
- **Mês pela metade não é usado**: uma pasta só conta como publicada quando
  passa em **todas** estas regras (tudo isso na escolha da edição; a lista de
  arquivos julgada completa é a mesma que o download baixa):
  1. Empresas, Estabelecimentos e Sócios com partes numeradas 0..n-1, sem
     buraco;
  2. cada grupo com pelo menos tantas partes quanto teve na última coleta
     completa concluída da Receita (contadas em `collection_file`); sem coleta
     anterior (ou sem arquivos registrados nela), pelo menos 10 partes por
     grupo. `RECEITA_MIN_PARTS`, quando **definida**, vale sempre e substitui
     a contagem da última coleta (é a saída quando a Receita passa a publicar
     menos partes). Um envio em ordem de nome parece completo a cada prefixo
     (Socios0 a Socios8 de 10), por isso a contagem. Quando a pasta mais nova
     é pulada por esta regra, o log registra em nível Warn o mês, o grupo, as
     partes encontradas e as exigidas;
  3. assentada: o arquivo de dados mais novo da pasta (data
     `getlastmodified` da listagem, formato RFC 1123 em GMT, lido de forma
     estrita) foi modificado há pelo menos `RECEITA_SETTLE_HOURS` horas
     (padrão 6; 0 desliga a regra). Arquivo sem data válida conta como não
     assentado.

  As tabelas de domínio não entram na regra: a carga não lê nenhuma (guarda
  os códigos como vêm). Se a pasta mais nova falha em alguma regra, o
  conector registra no log (Info) qual mês pulou e qual regra falhou e usa o
  mês anterior; olha no máximo as 3 pastas mais novas. Sem nenhuma edição
  completa, a coleta falha com "nenhuma edição publicada". Se a lista de
  arquivos de dados mudar entre a escolha e o download (parte nova, tamanho,
  ETag ou data diferente), o download falha antes de baixar qualquer coisa e
  nada é carregado; a próxima verificação escolhe de novo.

  Atenção: se a Receita passar a publicar **menos** partes que na última
  carga, nenhuma edição nova passa na regra 2 até alguém definir
  `RECEITA_MIN_PARTS` com a nova contagem (a fonte fica vencida, o que chama
  a atenção; o Warn do log diz quantas partes vieram).
- **Nada novo**: um pedido de carga completa cuja edição já tem coleta
  completa concluída não abre coleta nenhuma: o worker registra "nothing new
  to collect" no log, confirma o pedido e grava a verificação em
  `source_check` (data do banco). Recorte (`--cnpjs`) da mesma edição carrega
  de novo normalmente.
- **Frescor**: a Receita publica uma edição por mês (`PublishEvery` 30 dias).
  Quando a última edição carregada fica com 30 dias ou mais, a plataforma
  passa a verificar uma vez por dia se saiu edição nova (cada verificação é
  só uma listagem). A fonte aparece **vencida** quando a própria Receita não
  publica edição nova em 45 dias (`StaleAfter`) contados da **publicação** da
  última edição carregada (`observed_date`), não do primeiro dia do mês dela:
  uma edição publicada no dia 20 não vence 25 dias depois.
- **Depois de uma verificação que falhou**: se a listagem falhar (rede,
  servidor) ou não houver edição completa, a tentativa aparece como coleta
  falha, com a data pedida, a fonte fica `failed` e é tentada de novo no dia
  seguinte. A primeira verificação bem-sucedida depois disso (carga de edição
  nova, ou "nada novo") tira a fonte de `failed`: o estado volta a seguir a
  data da edição (`fresh`/`stale`) e `last_checked_at` mostra quando foi a
  verificação. Uma falha nova depois dela deixa a fonte `failed` outra vez.
- **Sem carga retroativa (estágio 1)**: todas as datas de referência da
  Receita são agora o primeiro dia do mês da edição. A carga recusa
  (`ErrOlderEdition`), antes de escrever qualquer coisa, uma data de
  referência anterior à de uma coleta da Receita que já gravou dados:
  concluída (`done`), ou falha/interrompida que já registrou arquivos em
  `collection_file` (a carga registra cada arquivo antes de gravar qualquer
  vínculo; uma tentativa que falhou antes disso não conta). Comparação da
  data exata, a mesma regra do encerramento de vínculos não vistos; carregar
  um mês antigo sobre dados (mesmo parciais) de um mês novo fecharia as
  versões novas das empresas. A saída é carregar de novo a edição nova (ou
  uma mais nova). Um recorte da mesma edição carrega de novo; uma carga completa
  de edição já concluída é pulada ("nada novo"). Os arquivos ficam
  no armazenamento bruto em `raw/receita/AAAA-MM-01/<arquivo>`.
- **Para quem atualiza**: coletas gravadas antes desta mudança têm como data
  de referência o dia em que rodaram (por exemplo 2026-11-11), não o primeiro
  dia do mês da edição. Num banco carregado antes, recarregue a Receita ou
  normalize essas datas para o primeiro dia do mês da edição; do contrário a
  regra acima recusaria a edição seguinte. Ainda não há banco de produção.
- **Download**: `PROPFIND` (Depth 1) na pasta do mês, `GET` com retomada
  (`Range` + `If-Range` com o ETag). Baixa Empresas, Estabelecimentos e Sócios
  (cada grupo precisa estar completo, partes 0..n-1 sem buraco) e as seis
  tabelas de domínio; não baixa `Simples.zip`. O token do compartilhamento vai
  na parte de usuário da URL base (`RECEITA_BASE_URL`).
- **Linha malformada** (pulada e contada em `rows_skipped`): não dá para
  separar no número de colunas esperado (aspas soltas, coluna a mais ou a
  menos, aspas que não fecham) ou não tem `cnpj_basico` válido. Também é
  pulada a linha de Sócios cujo sócio não dá para identificar (tipo fora de
  1/2/3, ou sócio PJ sem CNPJ válido), contada ainda em
  `socios_unidentified_partner`. Datas `00000000`, `0`, vazias ou impossíveis
  viram `valid_from` NULL; bytes NUL são removidos; nenhum dos dois faz a
  linha ser pulada.
- **Filiais** (`identificador_matriz_filial = 2`) não são carregadas; a
  `company_version` usa a matriz.
- **Sócio PF**: uma ficha (`person_record`) por pessoa por empresa
  (`context_key = cnpj_basico`). Sócios PF sem nome também são carregados: uma
  ficha por (empresa, nome vazio, fragmento do CPF); nunca se fundem.
- **Representante legal**: pessoa física identificada por nome e CPF
  mascarado na própria linha; vira ficha no contexto da mesma empresa e
  `legal_rep_of` (representante → sócio, `role` = qualificação do
  representante, sem data).
- **Sócio estrangeiro**: entidade `foreign` com chave
  `receita:<cnpj_basico>:<nome normalizado>`; sem nome, a chave é
  `receita:<cnpj_basico>:#<código de qualificação>` (`#` nunca aparece num
  nome normalizado), para que estrangeiros sem nome de uma mesma empresa não
  se fundam em silêncio; com o mesmo código, viram uma só entidade.
- **Sócio PJ órfão** (ou empresa que só aparece em Sócios): cria a entidade
  `company` com o nome da linha de sócio, sem `company_version`.
- **Identidades antes dos vínculos**: depois de gravar as fichas novas e
  antes do primeiro vínculo, a carga roda `ANALYZE person_record` e a
  primeira passada da resolução de identidades só para as fichas desta
  coleta (`identity.Resolver.ResolveRecords`, o mesmo resolvedor e o mesmo
  limite de nome comum da resolução final). Assim cada vínculo já é gravado
  apontando para a entidade em que a ficha ficou, e a resolução do fim da
  coleta (`Resolve`) não precisa mover ficha nem reapontar vínculo: só
  atualiza as propostas (o número de vínculos de cada uma), confere os
  ponteiros e marca a coleta como resolvida. Uma carga interrompida entre as
  duas passadas deixa fichas já juntadas e a coleta falhada sem a marca; a
  resolução da coleta seguinte a completa.
- **Vínculos em lotes**: os vínculos são gravados por balde — até 1.024
  faixas consecutivas de `cnpj_basico` com quantidades parecidas de linhas de
  Sócios, recalculadas a cada carga a partir do staging
  (`connector.RangeChunks`); todos os vínculos de uma empresa caem no mesmo
  balde, e os ids de empresas, pessoas e fichas são distribuídos nessa mesma
  ordem, de modo que cada balde lê e escreve trechos curtos das tabelas e
  índices —, cada balde numa transação curta que começa
  pela trava de identidade (`identity.LockTx`) e lê nela a entidade atual de
  cada ficha. Uma carga que falha no meio é reparada pela coleta seguinte (as
  inserções são idempotentes).
- **Arquivos brutos da coleta**: cada zip carregado vira uma linha imutável
  em `collection_file` (nome do CSV interno — a parte do `record_ref` antes de
  `:<linha>` —, chave do objeto no armazenamento bruto, SHA-256 calculado na
  leitura, tamanho e `source_url`, a URL WebDAV do arquivo **sem** o token do
  compartilhamento: o token vai só como usuário da autenticação Basic e nunca
  é gravado), de modo que todo vínculo aponta para o arquivo preservado.
- **Estatísticas**: depois das fases em lote a carga roda `ANALYZE` em
  `entity`, `company_version`, `person_record` e `relationship` (cada um fora
  de transação longa; amostragem, segundos), e cada lote de vínculos analisa
  a sua tabela de preparo antes do UPDATE/INSERT.
- **Encerramento** (`CloseUnseen`, só coleta completa): um vínculo aberto
  que a edição nova não lista mais é encerrado com `valid_to_after` = data de
  referência da última coleta que o viu (primeiro dia do mês daquela edição:
  um limite inferior verdadeiro) e `valid_to_before` = `observed_date` da
  coleta nova + 1 dia (exclusivo: no retrato publicado nesse dia o vínculo já
  não existia, então terminou nesse dia ou antes). Exemplo: sócio presente na
  edição 2026-10 (publicada em 2026-10-13) e ausente da 2026-11 (publicada em
  2026-11-10) fica "saiu entre 2026-10-01 e 2026-11-11"; numa consulta em
  2026-11-04 o vínculo ainda aparece (pode ter saído depois).

### Limitações conhecidas

- **Nomes das entidades congelados**: a entidade guarda o nome com que foi
  vista pela primeira vez. Uma empresa vista primeiro só como sócia (nome da
  linha de sócio) ou só em Sócios (nome vazio) mantém esse nome mesmo quando
  Empresas a trouxer depois; o nome atual fica na `company_version`.
- **Mudança de `company_version`** compara também os campos da matriz em
  Estabelecimentos (UF, município, CNAE, situação), mas o `record_ref` da
  versão cita só a linha de Empresas.
- **Conexões**: a carga segura uma conexão do pool durante toda a execução
  (trava de sessão), então o pool precisa de `pool_max_conns` ≥ 2 (a carga
  recusa um pool menor); com 4 arquivos em paralelo, o ideal é ≥ 6.
