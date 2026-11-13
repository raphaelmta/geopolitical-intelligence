# Plataforma de inteligência geopolítica

Plataforma de código aberto (AGPL-3.0) para mapear conexões geopolíticas a partir de fontes públicas: empresas e sócios (Receita Federal), candidaturas e doações eleitorais (TSE), mandatos de deputados e senadores (Câmara e Senado) e controle acionário de companhias abertas (CVM). Um único binário Go (`geoint`) com os subcomandos `api`, `worker`, `migrate`, `collect` e `version`, apoiado por PostgreSQL 18, NATS JetStream, MinIO e Keycloak. A interface web, a Bancada, fica em [`web/`](web/).

Todo vínculo mostrado vem de uma fonte pública e diz de onde veio: a base, o registro, a data de referência e o arquivo bruto preservado (com SHA-256). Os dados são **afirmações das fontes**, não verdades verificadas pela plataforma.

## Requisitos da máquina

Só são necessários **Docker** (com Compose v2), **make** e um shell POSIX com os utilitários comuns (`sh`, `sed`, `base64`, `df`; já vêm no Linux e no macOS). Go, Node.js e curl não precisam estar instalados: tudo roda em contêineres.

Os números abaixo vêm da carga nacional medida em 2026-11 nesta máquina de referência ([`docs/performance/2026-national-load.md`](docs/performance/2026-national-load.md)), com o disco do Docker em btrfs (cópia-na-escrita, compressão zstd). **A carga inicial da Receita levou 26 h 20 min, acima da meta de 12 h** (veja o documento).

