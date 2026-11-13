import { useEffect, useId, useState, type FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { apiGet } from '../api/client';
import type { SearchResponse } from '../api/types';
import { kindLabel, naturalKeyLabel, usefulLength } from '../api/format';
import { ErrorNotice } from '../components/ErrorNotice';
import { usePageHeading } from '../usePageHeading';

const LIMIT = 50;
const MAX_LENGTH = 200;

/**
 * Search by name or CNPJ. Submits on Enter or "Buscar" (no search while
 * typing: every search is audited). The query lives in the URL (?q=&tipo=)
 * so back/forward and links work.
 */
export function SearchPage() {
  const [params, setParams] = useSearchParams();
  const q = params.get('q') ?? '';
  const kind = params.get('tipo') ?? '';
  const [text, setText] = useState(q);
  const [kindChoice, setKindChoice] = useState(kind);
  const [tooShort, setTooShort] = useState(false);
  const hintId = useId();
  const h1 = usePageHeading('Busca');
  // Back/forward changes the URL: show the search it holds.
  useEffect(() => {
    setText(q);
    setKindChoice(kind);
  }, [q, kind]);
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
    const next = new URLSearchParams({ q: t });
    if (kindChoice) next.set('tipo', kindChoice);
    setParams(next);
  };

  return (
    <>
      <h1 ref={h1} tabIndex={-1}>
        Busca
      </h1>
      <form className="search" role="search" onSubmit={submit}>
        <div className="search__field">
          <label htmlFor="busca-texto">Nome ou CNPJ</label>
          <input
            id="busca-texto"
            type="search"
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
        {tooShort ? 'Digite pelo menos 3 caracteres' : 'Pressione Enter para buscar. CNPJ com 8 ou 14 dígitos.'}
      </p>

      <SearchHelp />

      {enabled && (
        <section aria-live="polite" className="results">
          {result.isPending && <p role="status">Buscando…</p>}
          {result.isError && <ErrorNotice error={result.error} onRetry={() => void result.refetch()} />}
          {result.isSuccess &&
            (result.data.items.length === 0 ? (
              <p className="empty">
                Nenhum resultado para “{q}”{kind ? ` com o tipo ${kindLabel(kind)}` : ''}. Veja abaixo de
                “Como a busca funciona” o que ela não encontra.
              </p>
            ) : (
              <table className="table">
                <caption>
                  Resultados para “{q}” ({result.data.items.length}
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
          CPF não é pesquisável, e uma busca nunca confirma a ligação entre um CPF e um nome. Os nomes aparecem com o
          CPF mascarado (***NNNNNN**).
        </li>
        <li>Até 200 caracteres e 10 palavras; no máximo {LIMIT} resultados, das entidades mais conectadas para as menos.</li>
      </ul>
    </details>
  );
}
