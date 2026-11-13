# Fundação e mapa de conexões — desenho

Data: 2026-11-11
Etapa 1 de 6 da plataforma open source de inteligência geopolítica.

Legenda: **[Rafael]** marca o que o dono do projeto decidiu ou pediu.
**[Decisão do desenho]** marca escolhas feitas por quem escreveu este
documento, que o dono pode vetar.

## 1. A plataforma inteira, para contexto

**[Rafael]** Uma plataforma open source que produz inteligência a partir de
dados públicos e fontes confiáveis, para apoiar decisões de investidores,
empresas e clientes políticos. Ela responde a três perguntas:

1. **Quem controla o quê?** Pessoas e empresas que compram empresas que
   compram empresas, até concentrar um mercado.
2. **Quem ganha com uma decisão?** Um ato de governo, seus beneficiados e os
   laços deles com quem decidiu.
3. **O que o evento vai provocar?** Previsão de desdobramentos, inclusive de
   decisões em um país sobre outros.

O objetivo é entender o movimento completo e seus atores reais, e não só a
consequência visível, cruzando passado com presente e conectando pessoas,
cargos e empresas.

**[Rafael]** O primeiro usuário é o próprio Rafael, como analista. Clientes
entram depois.

A plataforma foi dividida em seis etapas, cada uma com desenho e plano
próprios, nesta ordem (aprovada por Rafael ao pedir o plano):

1. **Fundação e mapa de conexões** (este documento).
2. Relatório: texto gerado por IA a partir do mapa, revisão obrigatória pelo
   analista, leitura em camadas na interface e exportação em PDF com
   logomarca.
3. Decisões de governo: atos, votações e contratos ligados ao mapa.
4. Modelos preditivos (serviço em Python isolado, consumindo da fila).
5. Camada internacional.
6. Abertura para clientes: contas, permissões e acesso direto.

Este documento cobre só a etapa 1, mas a etapa 1 precisa deixar a base
pronta para as outras sem retrabalho. Onde isso importa, está dito.

## 2. O que a etapa 1 entrega

Uma bancada de trabalho para o analista, rodando na máquina dele, que:

- coleta bases públicas brasileiras de empresas, sócios, políticos, mandatos
  e doações de campanha;
- junta tudo em um mapa de conexões entre pessoas, empresas, partidos e
  órgãos;
- permite buscar qualquer entidade, navegar pelas conexões, descobrir como
  duas entidades se ligam e ver o grupo de empresas sob um mesmo controlador;
- mostra, para cada conexão, de onde ela veio e quando foi coletada.

### Como saber que funcionou

- Numa máquina limpa com Docker, um comando sobe tudo e outro inicia a
  carga das bases.
- A base nacional inteira está carregada na máquina de referência e as
  consultas cumprem as metas de tempo (ver 6.3).
- Dado o CNPJ de uma empresa, o analista vê sócios e administradores, as
  outras empresas de cada um, e segue a cadeia por vários níveis.
- Dado um político e uma empresa, a bancada encontra os caminhos que os
  ligam (por exemplo: político ← doação ← pessoa → sócia de → empresa).
- Cada conexão na tela abre a fonte: base, registro original, link e data de
  coleta.
- Uma tela mostra a situação de cada base: última coleta, se está em dia ou
  vencida.
- Quando um sócio sai de uma empresa numa coleta nova, o vínculo antigo
  continua consultável com o período em que valeu.

## 3. Fora do escopo, e o que seria fracasso

### Não faz parte da etapa 1

- Geração de relatório, texto por IA e PDF (etapa 2).
- Investigações salvas, anotações e dossiês (etapa 2).
- Atos de governo, votações, contratos públicos (etapa 3).
- Qualquer modelo preditivo ou serviço em Python (etapa 4).
- Dados de outros países (etapa 5).
- Cadastro de clientes, múltiplas organizações, cobrança (etapa 6).
- Implantação em nuvem. A etapa 1 roda localmente; ela só não pode impedir
  a subida posterior.

### Fracasso mesmo funcionando

**[Rafael]** Análises superficiais, sem fonte ou com dados desatualizados.
Na etapa 1 isso se traduz em:

- uma conexão exibida sem fonte e data de coleta;
- um dado vencido apresentado como atual;
- uma cadeia que para no primeiro vínculo quando existe continuação;
- **[Decisão do desenho]** duas pessoas homônimas fundidas em uma só sem
  aviso. Isso cria uma conexão falsa entre pessoas reais, que é o pior erro
  possível para a plataforma, pela consequência jurídica e de reputação;