| Recurso | Estimativa |
|---|---|
| CPU | 6 núcleos (a máquina do ensaio) |
| Memória | 32 GB recomendados (o ensaio usou 31 GB); 16 GB com o perfil menor de [`deploy/TUNING.md`](deploy/TUNING.md) |
| Disco | SSD/NVMe, **pelo menos 200 GB livres** para começar: a base nacional ocupa cerca de 141 GB em tamanho real (volume do banco 130 GB com o WAL + arquivos brutos 12 GB) e precisa de cerca de 165 GB no pico (mais a cópia da busca, ~19 GB, e a área de download); o TSE acrescenta ~4 GB por semana. Na máquina medida, com btrfs comprimindo (zstd), o `df` cresceu só 112 GB no pico: não conte com isso em ext4/xfs (veja [Disco](#disco)) |
| Rede | a Receita entregou cerca de 5,2 MB/s em 2026-11 (24 min para baixar uma edição; 37 min no ensaio) |

O PostgreSQL sobe com parâmetros ajustados para essa máquina (memória, WAL, paralelismo, autovacuum), descritos em [`deploy/TUNING.md`](deploy/TUNING.md), com um perfil para 16 GB. Todo número de desempenho deve dizer com quais parâmetros foi medido.

## Instalação

1. Clone o repositório e entre na pasta.
2. Suba a pilha:

   ```sh
   make up
   ```

   Na primeira vez, `make up` cria o arquivo `.env` a partir de [`.env.example`](.env.example) com **senhas aleatórias novas** (banco, MinIO, administrador do Keycloak; [`scripts/init-env.sh`](scripts/init-env.sh)), visível só para o seu usuário (modo 0600). A senha do administrador do Keycloak fica nesse arquivo, na linha `KEYCLOAK_ADMIN_PASSWORD`. O `.env` nunca vai para o git e nunca é sobrescrito.

   Depois, `make up` mostra o espaço livre em disco, constrói as imagens, sobe PostgreSQL, NATS, MinIO, Keycloak e a interface, **aplica as migrações do banco com a API e o worker parados** e só então liga a API e o worker; termina quando a API responde em `/healthz`. Se a migração falhar, a API e o worker ficam parados e o comando termina com erro (o motivo aparece logo acima).

   `make up` **se recusa a subir** se o `.env` ainda tiver alguma senha de exemplo (`change-me-…`, valores públicos) ou se faltar alguma senha. Isso acontece com um `.env` criado por uma versão antiga: veja [Trocar senhas](#trocar-senhas).
3. Crie o primeiro usuário no Keycloak (abaixo) e abra a Bancada em http://localhost:5173.
4. Peça a primeira coleta de cada fonte (veja [Coletas](#coletas)): o agendador nunca faz a primeira coleta sozinho.

Endereços (todos só em `127.0.0.1`, nunca expostos à rede):

- Bancada: http://localhost:5173
- API: http://localhost:8080
- Keycloak (login e console de administração): http://localhost:8081

`make down` derruba a pilha mantendo os dados (volumes). A pilha volta sozinha depois de reiniciar a máquina (veja [Reinício e atualização](#reinício-e-atualização)).

### Configuração opcional (`.env`)

Além das senhas, o `.env` aceita (todas vazias por padrão; veja os comentários em [`.env.example`](.env.example)):

| Variável | Vazia significa | Para quê |
|---|---|---|
| `SCHEDULER_ENABLED` | ligado | `false` desliga o agendador: só coletas pedidas à mão |
| `IDENTITY_COMMON_NAME_THRESHOLD` | 100 | quantos trechos de CPF diferentes um nome pode ter e ainda ser unido automaticamente (padrão 100); acima disso vira proposta na fila de identidades. Nome + trecho de CPF é indício, não prova: a confiança de uma junção automática é max(0,5; 1 − (n−1)/100.000), nunca 1; baixar o limiar depois não desfaz junções |
| `RECEITA_MIN_PARTS`, `RECEITA_SETTLE_HOURS` | regras normais | deixe **vazias**. Um `RECEITA_MIN_PARTS` explícito sempre substitui o número de partes da última carga: use só quando a Receita de fato publicar menos partes (o log do worker avisa com as partes achadas e as exigidas). Detalhes em [`docs/sources/receita.md`](docs/sources/receita.md) |
| `WEB_ORIGIN` | `http://localhost:5173` | a única origem de navegador que a API aceita (CORS) |
| `API_ALLOWED_HOSTS` | só os locais | nomes de host aceitos pela API além dos locais (`localhost`, `127.0.0.1`, `api`), separados por vírgula |
| `DISK_WARN_GB` | 200 | abaixo de quantos GB livres o `make up` avisa |
| `PG_…` | perfil de 31 GB | parâmetros do PostgreSQL ([`deploy/TUNING.md`](deploy/TUNING.md)) |

Depois de mudar o `.env`, rode `make up` de novo.

## Primeiro usuário no Keycloak

O login é separado da aplicação: quem autentica é o Keycloak (OIDC). O navegador entra pelo Keycloak (fluxo *authorization code* com PKCE) e envia à API apenas o token de acesso; a API só verifica a assinatura, o emissor, a validade e a audiência desse token e nunca vê senhas.

Na primeira subida, o Keycloak importa o realm `geoint` de [`deploy/keycloak/realm-geoint.json`](deploy/keycloak/realm-geoint.json): cliente público `geoint-web` (PKCE S256, redirecionamento só para `http://localhost:5173`), papéis `analyst` e `admin`, proteção contra força bruta, cadastro aberto e recuperação de senha por e-mail desligados. O arquivo não contém usuários, senhas nem chaves: o Keycloak gera as chaves de assinatura ao importar.

1. Com a pilha no ar, abra http://localhost:8081/admin e entre com `KEYCLOAK_ADMIN` / `KEYCLOAK_ADMIN_PASSWORD` do seu `.env` (abra o arquivo num editor de texto).
2. No seletor de realms (canto superior esquerdo), escolha `geoint`. Não crie usuários da aplicação no realm `master`.
3. **Users → Create new user**: preencha o nome de usuário, nome, sobrenome e e-mail e salve.
4. Aba **Credentials → Set password**: a política do realm exige **pelo menos 12 caracteres, diferente do nome de usuário e do e-mail**. Deixe **Temporary** ligado para que a pessoa troque a senha no primeiro acesso.
5. Aba **Role mapping → Assign role**: filtre por papéis de realm (*Filter by realm roles*) e atribua `analyst` (consulta e decisões de identidade) ou, só para quem administra, `admin` (também pede coletas). O papel `admin` é composto e **já inclui `analyst`**: quem recebe `admin` não precisa receber `analyst` também.

Os papéis chegam à API na claim `roles` do token de acesso (no caso de `admin`, com `analyst` junto). A API compara papéis por nome exato; a hierarquia fica só no realm. Mudanças de papel valem a partir do próximo token (no máximo 5 minutos). Depois de 5 tentativas de senha erradas, a conta fica bloqueada por alguns minutos (proteção contra força bruta). O realm não exige segundo fator; para uso hospedado, recomenda-se exigir OTP pelo menos para quem tem `admin` (não está configurado).

### Reimportar o realm

A importação só acontece se o realm ainda não existir no volume do Keycloak. Para aplicar uma versão nova de `realm-geoint.json` numa máquina local, apagando **todos** os usuários e configurações do Keycloak:

```sh
make down
docker volume rm geoint_kcdata
make up
```

## A Bancada

Abra http://localhost:5173. Sem sessão, qualquer página manda para o login do Keycloak e volta para a mesma página depois. Telas:

- **Busca** (`/`): procura empresas, pessoas, partidos e órgãos pelo nome, pelo CNPJ (8 ou 14 dígitos) ou pelo CPF. Casa palavras inteiras; o quadro "Como a busca funciona" explica o que ela não acha. Um CPF (11 dígitos ou o trecho mascarado `***NNNNNN**`, sozinho ou com o nome) lista as **pessoas candidatas**: as fontes publicam só os 6 dígitos do meio, e a busca mostra quem coincide nesse trecho, nunca uma confirmação. O CPF digitado não vai para o endereço da página.
- **Entidade** (`/entidade/:id`): cadastro, registros que formam a pessoa e vínculos atuais e encerrados. Logo abaixo do nome, o **Mapa de conexões** desenha a entidade e seus vínculos atuais (a primeira página, até 200), com o link para o mapa completo. Cada vínculo diz se é **fato** ou **inferência**, e o botão **fonte** mostra a base, o registro, as datas e o arquivo bruto de onde veio.
- **Mapa** (`/mapa/:id`): o gráfico dos vínculos a partir de uma entidade, ao lado de uma lista com tudo o que o gráfico mostra. "Expandir" traz os vínculos de outra entidade; filtros por tipo, situação e data.
- **Caminhos** (`/caminhos`): as ligações mais curtas entre duas entidades, elo por elo, cada elo com a sua natureza e a sua fonte. Roda em segundo plano e mostra o progresso.
- **Grupo de controle** (`/grupo/:id`, a partir de uma empresa): "Controle documentado" segue só controle declarado, participação acima de 50 % e sócio único; "Vínculos societários" segue todo vínculo societário (mostra quem está ligado, não quem manda).
- **Identidades** (`/identidades`): a fila de propostas de "mesma pessoa" que a plataforma não juntou sozinha. Um analista confirma ou recusa cada proposta, vendo as fichas e a entidade de destino.
- **Bases** (`/bases`): a situação de cada fonte (em dia, vencida, nunca coletada, falhou), a data de referência dos dados e os contadores da última coleta. Administradores veem o botão **Coletar agora**.

Sessão: os tokens ficam só na memória da página (nunca em `localStorage`, `sessionStorage` ou cookies; o `sessionStorage` guarda apenas o estado temporário do redirecionamento, com o verificador PKCE, apagado na volta). Por isso, **recarregar a página faz um novo redirecionamento ao Keycloak**: enquanto a sessão do Keycloak estiver aberta (até 30 minutos sem uso), ele volta direto, sem pedir a senha. O token de acesso dura 5 minutos e é renovado com o *refresh token* um minuto antes de vencer, sem iframes; se a renovação for recusada, a aplicação mostra "Sua sessão expirou" e um botão para entrar de novo, voltando à mesma página. **Sair** encerra a sessão no Keycloak e mostra "Você saiu".

A configuração pública é servida em `/config.json` e vem de três variáveis do serviço `web` em `deploy/compose.yml`:

| Variável | Padrão | O que é |
|---|---|---|
| `WEB_API_URL` | `http://localhost:8080` | endereço da API, como o navegador o vê |
| `WEB_OIDC_AUTHORITY` | `http://localhost:8081/realms/geoint` | emissor OIDC (realm do Keycloak) |
| `WEB_OIDC_CLIENT_ID` | `geoint-web` | cliente OIDC público |

Ao iniciar, o contêiner valida os valores e não sobe se um deles for inválido: os endereços precisam ter a forma `http(s)://host[:porta][/caminho]`, sem aspas, espaços, ponto e vírgula, consulta ou fragmento; o host é um nome DNS válido (rótulos sem `-` no começo ou no fim, sem `..`), um IPv4 ou um IPv6 entre colchetes bem formado, e a porta vai de 1 a 65535; o cliente aceita só letras, dígitos, `.`, `_` e `-`. Uma barra final é removida. O `connect-src` da política de segurança de conteúdo (CSP) é limitado às origens (`esquema://host:porta`) da API e do Keycloak. Não há segredo: o cliente `geoint-web` é público.

## Coletas

Uma coleta baixa a publicação de uma fonte, guarda os arquivos brutos (MinIO), carrega os vínculos, resolve identidades e reconstrói as tabelas de busca. O worker roda **uma coleta por vez**; as outras esperam na fila.

### O agendador

O agendador nunca faz a primeira coleta de uma fonte: a primeira carga de cada fonte é pedida à mão (`make collect SOURCE=…` ou o botão **Coletar agora**). Depois disso ele a mantém em dia (1 minuto depois de o worker iniciar, e depois de hora em hora, pede uma coleta completa de cada fonte cuja publicação nova é esperada) e repete uma coleta que falhou no máximo uma vez por dia. Uma fonte cuja primeira coleta falhou também não é repetida sozinha: peça de novo. Tempo de cada coleta completa, medido em 2026-11 nesta máquina na primeira carga (inclui resolver identidades e reconstruir a busca, cerca de 40 min em toda coleta):

| Fonte | Publica a cada | Vence em | Tempo aproximado |
|---|---|---|---|
| Receita Federal (CNPJ) | mensal | 45 dias depois da **publicação** da edição | primeira carga: **26 h 20 min** (download 24 min, leitura 3 min, gravação 11 h 05 min, identidades 14 h 07 min, busca 41 min); mensais: não medidas |
| TSE (candidaturas e contas, eleições 2018 a 2026) | 7 dias | 30 dias | 2 h 53 min (download 4 GB em 2 min, gravação 23 min, identidades 1 h 45 min, busca 44 min) |
| Câmara dos Deputados (legislaturas 55 a 57) | 7 dias | 14 dias | 1 h 03 min (23 min de requisições, 39 min de busca) |
| Senado Federal (legislaturas 55 a 57) | 7 dias | 14 dias | 46 min (5 min de requisições, 41 min de busca) |
| CVM (companhias abertas, ano atual e anterior) | 7 dias | 30 dias | 45 min (2 min de carga, 43 min de busca) |

O prazo da Receita conta a partir do dia em que a edição foi publicada (a data do arquivo mais novo dela), não do primeiro dia do mês da edição. Se a Receita atrasar uma edição além desses 45 dias, a fonte e todos os vínculos dela aparecem como **vencidos** até a próxima carga.

Depois de cada coleta, as tabelas de busca são reconstruídas por inteiro; na base nacional isso levou de 39 a 44 minutos por coleta (medido em 2026-11), e durante esse tempo a busca continua servindo as tabelas anteriores.

Para desligar o agendador (só coletas pedidas à mão), ponha `SCHEDULER_ENABLED=false` no `.env` e rode `make up` de novo.

### Pedir uma coleta à mão

Na linha de comando (a fila é a mesma do agendador):

```sh
make collect SOURCE=receita    # ou tse, camara, senado, cvm
make collect SOURCE=receita ARGS="--cnpjs 12345678,87654321 --depth 1"   # recorte da Receita
make collect SOURCE=receita ARGS="--ref 2026-10-31"   # fixa a edição 2026-10 da Receita
```

`make collect` roda `geoint collect <fonte>` num contêiner da imagem da API. Ele registra o pedido (como o botão **Coletar agora**), põe na fila e termina; a coleta acontece no worker. Ele **recusa**, com erro, um pedido de coleta completa enquanto uma coleta completa da mesma fonte estiver rodando ou um pedido completo ainda estiver esperando na fila (recortes nunca são recusados). Uma data `--ref` no futuro é recusada; `--ref` fixa a edição carregada (por exemplo `--ref 2026-10-31` carrega a edição 2026-10 da Receita mesmo que a 2026-11 já tenha saído). Sem `--ref`, a Receita carrega a edição mais nova já publicada; se ela já foi carregada, nada é baixado de novo e o log do worker diz "nothing new to collect".

Na Bancada, um administrador pede a mesma coleta completa em **Bases → Coletar agora**. O botão fica desativado enquanto já houver um pedido aguardando início.

### Situação das bases e como saber que algo falhou

- **em dia**: a última coleta completa reflete uma publicação dentro do prazo da fonte.
- **vencida**: a última coleta completa reflete uma publicação mais velha que o prazo ("Vence em", acima). Todo vínculo vindo dela aparece com a etiqueta **vencido**. Pode ser a fonte que não publicou nada novo, ou coletas que não estão rodando.
- **falhou**: a coleta mais recente terminou com erro (e nenhuma verificação bem-sucedida veio depois). O texto do erro aparece em **Bases** para administradores. Os dados da coleta anterior continuam valendo.
- **nunca coletada**: nenhuma coleta completa terminou ainda.

A plataforma não manda e-mail nem alerta: **uma coleta que falhou só aparece na tela Bases e no log do worker**. Olhe a tela Bases de tempos em tempos (uma coleta agendada que falha de madrugada fica lá como "falhou"). Os detalhes estão no log do worker:

```sh
docker compose -p geoint --env-file .env -f deploy/compose.yml logs --tail 200 worker
```

Os logs de cada contêiner ficam limitados a 5 arquivos de 20 MB (os mais antigos são apagados).

## Reinício e atualização

Todos os serviços que ficam no ar têm `restart: unless-stopped`: depois de reiniciar a máquina, a pilha volta sozinha. Se a API ou o worker subirem antes do banco, eles saem com erro e o Docker os reinicia até o banco responder; nada é perdido nesse vaivém. Uma coleta que estava rodando quando a máquina caiu é marcada como **falhou** quando o worker volta, e o pedido é refeito pela fila. Um reinício não aplica migrações: código e banco continuam na mesma versão. `make down` para tudo até o próximo `make up`.

Para atualizar:

```sh
git pull
make up        # reconstrói as imagens, migra com a API e o worker parados e liga tudo de novo
```

Depois de um `git pull`, rode sempre `make up`, nunca só um reinício dos contêineres: só `make up` reconstrói as imagens e aplica as migrações. As migrações sempre rodam com a API e o worker (o carregador) parados: o código novo nunca roda sobre o esquema antigo. `make up` pode ser repetido sem efeito colateral. Para só migrar, com a pilha no ar, há `make migrate` (mesma ordem: para a API e o worker, migra, liga de novo). Leia as notas de atualização das fontes antes, em especial "Para quem atualiza" em [`docs/sources/receita.md`](docs/sources/receita.md).

**Quem atualiza de uma versão anterior a esta**: o `make up` vai recusar o `.env` antigo (senhas de exemplo e senhas novas que faltam). Siga [Trocar senhas](#trocar-senhas). Os volumes de dados criados pela versão antiga (gravados como root) são convertidos sozinhos para os usuários sem privilégio na primeira subida (serviço `volume-init`); nada precisa ser apagado.

## Segurança da pilha

- Nenhum contêiner roda como root: PostgreSQL (usuário 70), NATS e MinIO (10001), Keycloak (1000), API e worker (65532), interface (101). Todos descartam todas as capacidades do Linux e não podem ganhar privilégios (`no-new-privileges`). A exceção é o passo `volume-init`, que roda por um instante antes dos outros, sem rede, só para dar os volumes de dados a esses usuários.
- O sistema de arquivos dos contêineres é somente leitura (só os volumes de dados e pequenas áreas temporárias aceitam escrita), menos o Keycloak, cujo modo de desenvolvimento reconstrói o servidor a cada partida.
- O banco tem três identidades. O dono das tabelas (`POSTGRES_USER`) só é usado pelas migrações. O worker entra como `geoint_worker` e a API como `geoint_api`: nenhum dos dois pode apagar vínculos, desligar as regras de integridade (os gatilhos que proíbem apagar e reescrever afirmações) nem mudar a estrutura das tabelas; a API só grava auditoria, decisões de identidade e pedidos de coleta e de tarefas. A matriz completa está no topo de [`internal/db/migrations/0021_roles.sql`](internal/db/migrations/0021_roles.sql).
- A API não tem acesso aos arquivos brutos (MinIO); só o worker tem.

### Trocar senhas

Todas as senhas ficam no `.env`. Elas devem ter pelo menos 16 caracteres e só `A-Z a-z 0-9 . _ ~ -` (vão dentro de endereços de conexão). Para gerar uma senha nova:

```sh
dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr '+/' '-_' | tr -d '=\n'; echo
```

**Instalação nova, que nunca subiu** (ou que você aceita perder): apague o `.env` e rode `make up`; um `.env` novo com senhas aleatórias é criado.

**Instalação que já subiu com o `.env` antigo** (os dados ficam):

1. Acrescente as senhas novas que faltam (valores aleatórios; nada mais muda):

   ```sh
   sh scripts/init-env.sh --add-missing .env
   ```
2. Troque cada senha de exemplo, **uma de cada vez**, com a pilha no ar (`make up` recusa subir, mas o que já está no ar continua):
   - **PostgreSQL** (`POSTGRES_PASSWORD`): o banco guarda a senha dele; mudar só o `.env` quebra a pilha. Primeiro mude no banco, depois no `.env`:

     ```sh
     docker compose -p geoint --env-file .env -f deploy/compose.yml exec postgres \
       psql -U geoint -d geoint -c '\password geoint'
     ```

     O `psql` pede a senha nova duas vezes (digite a mesma que vai pôr no `.env`). Depois troque `POSTGRES_PASSWORD` no `.env`.
   - **MinIO** (`MINIO_ROOT_PASSWORD`): troque no `.env`; o MinIO passa a usar a nova no próximo `make up`.
   - **Keycloak** (`KEYCLOAK_ADMIN_PASSWORD`): o Keycloak só lê essa variável na primeira subida. Entre no console (http://localhost:8081/admin, realm `master`), **Users → admin → Credentials → Reset password**, defina a nova (sem **Temporary**) e ponha a mesma no `.env`, para não esquecê-la.
   - **Funções do banco** (`DB_WORKER_PASSWORD`, `DB_API_PASSWORD`): basta mudar no `.env`; cada `make up` (e `make migrate`) as aplica no banco.
3. Rode `make up`.

Para trocar uma senha no futuro, o caminho é o mesmo do passo 2.

## Backup e restauração

O que não pode ser recriado por uma nova coleta: o **banco** (decisões de identidade, auditoria, o histórico de vínculos encerrados, que nenhuma carga refaz), os **arquivos brutos** do MinIO (a prova de cada afirmação) e o **Keycloak** (usuários). A fila (NATS) não entra: só guarda pedidos pendentes, que podem ser pedidos de novo.

```sh
make backup DEST=/caminho/de/uma/pasta/nova
```

Durante o backup, a API, o worker, o Keycloak e o MinIO ficam parados (nenhuma carga roda); no fim, os que estavam no ar voltam. A pasta recebe `database.dump` (`pg_dump`), `miniodata.tar`, `kcdata.tar`, `roles.sql`, um `manifest.txt` (data, commit, versões, contagens, tamanhos) e `SHA256SUMS`. Escolha uma pasta fora do repositório (ou `backups/`, que o git ignora).

**O backup contém dados pessoais** tal como as fontes os publicam (os arquivos brutos do TSE, da CVM e da Câmara trazem CPFs completos) e os usuários do Keycloak. A pasta e os arquivos são criados visíveis só para o seu usuário; guarde o backup cifrado e fora da máquina, e trate-o na revisão de LGPD.

Para restaurar (numa máquina nova, ou por cima desta instalação):

```sh
make restore SRC=/caminho/do/backup            # recusa se o projeto já tiver dados
make restore SRC=/caminho/do/backup FORCE=1    # APAGA os dados atuais do projeto e restaura
```

A restauração confere todas as somas SHA-256, sobe um PostgreSQL novo, restaura o banco e compara as contagens com o manifesto, devolve os arquivos brutos e o Keycloak e termina com `make up`. Use o mesmo `POSTGRES_USER` do backup; as senhas são as do `.env` atual.

## Disco

O que cresce, e quanto (medido na carga nacional de 2026-11; tamanhos reais, sem a compressão do sistema de arquivos):

- **Banco**: medido em 2026-11 com as cinco fontes: 95,7 GB (Receita sozinha: 89 GB), dos quais 19 GB são as tabelas de busca; o volume do banco, com o WAL, ocupou 130 GB. As regravações mensais da Receita deixam linhas mortas que o autovacuum recicla (não medido ainda).
- **Arquivos brutos (MinIO)**: **guardados para sempre** (nada é apagado automaticamente). A Receita guarda cerca de 7,3 GiB por edição (uma por mês). O TSE e a CVM guardam um arquivo de novo só quando o servidor o regerou (a chave vem do ETag, da data e do tamanho do arquivo no servidor): um ano parado não ocupa mais nada. Mas, em 2026-11-12, o TSE tinha regerado na última semana 8 dos seus 10 arquivos (≈ 4,0 dos 4,1 GB, inclusive as prestações de contas de 2018 a 2022), então hoje a coleta semanal do TSE ainda guarda **cerca de 4 GB por semana (≈ 200 GB por ano)**, enquanto o TSE continuar regerando tudo; a CVM, cerca de 17 MB por semana. Veja [`docs/sources/tse.md`](docs/sources/tse.md).
- **WAL do PostgreSQL**: até `max_wal_size` (32 GB) durante as cargas.
- **Reconstrução da busca**: precisa de cerca de 2 vezes o tamanho das tabelas de busca (19 GB a mais) durante a reconstrução, que levou cerca de 40 minutos na base nacional (as novas são montadas ao lado das antigas).
- **Área de download do worker**: o maior arquivo sendo baixado (alguns GB), apagada no fim.

Somando: cerca de 165 GB no pico da carga nacional (banco 96 GB + WAL 32 GB + brutos 12 GB + cópia da busca 19 GB + download alguns GB), mais o crescimento dos brutos (TSE ~4 GB/semana; Receita 7,3 GB/mês). Por isso a recomendação é de 200 GB livres (250 GB para um ano de TSE). Na máquina medida o disco é btrfs com compressão zstd e o `df` cresceu só 112 GB no pico; em ext4 ou xfs conte com o tamanho real.

`make up` mostra o espaço livre onde o Docker guarda os dados e avisa abaixo de 200 GB (`DISK_WARN_GB`). Para ver o uso por volume: `docker system df -v`. Com o disco cheio, o PostgreSQL para de gravar (a coleta em andamento falha; nada já gravado se perde) e o NATS e o MinIO também param de aceitar dados: libere espaço e peça a coleta de novo.

## Carga nacional

A carga nacional (as cinco fontes completas, para medir a plataforma) precisa começar de **volumes novos**: por desenho, nada pode ser apagado do banco, e coletas de teste antigas mudariam o resultado (uma edição "já carregada", vínculos de teste encerrados pela primeira carga real). Não apague a pilha de uso (`geoint`): crie um projeto separado, com volumes e senhas próprios:

```sh
sh scripts/init-env.sh .env.national           # senhas novas, só deste projeto
# no .env.national: SCHEDULER_ENABLED=false (só coletas pedidas à mão)
# e, recomendado, PG_SHM_SIZE=12g (folga para a reconstrução da busca)
make down                                      # só se a pilha geoint estiver no ar: usa as mesmas portas
make up DEV_PROJECT=geoint-national ENV_FILE=.env.national
make national-check DEV_PROJECT=geoint-national ENV_FILE=.env.national
make collect SOURCE=receita ARGS="--ref 2026-10-31" DEV_PROJECT=geoint-national ENV_FILE=.env.national
```

`make national-check` ([`scripts/preflight.sh`](scripts/preflight.sh)) não muda nada; ele confere e recusa começar se algo estiver errado: o `.env` sem senhas de exemplo; os parâmetros do PostgreSQL em vigor iguais aos de [`deploy/TUNING.md`](deploy/TUNING.md) (e os mostra, para anotar junto das medições); o banco sem nenhuma coleta (se houver, lista e recusa, a menos que `ALLOW_EXISTING=1`) e nenhuma coleta `running`; as funções do worker e da API entram com as senhas do arquivo de configuração (um login de verdade pelo endereço de rede do contêiner, onde o banco pede senha; pelo `localhost` ele aceitaria qualquer senha) e têm as permissões principais esperadas; espaço livre de pelo menos 200 GB (`NATIONAL_DISK_MIN_GB`); e mostra o agendador e o limiar de identidade. Fixe a edição da Receita com `--ref`, para que uma edição nova publicada no meio da carga não mude o que é medido. Depois da carga, `make measure DEV_PROJECT=geoint-national ENV_FILE=.env.national PASS=cold` mede as metas pela API autenticada ([`scripts/measure.sh`](scripts/measure.sh): cria um usuário temporário no Keycloak e o apaga no fim; resultados em `results/`, fora do git), e `scripts/load-sampler.sh` registra disco, memória e WAL a cada minuto durante uma carga. Depois da medição, volte à pilha de uso com `make down DEV_PROJECT=geoint-national ENV_FILE=.env.national` e `make up`. Para apagar a base nacional de vez (irreversível): `docker compose -p geoint-national --env-file .env.national -f deploy/compose.yml down -v`.

## Testes e verificações

```sh
make test          # go test ./... contra postgres/nats/minio descartáveis
make test-race PKG=./internal/auth   # o mesmo pacote com o detector de corridas
make lint          # gofmt + go vet
make vuln          # govulncheck
make test-scripts  # testes dos scripts de shell (.env, volumes, verificações; um deles sobe um PostgreSQL descartável)
make test-web      # testes da aplicação web (Vitest)
make lint-web      # checagem de tipos (tsc)
make audit-web     # npm audit --audit-level=high
make check-web-image   # constrói a imagem web e confere CSP, cabeçalhos e /config.json
make e2e           # teste de ponta a ponta, num navegador de verdade
```

Os alvos `*-web` rodam em contêineres `docker run --rm` sem nome fixo e instalam as dependências sob uma trava no próprio diretório `web/` (compartilhada por todas as execuções, qualquer que seja o volume de cache `WEB_CACHE`, padrão `geoint-nodecache`), então duas execuções simultâneas não colidem; a versão do Node é a de `engines` em `web/package.json`, a mesma da imagem fixada. `make test` e as ferramentas Go usam o projeto `geoint-test` (mude com `TEST_PROJECT=…`) com parâmetros pequenos do PostgreSQL; `check-web-image` usa `TEST_PROJECT` no nome da imagem e do contêiner (ex.: `make check-web-image TEST_PROJECT=meu-teste`).

### `make e2e`

Sobe uma pilha descartável (projeto `geoint-e2e`, [`deploy/compose.e2e.yml`](deploy/compose.e2e.yml) sobre `deploy/compose.yml`) que **não publica nenhuma porta na máquina** e não colide com a pilha de uso (`geoint`). Ela roda como a de produção (sem root, somente leitura, com as três identidades do banco), com parâmetros pequenos do PostgreSQL e um arquivo de configuração próprio com senhas aleatórias descartáveis (nunca o seu `.env`). Um simulador ([`cmd/sourcesim`](cmd/sourcesim)) serve as amostras de [`testdata/`](testdata/) no formato de cada fonte; [`scripts/e2e-seed.sh`](scripts/e2e-seed.sh) migra o banco e coleta as cinco fontes; o Playwright ([`web/e2e/`](web/e2e/)), num contêiner na mesma rede, faz login de verdade no Keycloak, busca (também por CPF, conferindo que o CPF não vai para o endereço nem para o armazenamento), navega, abre fontes, o mapa de conexões da entidade, o mapa, caminhos (inclusive um que só existe porque a resolução de identidade juntou a mesma pessoa na Receita e no TSE), grupo de controle, a fila de identidades (uma proposta é aberta e cancelada, outra é rejeitada; para haver propostas, essa pilha usa `IDENTITY_COMMON_NAME_THRESHOLD=1`) e bases, e confere a CSP, os cabeçalhos, o CORS, os papéis e que nenhum token fica guardado no navegador. A pilha é sempre derrubada com os volumes no fim, também em caso de falha (depois de mostrar as últimas linhas de log). Leva cerca de 1 min 30 s numa máquina de 6 núcleos com as imagens em cache. Capturas e rastros de testes que falharam ficam em `web/e2e-results/` (fora do git); os rastros contêm tokens do realm descartável, cujas chaves somem quando a pilha é derrubada. O seed para logo no início se o ano corrente passar do que as amostras da CVM cobrem.

O realm dessa pilha, `geoint-e2e` ([`deploy/keycloak/realm-e2e.json`](deploy/keycloak/realm-e2e.json)), é o de produção mais dois usuários de teste (`analista` e `admin-e2e`) com senhas que começam com `e2e-only-`. **Essas credenciais só existem na pilha descartável do `make e2e`**: o arquivo é montado apenas por `compose.e2e.yml`, e um teste (`internal/auth/realm_test.go`) garante que ele difere do realm de produção só no nome e nesses usuários, e que `compose.yml` nunca o importa.

## Limites conhecidos e avisos

- **Busca**: casa palavras inteiras (só a última pode ser começo de palavra); não tolera erros de digitação; nomes com menos de 3 caracteres úteis ou só com palavras comuns ("LTDA") só são achados pelo CNPJ; números longos dentro de nomes não são pesquisáveis; a busca por CPF casa só os 6 dígitos do meio e devolve candidatos, não confirmação ([`docs/search.md`](docs/search.md)).
- **Identidade**: pessoas nunca são juntadas só pelo nome. A junção automática exige o mesmo nome normalizado **e** o mesmo trecho de CPF; nome + trecho de CPF é indício, não prova, e a confiança mostrada nunca é 1. Quando o nome é muito comum (mais de 100 trechos diferentes), o grupo vira proposta na fila de identidades. Fichas sem trecho de CPF nunca se juntam sozinhas: aparecem em "Possível mesma pessoa", e só um analista pode juntá-las. O Senado não publica CPF, e os candidatos de 2024 vieram sem CPF.
- **Partidos**: siglas grafadas de forma diferente entre fontes e partidos que mudaram de nome ou se fundiram (DEM/PSL → UNIÃO) são entidades distintas.
- **Doações de empresas** foram proibidas em 2015: nas eleições de 2018 a 2026 não há doação de pessoa jurídica a candidato. Doações por financiamento coletivo e os doadores originários de repasses não são carregados.
- **Fontes que não são retrato completo** (TSE, Câmara, Senado): um registro removido ou corrigido pela fonte depois de carregado não apaga o que já foi gravado; uma correção de data de fim grava uma segunda linha ao lado da antiga.
- **CVM**: uma holding descrita de formas diferentes mostra só a versão do formulário mais recente; holdings estrangeiras sem CNPJ não são descritas; o grupo de controle documentado depende do que as companhias declararam.
- **Dados públicos com proveniência**: cada vínculo é uma afirmação da fonte na data de referência mostrada, não uma verificação da plataforma. Confira sempre a fonte antes de concluir.
- **Arquivos brutos**: o MinIO guarda os arquivos **exatamente como as fontes os publicam**, inclusive CPFs completos onde a fonte os publica (TSE, CVM, Câmara). O banco e a Bancada só guardam e mostram o trecho do meio do CPF, mas os arquivos brutos e os backups contêm o número inteiro. Isso importa para a revisão de LGPD e para quem guarda os backups.
- **LGPD**: a plataforma trata dados pessoais publicados por órgãos oficiais. Uma revisão de LGPD (finalidade, base legal, retenção, direitos dos titulares) é **pré-requisito** antes de abrir o uso a clientes ou terceiros.
- **MinIO**: a imagem usada é uma compilação comunitária de uma versão congelada, aceitável só para uso local; antes de qualquer implantação hospedada, troque por um armazenamento compatível com S3 mantido ([`deploy/VERSIONS.md`](deploy/VERSIONS.md)).
- **Keycloak** roda em modo de desenvolvimento (`start-dev`), por HTTP e com armazenamento embutido: só para uso local (veja abaixo).

### Antes de abrir para clientes

Antes de qualquer uso por terceiros ou implantação fora da sua máquina:

- **Revisão de LGPD** feita (finalidade, base legal, retenção, direitos dos titulares; inclui os arquivos brutos e os backups, que têm CPFs completos).
- **MinIO** trocado por um armazenamento compatível com S3 mantido (acima).
- **Keycloak** em modo de produção (`start`), atrás de TLS, com nome de host público próprio (`KC_HOSTNAME` com `https://`) e banco de dados próprio; `OIDC_ISSUER`, os endereços de redirecionamento e as origens do cliente ajustados para o endereço público da interface (HTTPS); `OIDC_DISCOVERY_URL` em HTTPS, ou numa rede privada confiável (as chaves de assinatura dos tokens chegam à API por esse endereço, e quem puder alterá-las no caminho consegue forjar tokens); `API_ALLOWED_HOSTS` com o nome público da API.
- **Segundo fator (OTP)** exigido pelo menos para administradores, e senha do administrador do Keycloak forte e fora do repositório.
- **Histórico do git reescrito** antes do primeiro envio a qualquer servidor ([`docs/publishing.md`](docs/publishing.md)).
- **Busca por POST**: a Bancada manda o texto buscado (que pode ter um CPF) à API num GET, em `?q=`. Hoje nada o registra (nem a API nem o servidor web gravam a query string), mas proxies, balanceadores, WAFs e APMs costumam registrar a URL inteira. Antes de abrir, a busca precisa passar a POST, com o texto no corpo; até lá, nada na frente de `/api/v1/search` pode registrar query strings ([`docs/search.md`](docs/search.md)).

Versões fixadas em [`deploy/VERSIONS.md`](deploy/VERSIONS.md); parâmetros do PostgreSQL em [`deploy/TUNING.md`](deploy/TUNING.md); formato da API em [`docs/api.md`](docs/api.md); cada fonte em [`docs/sources/`](docs/sources/).

## Licença

GNU AGPL-3.0, veja [`LICENSE`](LICENSE).
