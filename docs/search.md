# Busca por nome, CNPJ e CPF

Implementação: `internal/search`. Desenho medido: bloco `SEARCH_STRATEGY` em
`docs/spikes/2026-11-receita-path-queries.md`. Este arquivo registra só o que
o produto acrescenta ou muda em relação ao spike e o que o operador precisa
saber.

## Tabelas

- `search_company`: empresas.
- `search_person`: pessoas que ainda têm registro de pessoa. As entidades
  esvaziadas por fusões de identidade ficam de fora.
- `search_other`: estrangeiros (`E`), partidos (`A`) e órgãos públicos
  (`O`).

A ordem é `grau DESC, nome normalizado (C), id`. O grau é o número de
relações abertas, somadas todas as fontes.

## Reconstrução

- Ao fim de toda coleta bem-sucedida, o worker marca as tabelas como sujas
  (`search_state.requested_at`) e as reconstrói por inteiro. A troca é por
  renomeação.
- A marca é gravada mesmo se o worker estiver sendo desligado, com até 5 s
  de prazo, e nunca volta no tempo. Se ainda assim ela se perder, as tabelas
  também ficam sujas quando há mais coletas encerradas (concluídas ou com
  falha) do que no início da última reconstrução. Essa contagem não depende
  do relógio.
- Se a reconstrução falhar ou for interrompida (desligamento), as tabelas
  continuam sujas. O worker refaz a reconstrução ao iniciar, em segundo plano,
  e também após a próxima coleta. Enquanto isso, as tabelas anteriores
  continuam servindo.
- Só uma reconstrução roda por vez. Pedidos que chegam durante uma
  reconstrução geram exatamente mais uma.
- A troca espera as buscas em andamento com `lock_timeout` curto (2 s) e
  tenta de novo, até 10 vezes, por cerca de 1 min.
- **Tempo:** no volume nacional, a reconstrução deve passar dos 819 s do
  spike, porque agora também calcula a normalização e o grau (a medir na
  Tarefa 20). Durante esse tempo, o worker não consome outra mensagem de
  coleta.
- Outros processos (a API) podem usar a lista anterior de palavras
  frequentes por até 30 s depois de uma troca.

## Consulta

- Uma consulta com CPF é uma busca pelo trecho do CPF (ver "Busca por CPF"
  abaixo). Isso é decidido antes de qualquer outra regra.
- Somente dígitos, depois de retirar `. / -` e espaços:
  - 8 ou 14 dígitos: busca exata pela raiz do CNPJ;
  - 3 a 7 dígitos: prefixo da raiz, em ordem de chave (um número de 6
    dígitos é um prefixo de CNPJ, nunca um trecho de CPF);
  - 11 dígitos: CPF (busca pelo trecho);
  - outros tamanhos: nada.
- CNPJ alfanumérico (8 caracteres, ou 12 mais 2 dígitos, misturando letras e
  dígitos): primeiro a busca exata pela raiz. Se não achar, a consulta vira
  busca por nome.
- Limites: no máximo 200 caracteres (`ErrQueryTooLong`) e no máximo 10
  palavras usadas.

## Busca por CPF

A plataforma nunca guarda um CPF inteiro. `person_record.cpf_fragment` guarda
só os **6 dígitos do meio** (posições 4 a 9 dos 11): é o que a Receita publica
dos sócios (`***NNNNNN**`), e os conectores das fontes que publicam o CPF
inteiro (TSE, CVM, Câmara) derivam o trecho na leitura. Por isso a busca por
CPF **casa só o trecho** e devolve **candidatos**: pessoas que têm ao menos um
registro com esse trecho. Nunca confirma que o CPF digitado é daquela pessoa.

- Formas aceitas, separadas das palavras (não coladas a letras ou dígitos):
  - 11 dígitos com ou sem a pontuação usual: `123.456.789-09`,
    `12345678909`, `123 456 789 09` (cada separador é no máximo um entre
    `. - /` e espaço, como na máscara);
  - a forma mascarada das fontes: `***456789**`, `***.456.789-**`.

  Um CPF colado a uma palavra (`MARIA12345678909`) não é tirado da consulta:
  esse termo segue as regras de palavras e é descartado, como antes.
- CPF inteiro: os dois dígitos verificadores são conferidos (módulo 11).
  Dígitos errados, ou todos iguais (`000.000.000-00`, `111…`), são recusados
  com `ErrInvalidCPF` (400 `invalid_cpf`): pega erros de digitação. A forma
  mascarada não tem verificação.
- Dois CPFs diferentes na mesma consulta: `ErrMultipleCPFs` (400
  `multiple_cpfs`). O mesmo CPF duas vezes conta como um.
- **Só o CPF**: as pessoas com algum registro desse trecho, na mesma ordem
  dos níveis de nome (grau desc., nome normalizado, id), com o mesmo limite
  (padrão 20, máximo 50). O grau é contado na hora, a partir dos vínculos
  abertos, com a mesma definição da reconstrução; entre duas reconstruções
  ele pode diferir do das tabelas de busca.