- **[Decisão do desenho]** uma inferência exibida com a mesma aparência de
  um fato documentado.

## 4. Regras de dados

Estas regras valem para a plataforma inteira e nascem na etapa 1.

1. **Toda afirmação tem procedência.** Cada vínculo e cada atributo guarda:
   a base de origem, a identificação do registro original, o link ou
   arquivo, e a data de coleta. Sem isso, não é gravado.
2. **Nada é apagado.** Quando um dado muda, o anterior é encerrado com data
   de fim e o novo é gravado. Guardam-se dois tempos: o período em que o
   fato valeu no mundo e o momento em que a plataforma soube dele.
3. **Arquivos brutos são preservados.** O que foi baixado de cada base fica
   guardado como veio, para que qualquer afirmação possa ser reconferida.
4. **Fato e inferência são tipos diferentes.** Um vínculo copiado de uma
   base é fato. Um vínculo deduzido pela plataforma (por exemplo, "estas
   duas fichas são a mesma pessoa") é inferência, tem grau de confiança e é
   exibido de forma visivelmente diferente.
5. **Cada base tem prazo de validade.** A configuração da base diz de quanto
   em quanto tempo ela é publicada. Passado o prazo sem nova coleta, a base
   e tudo que vem dela aparecem como vencidos.
6. **Só dados públicos.** Nenhuma fonte que exija burlar acesso, e nada além
   do que a base publica. Os dados envolvem pessoas reais, então a LGPD se
   aplica; uma avaliação jurídica é pré-requisito da etapa 6, não desta.

## 5. Fontes da etapa 1

**[Decisão do desenho]** O conjunto inicial, escolhido por cobrir "sócios,
cargos, doações e aquisições" com dados abertos e estruturados:

| Base | Para quê |
|------|----------|
| Receita Federal, dados abertos do CNPJ | Empresas, estabelecimentos, quadro de sócios e administradores |
| TSE, dados abertos | Candidaturas, partidos, receitas de campanha (doações) |
| Câmara dos Deputados, dados abertos | Deputados, mandatos, partidos |
| Senado Federal, dados abertos | Senadores, mandatos, partidos |
| CVM, dados abertos | Companhias abertas, controladores e participações |

Observações que o construtor precisa confirmar nos formatos atuais de cada
base antes de planejar os conectores, porque mudam com o tempo:

- O quadro de sócios da Receita traz o CPF de pessoas físicas mascarado e
  não traz o percentual de participação. Percentual só existe onde a fonte
  informa (CVM).
- A forma como o TSE publica CPF de candidatos e doadores mudou nos últimos
  anos.
- **Aquisições** não são um tipo de evento próprio nesta etapa. O que a
  etapa 1 entrega é o histórico societário: a data de entrada de cada sócio
  (que a Receita publica) e, a partir da primeira coleta, entradas e saídas
  detectadas entre coletas sucessivas, exibidas na linha do tempo da
  entidade. Uma saída detectada assim é fato com data imprecisa: "saiu
  entre a coleta X e a coleta Y". Não há carga retroativa de arquivos
  antigos da Receita; o histórico de saídas começa na primeira coleta. Um
  evento explícito de "aquisição" e o CADE ficam para etapa posterior.

**Cobertura inicial de cada base:**

- TSE: eleições de 2018, 2020, 2022, 2024 e 2026, todos os cargos
  (federais, estaduais e municipais). O conector deve aceitar anos
  anteriores depois sem mudança de desenho.
- Câmara e Senado: legislatura atual e as duas anteriores.
- Receita e CVM: situação atual publicada, mais o histórico que a própria
  base traz.

Cada base é um **conector** independente com o mesmo contrato: baixar,
guardar o bruto, converter em afirmações com procedência, publicar. Incluir
uma base nova depois deve significar escrever um conector, sem tocar no
resto. Portal da Transparência e CADE são os próximos candidatos.

## 6. Como é feito

### 6.1 Escolhas técnicas

**[Rafael]** Linguagem rápida e com pouca superfície de vulnerabilidade, na
linha de Go ou Java, em vez de Python no núcleo. Fila para requisições e
eventos assíncronos, para a interface nunca travar. Login apartado e sem
vulnerabilidades. Testar bastante localmente e depois subir em ambiente
compatível. Rafael tem experiência com dados e programação e delegou as
decisões técnicas, pedindo que sejam explicadas.

**[Decisão do desenho]**

- **Go** para todo o código de servidor da etapa 1: API, conectores,
  resolução de identidades.
- **PostgreSQL** como único banco. O mapa é guardado em tabelas de entidades
  e vínculos, e as consultas de cadeia usam consultas recursivas. Um banco
  de grafos dedicado só entra se medições mostrarem que o Postgres não dá
  conta.
- **NATS JetStream** como fila. Coletas, resolução de identidades e buscas
  demoradas são tarefas na fila.
- **Keycloak** como serviço de login separado, falando OpenID Connect. O
  código da plataforma nunca vê senha: só valida o token que o Keycloak
  emite. Dois papéis nesta etapa: analista e administrador.
- **Armazenamento de arquivos compatível com S3** para os brutos (MinIO
  localmente).
- **Interface web em TypeScript com React**, textos em português do Brasil,
  conversando só com a API. Visualização do mapa com uma biblioteca de
  grafos pronta.
- **Docker Compose** sobe tudo localmente. Nada no código pode depender de
  estar na máquina local.
- Código e nomes internos em inglês; interface e documentação de uso em
  português.

### 6.2 Partes e responsabilidades

- **API**: única porta de entrada da interface. Valida o token, responde
  buscas e consultas ao mapa, cria tarefas demoradas e informa o andamento.
- **Conectores**: um por base. Consomem pedidos de coleta da fila, baixam,
  guardam o bruto, gravam afirmações com procedência.
- **Resolução de identidades**: decide quando duas fichas de bases
  diferentes são a mesma pessoa ou empresa (ver 6.4).
- **Agendador**: dispara coletas conforme a periodicidade de cada base e
  marca bases vencidas.
- **Interface**: a bancada do analista.

### 6.3 Volume e teste local

**[Rafael]** A etapa 1 termina com a base nacional inteira carregada. Um
mapa parcial não responde "quem controla o quê".

A base da Receita tem dezenas de milhões de empresas; com histórico e
procedência, o mapa chega a centenas de milhões de linhas.

**Máquina de referência** (a do Rafael, medida em 2026-11-11): 6 núcleos,
31 GB de memória, SSD NVMe de 1 TB com cerca de 750 GB livres, Fedora
Linux, Docker instalado. Go e Node.js não estão instalados: compilação e
testes devem rodar em contêineres, ou o plano deve incluir a instalação
como passo explícito.

**[Decisão do desenho]**

- A carga nacional é critério de entrega e deve caber nessa máquina: carga
  em lote, particionamento onde ajudar, e procedência guardada por lote de
  registros de uma mesma coleta (cada vínculo aponta para o lote e para o
  registro de origem) em vez de repetir todos os campos em cada linha.
- O maior risco técnico é a busca de caminhos por consulta recursiva nesse
  volume. O plano deve começar por um teste descartável que carrega a
  Receita nacional e mede busca de caminho e grupo de controle. Se o
  Postgres não atender às metas abaixo, a troca do motor de consulta de
  grafo é decidida ali, antes de construir o resto, e volta ao Rafael.
- Metas na máquina de referência, com a base nacional carregada: busca por
  nome ou CNPJ e página de entidade em até 1 segundo; expansão de vizinhos
  em até 2 segundos; caminhos até profundidade 6 e grupo de controle
  concluídos em até 2 minutos como tarefa em segundo plano; carga nacional
  completa da Receita em até 12 horas.
- Os conectores também aceitam um **recorte** (uma lista de CNPJs mais tudo
  que se liga a eles até N níveis), usado só para desenvolvimento e testes
  automáticos. O repositório traz um recorte pequeno pronto, com dados
  reais públicos, para que os testes não dependam de baixar a base inteira.

### 6.4 Identidades: o ponto mais delicado

Empresas têm CNPJ e se juntam com segurança. Pessoas físicas, não: as bases
trazem nome e, no máximo, parte do CPF.

**[Decisão do desenho]**

- Cada ficha de pessoa em cada base é guardada como veio.
- A resolução propõe que duas fichas são a mesma pessoa com um grau de
  confiança, a partir de nome, pedaço de CPF, e contexto quando houver.
- A regra é a mesma dentro de uma base e entre bases. O quadro de sócios da
  Receita não tem identificador de pessoa, então "as outras empresas deste
  sócio" já é uma junção de fichas. Consequência assumida: **todo salto que
  passa por uma pessoa física é uma inferência de identidade**, com
  confiança, e o modelo de dados e as telas de cadeia e caminho tratam isso
  como caso normal, não como exceção.
- Coincidência forte (nome igual e pedaço de CPF igual) é juntada
  automaticamente com confiança alta, exceto para nomes muito comuns: acima
  de um limiar de frequência do nome na base, a junção não é automática e
  vai para a fila do analista. O limiar é do construtor; o teste é que
  homônimos conhecidos com o mesmo pedaço de CPF não se fundam em silêncio.
- Coincidência só de nome nunca junta sozinha: aparece na bancada como
  "possível mesma pessoa", e o analista confirma ou rejeita.
- A junção é uma inferência (regra 4): tem confiança, pode ser desfeita, e
  toda conexão que depende dela mostra essa dependência.
- A decisão do analista fica registrada e sobrevive a novas coletas.

### 6.5 O que a bancada faz

- **Busca** por nome, CNPJ ou parte deles, com filtro por tipo.
- **Página da entidade**: atributos, vínculos atuais, vínculos encerrados em
  linha do tempo, e a fonte de cada um.
- **Explorador do mapa**: parte de uma entidade e expande vizinhos sob
  demanda, com filtro por tipo de vínculo e por período.
- **Caminhos entre duas entidades**: os caminhos mais curtos até uma
  profundidade escolhida, cada elo marcado como fato ou inferência. Se
  demorar, roda como tarefa e a tela mostra o andamento.
- **Grupo de controle**: a partir de uma empresa, sobe até os controladores
  finais e desce por tudo que eles controlam, em dois modos que o analista
  alterna:
  - *Controle documentado*: segue só vínculos em que o controle é
    sustentado pela fonte: controlador declarado na CVM, participação
    informada acima de 50%, ou sócio único.
  - *Vínculos societários*: segue todo vínculo de sócio, cada um rotulado
    "sócio, participação não informada", sem afirmar controle.
    Administradores aparecem, mas não propagam a cadeia. A expansão para em
    fundos de investimento, em sócios estrangeiros sem cadastro no Brasil e
    em empresas com número de sócios acima de um limite configurável,
    mostrando que parou e por quê, para o grupo não inflar até ficar
    inútil.
- **Fila de identidades**: os "possível mesma pessoa" aguardando decisão.
- **Situação das bases**: última coleta, próxima prevista, em dia ou
  vencida, erros da última execução.

### 6.6 Segurança

- Toda rota da API exige token válido, exceto verificação de saúde.
- Consultas ao banco sempre parametrizadas.
- Segredos fora do repositório, por variáveis de ambiente.
- Contêineres sem usuário root, e só a interface e a API expostas.
- Poucas dependências, com verificação automática de vulnerabilidades
  conhecidas.
- Registro de auditoria: quem consultou qual entidade e quando. Serve à
  segurança agora e à LGPD na etapa 6.

### 6.7 Testes

- Cada conector é testado contra arquivos de exemplo guardados no
  repositório, com o formato real da base.
- Regras de dados (procedência obrigatória, nada apagado, fato separado de
  inferência) têm testes próprios.
- A resolução de identidades tem casos de homônimos que não podem ser
  juntados.
- Um teste de ponta a ponta sobe tudo, carrega um recorte pequeno e percorre
  busca, página de entidade e caminho.

## 7. O que fica para quem constrói

- Esquema exato das tabelas e índices.
- Estrutura do repositório e divisão em serviços ou módulos.
- Bibliotecas Go, biblioteca de grafos da interface, ferramenta de migração
  de banco.
- Desenho visual da bancada, respeitando o que 6.5 pede que ela faça.
- Formato das mensagens na fila.
- Algoritmo e limiares da resolução de identidades, respeitando 6.4.
- Ordem de construção dos conectores (sugestão: Receita primeiro, por ser a
  base do mapa e a de maior volume).

## 8. Em aberto

- **Nome e logomarca da plataforma.** Necessários só na etapa 2.

## 9. Licença

**[Rafael]** AGPL-3.0. Todas as dependências escolhidas precisam ser
compatíveis com ela.
