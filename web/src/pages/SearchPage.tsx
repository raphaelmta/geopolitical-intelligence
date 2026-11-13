import { useEffect, useId, useRef, useState, type FormEvent } from 'react';
import { Link, useNavigationType, useSearchParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { apiGet } from '../api/client';
import { mayCarryCPF } from '../api/cpf';
import type { SearchResponse } from '../api/types';
import { kindLabel, naturalKeyLabel, usefulLength } from '../api/format';
import { ErrorNotice } from '../components/ErrorNotice';
import { usePageHeading } from '../usePageHeading';

const LIMIT = 50;
const MAX_LENGTH = 200;

interface Search {
  q: string;
  kind: string;
}

/**
 * The last search carrying a CPF, in this tab's memory only (gone on reload
 * or sign-out): brought back when the user returns with Back to the search
 * page, whose URL never holds it.
 */
let lastCPFSearch: Search | null = null;

/** Test support: forget the remembered CPF search. */
export function forgetCPFSearch() {
  lastCPFSearch = null;
}

/**
 * Search by name, CNPJ or CPF. Submits on Enter or "Buscar" (no search while
 * typing: every search is audited). The query lives in the URL (?q=&tipo=)
 * so back/forward and links work — except a query that may carry a CPF
 * (mayCarryCPF): that one stays in memory, never in the URL, the history or
 * storage. A CPF that arrives in the URL (an old link) is moved to memory
 * and removed from the URL at once.
 */
export function SearchPage() {
  const [params, setParams] = useSearchParams();
  const navigation = useNavigationType();
  const urlQ = params.get('q') ?? '';
  const urlKind = params.get('tipo') ?? '';
  const [memory, setMemory] = useState<Search | null>(() => {
    if (mayCarryCPF(urlQ)) return { q: urlQ, kind: urlKind };
    if (!urlQ && navigation === 'POP' && lastCPFSearch?.kind === urlKind) return lastCPFSearch;
    return null;
  });
  const memoryRef = useRef(memory);
  memoryRef.current = memory;
  const active: Search = memory ?? { q: urlQ, kind: urlKind };
  const { q, kind } = active;
  const [text, setText] = useState(q);
  const [kindChoice, setKindChoice] = useState(kind);
  const [tooShort, setTooShort] = useState(false);
  const hintId = useId();
  const h1 = usePageHeading('Busca');
  // Back/forward changes the URL: show the search it holds. A URL with a
  // CPF is replaced by one without it (the search moves to memory).
  useEffect(() => {
    if (mayCarryCPF(urlQ)) {
      const moved = { q: urlQ, kind: urlKind };
      lastCPFSearch = moved;
      setMemory(moved);
      setText(urlQ);
      setKindChoice(urlKind);
      setParams(urlKind ? { tipo: urlKind } : {}, { replace: true });
      return;
    }
    if (urlQ) {
      setMemory(null);
      setText(urlQ);
      setKindChoice(urlKind);
    } else if (!memoryRef.current) {
      setText('');
      setKindChoice(urlKind);
    }
  }, [urlQ, urlKind]);
  const enabled = usefulLength(q) >= 3;

  const result = useQuery({
    queryKey: ['search', q, kind],
    queryFn: () => {
      const p = new URLSearchParams({ q, limit: String(LIMIT) });
      if (kind) p.set('kind', kind);
      return apiGet<SearchResponse>(`/search?${p}`);
    },
    enabled,
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const t = text.trim();
    if (usefulLength(t) < 3) {
      setTooShort(true);
      return;
    }
    setTooShort(false);
    if (mayCarryCPF(t)) {
      const s = { q: t, kind: kindChoice };
      lastCPFSearch = s;
      memoryRef.current = s;
      setMemory(s);
      // Leave the URL without the previous query (and with the type).
      if (urlQ || urlKind !== kindChoice) setParams(kindChoice ? { tipo: kindChoice } : {});
      return;
    }
    lastCPFSearch = null;
    memoryRef.current = null;
    setMemory(null);
    const next = new URLSearchParams({ q: t });
    if (kindChoice) next.set('tipo', kindChoice);
    setParams(next);
  };

  const cpfSearch = result.isSuccess && result.data.cpf_fragment_search === true;
  const tooCommon = result.isSuccess && result.data.cpf_fragment_too_common === true;
  // Candidates only (no company found by the words besides them).
  const onlyCandidates = result.isSuccess && result.data.items.every((i) => i.match === 'cpf_fragment');
  const hasWords = usefulLength(q.normalize('NFKC').replace(/[\d*]/g, '')) > 0;

  return (
    <>
      <h1 ref={h1} tabIndex={-1}>
        Busca
      </h1>
      <form className="search" role="search" onSubmit={submit}>
        <div className="search__field">
          <label htmlFor="busca-texto">Nome, CNPJ ou CPF</label>
          <input
            id="busca-texto"
            type="search"
            placeholder="nome, CNPJ ou CPF"
            value={text}
            maxLength={MAX_LENGTH}
            autoComplete="off"
            spellCheck={false}
            aria-describedby={hintId}
            onChange={(e) => setText(e.target.value)}
          />
        </div>
        <div className="search__field search__field--kind">
          <label htmlFor="busca-tipo">Tipo</label>
          <select id="busca-tipo" value={kindChoice} onChange={(e) => setKindChoice(e.target.value)}>
            <option value="">Todos</option>
            <option value="company">Empresa</option>
            <option value="person">Pessoa</option>
            <optgroup label="Outros">
              <option value="party">Partido</option>
              <option value="public_body">Órgão público</option>
              <option value="foreign">Estrangeiro</option>
            </optgroup>
          </select>
        </div>
        <button type="submit" className="button button--primary">
          Buscar
        </button>
      </form>
      <p id={hintId} className={tooShort ? 'hint hint--warning' : 'hint'} role="status">
        {tooShort
          ? 'Digite pelo menos 3 caracteres'
          : 'Pressione Enter para buscar. CNPJ com 8 ou 14 dígitos; CPF com 11 dígitos, que pode vir com o nome.'}
      </p>

      <SearchHelp />

      {enabled && (
        <section aria-live="polite" className="results">
          {result.isPending && <p role="status">Buscando…</p>}
          {result.isError && <ErrorNotice error={result.error} onRetry={() => void result.refetch()} />}
          {cpfSearch && <CPFNote withCompanies={!onlyCandidates} />}
          {result.isSuccess &&
            (result.data.items.length === 0 ? (
              tooCommon ? (
                <p className="empty" role="status">
                  Esse trecho de CPF é genérico demais nas fontes (aparece em milhares de registros). Acrescente o nome
                  para buscar.
                </p>
              ) : cpfSearch ? (
                <p className="empty">
                  {kind && kind !== 'person'
                    ? `A busca por CPF só encontra pessoas; nada com o tipo ${kindLabel(kind)}. Procurando uma empresa cujo nome tem um CPF? Busque só pelas palavras do nome.`
                    : 'Nenhuma pessoa com esse trecho do CPF' + (hasWords ? ' e essas palavras.' : '.')}
                </p>
              ) : (
                <p className="empty">
                  Nenhum resultado para “{q}”{kind ? ` com o tipo ${kindLabel(kind)}` : ''}. Veja abaixo de
                  “Como a busca funciona” o que ela não encontra.
                </p>
              )
            ) : (
              <table className="table">
                <caption>
                  {cpfSearch && onlyCandidates ? 'Pessoas candidatas para “' : 'Resultados para “'}
                  {q}” ({result.data.items.length}
                  {result.data.items.length >= LIMIT ? ', no máximo; refine a busca' : ''})
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Tipo</th>
                    <th scope="col">Nome</th>
                    <th scope="col">CNPJ básico / chave</th>
                  </tr>
                </thead>
                <tbody>
                  {result.data.items.map((item) => (
                    <tr key={item.id}>
                      <td>{kindLabel(item.kind)}</td>
                      <td>
                        <Link to={`/entidade/${encodeURIComponent(item.id)}`}>{item.name}</Link>
                        {item.match === 'cpf_fragment' && (
                          <>
                            {' '}
                            <span className="tag tag--identity">coincide com o trecho do CPF</span>
                          </>
                        )}
                      </td>
                      <td className="mono">{naturalKeyLabel(item.kind, item.natural_key)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ))}
        </section>
      )}
    </>
  );
}

/** Shown above every CPF search's results; cannot be dismissed. */
function CPFNote({ withCompanies }: { withCompanies: boolean }) {
  return (
    <div className="notice notice--caveat" role="note" aria-label="Sobre a busca por CPF">
      <p>
        <strong>Busca por trecho do CPF.</strong> As fontes publicam só os 6 dígitos do meio do CPF. As pessoas marcadas
        “coincide com o trecho do CPF” têm um CPF que <strong>coincide nesse trecho</strong> — não é a confirmação de que
        o CPF digitado seja delas, e várias pessoas podem ter o mesmo trecho.
      </p>
      {withCompanies && (
        <p>As empresas da lista foram achadas só pelas palavras do nome; o CPF não é usado para elas.</p>
      )}
      <p>Para restringir, acrescente o nome à busca (por exemplo: maria silva e o CPF).</p>
    </div>
  );
}

function SearchHelp() {
  return (
    <details className="help">
      <summary>Como a busca funciona</summary>
      <ul>
        <li>
          Casa <strong>palavras inteiras</strong>; só a última palavra digitada pode ser o começo de uma palavra
          (“PETRO” acha “PETROBRAS” se for a última). Não acha um trecho no meio de uma palavra.
        </li>
        <li>
          Não tolera <strong>erros de digitação</strong> nem grafias diferentes. Acentos e maiúsculas não importam.
        </li>
        <li>
          <strong>CNPJ</strong>: digite 8 ou 14 dígitos (pontuação é ignorada). De 3 a 7 dígitos busca o começo do CNPJ
          básico.
        </li>
        <li>
          Nomes com menos de 3 caracteres úteis (“OI”, “3M”) ou feitos só de palavras comuns (“LTDA”) só são achados
          pelo CNPJ.
        </li>
        <li>
          Números longos dentro de nomes não são pesquisáveis: palavras com 5 ou mais dígitos seguidos e, em nomes com 9
          ou mais dígitos no total, toda palavra com dígito. Busque esses nomes pelas palavras sem números.
        </li>
        <li>
          <strong>CPF</strong>: digite os 11 dígitos (com ou sem pontuação) ou o trecho mascarado (***NNNNNN**), sozinho
          ou com o nome. As fontes publicam só os <strong>6 dígitos do meio</strong> do CPF, e só eles são guardados: o
          resultado lista as pessoas cujo CPF coincide nesse trecho — <strong>candidatas, não uma confirmação</strong>.
          Várias pessoas podem ter o mesmo trecho; acrescente o nome para restringir. Um CPF com dígitos verificadores
          errados é recusado. Um número de 11 dígitos é sempre lido como CPF (um telefone, por exemplo, costuma ser
          recusado como CPF inválido). Com palavras e sem tipo, as empresas achadas por essas palavras vêm depois das
          pessoas. O CPF digitado não vai para o endereço da página nem para o histórico.
        </li>
        <li>
          Um CPF escrito dentro de um nome nunca é pesquisável por ele, e os nomes aparecem com o CPF mascarado
          (***NNNNNN**).
        </li>
        <li>Até 200 caracteres e 10 palavras; no máximo {LIMIT} resultados, das entidades mais conectadas para as menos.</li>
      </ul>
    </details>
  );
}