- **CPF e palavras**, em qualquer ordem (`maria silva 123.456.789-09`): o CPF
  sai da consulta **antes** das regras de palavras, e o resto passa por elas
  sem mudança (palavras vazias, números longos, prefixo só na última). O
  resultado é a interseção: entre as pessoas do trecho, as que têm todas
  essas palavras no nome. Se nenhuma palavra sobrar (só palavras vazias, por
  exemplo), vale só o trecho.
- **Tipo**: com `company`, `party`, `public_body` ou `foreign` a resposta é
  vazia (só pessoas têm trecho de CPF); com tipo nenhum ou `person`, só
  pessoas. Um CPF inválido é recusado qualquer que seja o tipo.
- **Quantas pessoas dividem um trecho**: 6 dígitos são 1 milhão de valores
  para cerca de 18,7 milhões de pessoas e 29 milhões de registros na base
  nacional, então um trecho é de dezenas de pessoas em média, e de algumas
  centenas nos piores casos. Daí o aviso na tela e a sugestão de acrescentar
  o nome.
- **Desempenho**: a consulta começa pelo trecho, pelo índice parcial
  `person_record_cpf_fragment_entity (cpf_fragment, entity_id)` (migração
  0022), e calcula grau, nome e palavras só nesse conjunto pequeno. Nunca roda
  a busca por nome inteira para filtrar depois. Testado com 320 pessoas num
  mesmo trecho.
- **O que não é guardado nem devolvido**: a resposta não repete o CPF nem o
  trecho (os nomes continuam mascarados); cada resultado traz `match:
  "cpf_fragment"` e a resposta, `cpf_fragment_search: true`. A auditoria
  guarda só o tipo, a contagem e `cpf_fragment_search: true`, nunca o texto
  buscado. O CPF não aparece em logs (o log de acesso registra só a rota),
  mensagens de erro, métricas ou parâmetros de tarefas. Na Bancada, uma busca
  com CPF fica só na memória da página: nunca no endereço (`?q=`), no
  histórico ou no armazenamento do navegador.
- A proteção dos nomes continua: um nome (de empresa ou pessoa) que contém um
  CPF nunca é achado por esse CPF, porque a busca por CPF só olha o trecho dos
  registros de pessoa, nunca o nome.

## Perdas aceitas, além das do spike

- Consultas com menos de 3 caracteres úteis (`OI`, `3M`) ou só com palavras
  vazias (`LTDA`) são recusadas (`ErrQueryTooShort`). Esses nomes só são
  achados pelo CNPJ.
- A regra de palavras é uma só, definida em SQL (`search_words`, migração
  0008) e usada tanto no índice quanto na consulta. Ficam fora:
  - as palavras vazias;
  - toda palavra com 5 ou mais dígitos seguidos (`MARIA12345678901`);
  - quando o nome (ou a consulta) tem **9 ou mais dígitos no total**, contados
    em qualquer posição, toda palavra que contém algum dígito
    (`SILVA123.456.789-01`, `123 456 789 DV 01`, `1 2 3 4 5 6 7 8 9 0 1`,
    `12.345.678/0001-90`).

  Garantia: **um nome que contém 9 ou mais dígitos no total não é pesquisável
  por nenhum dos seus termos com dígitos**. Como um CPF tem 11 dígitos e um
  CNPJ 14, escritos de qualquer forma, nenhuma consulta, com os dígitos certos
  ou errados, casa um nome desses pelos dígitos. Esse nome só é achado pelas
  palavras sem dígitos, e nunca digitando-o por inteiro.

  O custo: os dígitos desses nomes, e números longos em qualquer nome, não
  são buscáveis. Números curtos em outros nomes continuam buscáveis
  (`POSTO 15`, `C6 BANK`, `99 TAXIS`, `RUA 7 123`).
- Quando uma palavra descartada fica no fim da consulta, a palavra anterior
  casa só inteira. A decisão depende só da forma das palavras, nunca de os
  dígitos estarem certos.
- Uma consulta com 9 ou mais dígitos no total não usa o nível de nome exato.
  Assim, o nível exato também nunca confirma dígitos. (Uma consulta com um
  CPF separado das palavras é uma busca por CPF, acima.)
- Nos nomes exibidos, os CPFs aparecem mascarados, com os separadores
  preservados:
  - 11 dígitos seguidos, mesmo colados a letras, viram `***NNNNNN**`;
  - `ddd?ddd?ddd?dd`, em que cada `?` é no máximo um entre `. - /` e espaço,
    vira, por exemplo, `***.456.789-**`.

  Um CPF escrito de outra forma (`1 2 3 4 5 6 7 8 9 0 1`) aparece como a fonte
  o publicou. Ele continua sem ser pesquisável pelos dígitos.
