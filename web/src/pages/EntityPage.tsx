import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { Link, useLocation, useNavigate, useParams } from 'react-router';
import { useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError, apiGet } from '../api/client';
import type {
  CompanyVersion,
  Edge,
  EntityRef,
  EntityResponse,
  PersonRecord,
  PossibleSame,
  RelationshipsPage,
} from '../api/types';
import { attrLabels } from '../api/attrs';
import {
  closedLabel,
  formatDate,
  formatMoney,
  formatConfidence,
  CONFIDENCE_MISSING,
  formatTimestamp,
  kindLabel,
  naturalKeyLabel,
  REL_TYPES,
  relationLabel,
  relationTypeName,
  sourceName,
} from '../api/format';
import { isClosed } from '../api/edges';
import { EdgeBadge } from '../components/EdgeBadge';
import { EdgeLegend } from '../components/EdgeLegend';
import { ErrorNotice } from '../components/ErrorNotice';
import { GraphDetails } from '../components/GraphDetails';
import { KindIcon } from '../components/KindIcon';
import { GraphPanel, graphStartsOpen } from '../components/GraphView';
import { entityIdOf, nodeElementId, toElements } from '../components/graphStyle';
import { MergeDialog, SplitDialog } from '../components/IdentityDialogs';
import { ProvenancePopover } from '../components/ProvenancePopover';
import { Timeline } from '../components/Timeline';
import { usePageHeading } from '../usePageHeading';

/** The API returns at most this many "possible same" candidates. */
const POSSIBLE_SAME_MAX = 20;

export function EntityPage() {
  const { id = '' } = useParams();
  if (!/^[1-9]\d{0,17}$/.test(id)) return <EntityNotFound />;
  // key: a new entity starts with a fresh filter and fresh paging.
  return <Entity key={id} id={id} />;
}

function EntityNotFound() {
  const h1 = usePageHeading('Entidade não encontrada');
  return (
    <>
      <h1 ref={h1} tabIndex={-1}>
        Entidade não encontrada
      </h1>
      <p>
        Não há entidade com este número. <Link to="/">Voltar para a busca</Link>.
      </p>
    </>
  );
}

/** Location state set after a merge that moved the records to another entity. */
interface IdentityState {
  identityNotice?: 'merged';
}

function Entity({ id }: { id: string }) {
  const [type, setType] = useState('');
  const typeId = useId();
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const merged = (location.state as IdentityState | null)?.identityNotice === 'merged';
  const [notice, setNotice] = useState<ReactNode>(
    merged ? 'As fichas foram juntadas nesta entidade. Você pode separar alguma depois, em “Separar ficha”.' : null,
  );
  const [splitting, setSplitting] = useState<PersonRecord | null>(null);
  const [merging, setMerging] = useState<PossibleSame | null>(null);
  const noticeRef = useRef<HTMLDivElement>(null);
  // The notice is read once: drop it from the history entry so back/forward
  // or a reload does not show it again.
  useEffect(() => {
    if (merged) navigate(location.pathname + location.search + location.hash, { replace: true, state: null });
  }, []); // once, on arrival
  const q = useQuery({
    queryKey: ['entity', id],
    queryFn: () => apiGet<EntityResponse>(`/entities/${encodeURIComponent(id)}`),
  });
  const h1 = usePageHeading(q.data?.entity.name ?? `Entidade nº ${id}`, !q.isPending);

  if (q.isPending) return <p role="status">Carregando a entidade…</p>;
  if (q.isError) {
    if (q.error instanceof ApiError && q.error.code === 'not_found') return <EntityNotFound />;
    return (
      <>
        <h1 ref={h1} tabIndex={-1}>
          Entidade nº {id}
        </h1>
        <ErrorNotice error={q.error} onRetry={() => void q.refetch()} />
      </>
    );
  }

  const { entity, versions, records, records_truncated, possible_same } = q.data;
  const key = naturalKeyLabel(entity.kind, entity.natural_key);
  const numericId = Number(id);
  // Only people are resolved by identity; companies, parties and bodies are
  // keyed by registry number.
  const isPerson = entity.kind === 'person';

  // Records moved between entities: everything that shows them reloads.
  const reloadIdentity = () => {
    void queryClient.invalidateQueries({ queryKey: ['entity'] });
    void queryClient.invalidateQueries({ queryKey: ['relationships'] });
    void queryClient.invalidateQueries({ queryKey: ['proposals'] });
  };
  const splitDone = (newId: number) => {
    setSplitting(null);
    setNotice(
      <>
        <p>Ficha separada: ela agora forma outra pessoa, e esta página foi recarregada.</p>
        <p>
          <Link to={`/entidade/${encodeURIComponent(newId)}`}>Abrir a entidade nº {newId}</Link>
        </p>
      </>,
    );
    reloadIdentity();
  };
  const mergeDone = (resultId: number) => {
    setMerging(null);
    reloadIdentity();
    if (String(resultId) === id) {
      setNotice('As fichas foram juntadas nesta entidade. Você pode separar alguma depois, em “Separar ficha”.');
    } else {
      navigate(`/entidade/${encodeURIComponent(resultId)}`, { state: { identityNotice: 'merged' } satisfies IdentityState });
    }
  };

  return (
    <article className="entity">
      <header className="entity__head">
        <KindIcon kind={entity.kind} size="large" />
        <div>
          <p className="eyebrow">{kindLabel(entity.kind)}</p>
          <h1 ref={h1} tabIndex={-1}>
            {entity.name}
          </h1>
          {key && (
            <p className="entity__key">
              {entity.kind === 'company' ? 'CNPJ básico ' : 'Chave '}
              <span className="mono">{key}</span>
            </p>
          )}
        </div>
      </header>

      <nav className="explore" aria-label="Explorar">
        <ul className="explore__list">
          <li>
            <Link className="button button--primary" to={`/mapa/${encodeURIComponent(id)}`}>
              Ver no mapa
            </Link>
          </li>
          <li>
            <Link className="button" to={`/caminhos?a=${encodeURIComponent(id)}`}>
              Caminhos a partir daqui
            </Link>
          </li>
          {entity.kind === 'company' && (
            <li>
              <Link className="button" to={`/grupo/${encodeURIComponent(id)}`}>
                Grupo de controle
              </Link>
            </li>
          )}
        </ul>
      </nav>

      {notice && (
        <div ref={noticeRef} className="notice" role="status" tabIndex={-1}>
          {typeof notice === 'string' ? <p>{notice}</p> : notice}
        </div>
      )}

      <ConnectionsSection entity={entity} />

      <EdgeLegend />

      {(isPerson || possible_same.length > 0) && (
        <PossibleSameSection items={possible_same} onMerge={isPerson ? setMerging : undefined} />
      )}

      {versions.length > 0 && <VersionsSection versions={versions} />}

      {records.length > 0 && (
        <RecordsSection
          records={records}
          truncated={records_truncated}
          onSplit={isPerson && records.length >= 2 ? setSplitting : undefined}
        />
      )}

      {splitting && (
        <SplitDialog
          entityId={numericId}
          entityName={entity.name}
          recordId={splitting.id}
          onClose={() => setSplitting(null)}
          onDone={splitDone}
          fallbackFocus={() => noticeRef.current ?? h1.current}
        />
      )}
      {merging && (
        <MergeDialog
          entityId={numericId}
          candidate={merging}
          onClose={() => setMerging(null)}
          onDone={mergeDone}
          fallbackFocus={() => noticeRef.current ?? h1.current}
        />
      )}

      <div className="filter">
        <label htmlFor={typeId}>Tipo de vínculo</label>
        <select id={typeId} value={type} onChange={(e) => setType(e.target.value)}>
          <option value="">Todos os tipos</option>
          {REL_TYPES.map((t) => (
            <option key={t} value={t}>
              {relationTypeName(t)}
            </option>
          ))}
        </select>
      </div>

      <RelationshipsSection entityId={numericId} state="open" type={type} title="Vínculos atuais" />
      <RelationshipsSection entityId={numericId} state="closed" type={type} title="Vínculos encerrados" />
    </article>
  );
}

function PossibleSameSection({
  items,
  onMerge,
}: {
  items: PossibleSame[];
  /** Absent: no merge action (not a person). */
  onMerge?: (c: PossibleSame) => void;
}) {
  const h = useId();
  return (
    <section aria-labelledby={h} className="section">
      <h2 id={h}>Possível mesma pessoa</h2>
      {items.length === 0 ? (
        <p className="muted">Nenhuma outra ficha com este nome normalizado foi encontrada.</p>
      ) : (
        <>
          <p>
            Estas entidades têm o mesmo nome normalizado. Elas <strong>não foram unidas automaticamente</strong>: são
            pessoas distintas até que alguém confirme o contrário.
          </p>
          <ul className="plain-list">
            {items.map((c) => (
              <li key={c.entity_id}>
                <Link to={`/entidade/${encodeURIComponent(c.entity_id)}`}>{c.name}</Link>{' '}
                <span className="muted">
                  · {c.records} {c.records === 1 ? 'registro' : 'registros'}
                  {c.shared_fragment && ' · mesmo trecho do CPF'}
                </span>
                {onMerge && (
                  <>
                    {' '}
                    <button
                      type="button"
                      className="link-button"
                      aria-describedby={`${h}-merge-${c.entity_id}`}
                      onClick={() => onMerge(c)}
                    >
                      Juntar a esta pessoa
                    </button>
                    <span id={`${h}-merge-${c.entity_id}`} className="visually-hidden">
                      {c.name}, entidade nº {c.entity_id}
                    </span>
                  </>
                )}
              </li>
            ))}
          </ul>
          {items.length >= POSSIBLE_SAME_MAX && (
            <p className="muted small">Mostrando as {POSSIBLE_SAME_MAX} primeiras; pode haver mais.</p>
          )}
        </>
      )}
    </section>
  );
}

function VersionsSection({ versions }: { versions: CompanyVersion[] }) {
  const h = useId();
  // Current (open) versions first, then closed ones, each newest first as sent.
  const ordered = [...versions.filter((v) => v.open), ...versions.filter((v) => !v.open)];
  return (
    <section aria-labelledby={h} className="section">
      <h2 id={h}>Cadastro</h2>
      <Timeline
        label="Versões cadastrais"
        items={ordered.map((v) => ({
          key: v.id,
          title: v.name,
          current: v.open,
          children: (
            <dl className="facts facts--inline">
              <dt>Situação</dt>
              <dd>{v.status || '—'}</dd>
              <dt>Natureza jurídica</dt>
              <dd>{v.legal_nature || '—'}</dd>
              <dt>UF / município</dt>
              <dd>
                {v.uf || '—'} / {v.city || '—'}
              </dd>
              {v.capital != null && (
                <>
                  <dt>Capital social</dt>
                  <dd>{formatMoney(v.capital)}</dd>
                </>
              )}
              <dt>Período</dt>
              <dd>
                {v.first_seen_reference_date && (
                  <>
                    {seenRange(v.first_seen_reference_date, v.last_seen_reference_date)}
                    {v.closed_reference_date && ` · encerrada em ${formatDate(v.closed_reference_date)}`}
                    <br />
                  </>
                )}
                <span className={v.first_seen_reference_date ? 'muted small' : undefined}>
                  vista da coleta {v.first_collection_id} à coleta {v.last_seen_collection_id ?? v.first_collection_id}
                  {v.closed_collection_id != null && ` · encerrada na coleta ${v.closed_collection_id}`}
                </span>
              </dd>
              <dt>Registro</dt>
              <dd>
                <code className="break">{v.record_ref}</code> · gravado em {formatTimestamp(v.recorded_at)}
              </dd>
            </dl>
          ),
        }))}
      />
      <p className="muted small">Códigos como os publica a Receita Federal (município pelo código da Receita).</p>
    </section>
  );
}

/** "vista de 01/09/2026 a 01/10/2026" (or "em" when both dates are the same). */
function seenRange(first: string, last: string | null | undefined): string {
  const a = formatDate(first);
  const b = formatDate(last ?? first);
  return a === b ? `vista em ${a}` : `vista de ${a} a ${b}`;
}

function RecordsSection({
  records,
  truncated,
  onSplit,
}: {
  records: PersonRecord[];
  truncated: boolean;
  /** Absent: no split action (not a person, or a single record). */
  onSplit?: (r: PersonRecord) => void;
}) {
  const h = useId();
  return (
    <section aria-labelledby={h} className="section">
      <h2 id={h}>Registros que formam esta pessoa</h2>
      <div className="table-wrap">
        <table className="table">
          <caption>
            {records.length} {records.length === 1 ? 'registro' : 'registros'}
            {truncated ? ' (lista cortada; há mais)' : ''}
          </caption>
          <thead>
            <tr>
              <th scope="col">Fonte</th>
              <th scope="col">Contexto</th>
              <th scope="col">Nome no registro</th>
              <th scope="col">CPF (mascarado)</th>
              <th scope="col">Identidade</th>
              <th scope="col">Registro</th>
              {onSplit && (
                <th scope="col">
                  <span className="visually-hidden">Ações</span>
                </th>
              )}
            </tr>
          </thead>
          <tbody>
            {records.map((r) => (
              <tr key={r.id}>
                <td>{sourceName(r.source_key)}</td>
                <td className="mono">{r.context_key}</td>
                <td>{r.name}</td>
                <td className="mono">{r.cpf_masked ?? '—'}</td>
                <td>
                  {formatConfidence(r.identity_confidence) ?? CONFIDENCE_MISSING}
                  {r.locked && ' · fixado por analista'}
                </td>
                <td>
                  <RecordProvenance record={r} />
                </td>
                {onSplit && (
                  <td>
                    <button
                      type="button"
                      className="link-button"
                      aria-describedby={`${h}-split-${r.id}`}
                      onClick={() => onSplit(r)}
                    >
                      Separar ficha
                    </button>
                    <span id={`${h}-split-${r.id}`} className="visually-hidden">
                      {sourceName(r.source_key)} {r.context_key}
                    </span>
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

/** Where a person record was first read: file and line, and that collection. */
function RecordProvenance({ record: r }: { record: PersonRecord }) {
  if (!r.record_ref && r.first_collection_id == null) return <span className="muted small">não informado</span>;
  const ref = formatDate(r.first_seen_reference_date);
  return (
    <span className="small">
      {r.record_ref && <code className="break">{r.record_ref}</code>}
      {r.first_collection_id != null && (
        <span className="muted">
          {r.record_ref ? ' · ' : ''}1ª coleta nº {r.first_collection_id}
          {ref && `, referência ${ref}`}
        </span>
      )}
    </span>
  );
}

/**
 * The relationships of an entity, page by page. The map section and the
 * "Vínculos atuais" table share it: with the same arguments they share the
 * query (one request per page).
 */
function useRelationships(entityId: number, state: 'open' | 'closed', type: string) {
  return useInfiniteQuery({
    queryKey: ['relationships', entityId, state, type],
    queryFn: ({ pageParam }) => {
      const p = new URLSearchParams({ state });
      if (type) p.set('type', type);
      if (pageParam) p.set('cursor', pageParam);
      return apiGet<RelationshipsPage>(`/entities/${encodeURIComponent(entityId)}/relationships?${p}`);
    },
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
  });
}

/**
 * "Mapa de conexões": the entity at the centre and its current relationships
 * of the first page, drawn as on the map page (same elements, styles and
 * legend: fact or inference, closed, stale, merged identity), from the data
 * the "Vínculos atuais" table already loads (no filter). Never more than
 * that first page, so a hub cannot freeze the page; the full map
 * (/mapa/:id) expands, filters and pages. No relationships: no drawing, and
 * the drawing's code (a lazy chunk) is never loaded.
 */
function ConnectionsSection({ entity }: { entity: EntityRef }) {
  const h = useId();
  const navigate = useNavigate();
  const q = useRelationships(entity.id, 'open', '');
  const [selected, setSelected] = useState<string | null>(null);
  const afterRef = useRef<HTMLParagraphElement>(null);
  const first = q.data?.pages[0];
  const edges = useMemo(() => first?.items ?? [], [first]);
  const more = Boolean(first?.next_cursor);
  const elements = useMemo(() => toElements(edges, [entity], entity.id), [edges, entity]);
  const nodes = useMemo(() => {
    const m = new Map<number, EntityRef>([[entity.id, entity]]);
    for (const e of edges) {
      m.set(e.src.id, e.src);
      m.set(e.dst.id, e.dst);
    }
    return m;
  }, [edges, entity]);
  const mapLink = `/mapa/${encodeURIComponent(entity.id)}`;

  const open = (elementId: string) => {
    const id = entityIdOf(elementId);
    if (id != null && id !== entity.id) navigate(`/entidade/${encodeURIComponent(id)}`);
  };

  let body: ReactNode;
  if (q.isPending) {
    body = graphStartsOpen() ? (
      <div className="graph-placeholder" role="status">
        Carregando o mapa de conexões…
      </div>
    ) : (
      <p role="status">Carregando o mapa de conexões…</p>
    );
  } else if (q.isError && edges.length === 0) {
    body = <p className="muted">O mapa não pôde ser carregado; veja os vínculos atuais abaixo.</p>;
  } else if (edges.length === 0) {
    body = <p className="muted">Nenhum vínculo atual: não há conexões para desenhar.</p>;
  } else {
    body = (
      <>
        <p className="connections__summary">
          {nodes.size - 1} {nodes.size - 1 === 1 ? 'entidade ligada' : 'entidades ligadas'} por {edges.length}{' '}
          {edges.length === 1 ? 'vínculo atual' : 'vínculos atuais'}
          {more && (
            <>
              {' '}
              — <strong>mostrando os {edges.length} primeiros; há mais</strong>.{' '}
              <Link to={mapLink}>Abra o mapa completo</Link> para ver todos, filtrar e expandir.
            </>
          )}
        </p>
        <p className="small skip-inline">
          <button type="button" className="link-button" onClick={() => afterRef.current?.focus()}>
            Pular o mapa
          </button>
        </p>
        <GraphPanel
          className="graph-panel--embedded"
          label={`Mapa de conexões de ${entity.name}: ${nodes.size} entidades e ${edges.length} vínculos`}
          hint="Clique numa entidade ou numa linha para ver os detalhes ao lado; duplo clique numa entidade abre a página dela. Arraste para mover, role para aproximar. As tabelas de vínculos abaixo têm tudo o que o gráfico mostra."
          elements={elements}
          selectedId={selected}
          onSelect={setSelected}
          onActivate={open}
          onClear={() => setSelected(null)}
          details={
            <GraphDetails
              edges={edges}
              entities={nodes}
              selected={selected}
              onSelect={setSelected}
              actions={(n) =>
                n.id === entity.id ? (
                  <Link className="button button--small" to={mapLink}>
                    Explorar no mapa completo
                  </Link>
                ) : (
                  <>
                    <Link
                      className="button button--primary button--small"
                      to={`/entidade/${encodeURIComponent(n.id)}`}
                      aria-label={`Abrir a página de ${n.name}`}
                    >
                      Abrir página
                    </Link>
                    <Link className="button button--small" to={`/mapa/${encodeURIComponent(n.id)}`} aria-label={`mapa a partir de ${n.name}`}>
                      Mapa a partir daqui
                    </Link>
                  </>
                )
              }
            />
          }
        />
        <details className="connections__nodes">
          <summary>Entidades no mapa ({nodes.size - 1})</summary>
          <ul className="plain-list">
            {[...nodes.values()]
              .filter((n) => n.id !== entity.id)
              .map((n) => (
                <li key={n.id} className={selected === nodeElementId(n.id) ? 'is-selected' : undefined}>
                  {n.name} <span className="muted small">· {kindLabel(n.kind)}</span>{' '}
                  <button
                    type="button"
                    className="link-button"
                    aria-pressed={selected === nodeElementId(n.id)}
                    aria-label={`Destacar no gráfico ${n.name}`}
                    onClick={() => setSelected(nodeElementId(n.id))}
                  >
                    destacar
                  </button>{' '}
                  ·{' '}
                  <Link to={`/entidade/${encodeURIComponent(n.id)}`} aria-label={`Abrir ${n.name}`}>
                    Abrir
                  </Link>
                </li>
              ))}
          </ul>
        </details>
      </>
    );
  }

  return (
    <section aria-labelledby={h} className="section connections">
      <h2 id={h}>Mapa de conexões</h2>
      {body}
      <p ref={afterRef} tabIndex={-1} className="small">
        <Link to={mapLink}>Abrir mapa completo</Link>
        <span className="muted"> — expandir, filtrar por tipo e data, e carregar todos os vínculos.</span>
      </p>
    </section>
  );
}

function RelationshipsSection({
  entityId,
  state,
  type,
  title,
}: {
  entityId: number;
  state: 'open' | 'closed';
  type: string;
  title: string;
}) {
  const h = useId();
  const q = useRelationships(entityId, state, type);
  // Pages are appended; a row seen on an earlier page is not repeated.
  const seen = new Set<number>();
  const edges = (q.data?.pages.flatMap((p) => p.items) ?? []).filter(
    (e) => !seen.has(e.relationship_id) && Boolean(seen.add(e.relationship_id)),
  );
  const closed = state === 'closed';

  return (
    <section aria-labelledby={h} className="section">
      <h2 id={h}>{title}</h2>
      {closed && (
        <p className="muted small section-note">
          A data de encerramento é o primeiro dia em que o vínculo já não valia.
        </p>
      )}
      {q.isPending && <p role="status">Carregando vínculos…</p>}
      {q.isError && edges.length === 0 && <ErrorNotice error={q.error} onRetry={() => void q.refetch()} />}
      {q.isSuccess && edges.length === 0 && (
        <p className="muted">{closed ? 'Nenhum vínculo encerrado' : 'Nenhum vínculo atual'}{type ? ' deste tipo' : ''}.</p>
      )}
      {edges.length > 0 && (
        <div className="table-wrap">
          <table className="table table--edges">
            <caption className="visually-hidden">
              {title}: {edges.length} carregados
            </caption>
            <thead>
              <tr>
                <th scope="col">Relação</th>
                <th scope="col">Com</th>
                <th scope="col">Detalhes</th>
                <th scope="col">Período</th>
                <th scope="col">Natureza</th>
                <th scope="col">Origem</th>
              </tr>
            </thead>
            <tbody>
              {edges.map((e) => (
                <EdgeRow key={e.relationship_id} edge={e} entityId={entityId} />
              ))}
            </tbody>
          </table>
        </div>
      )}
      {q.isError && edges.length > 0 && <ErrorNotice error={q.error} onRetry={() => void q.fetchNextPage()} />}
      {q.hasNextPage && (
        <button
          type="button"
          className="button"
          disabled={q.isFetchingNextPage}
          onClick={() => void q.fetchNextPage()}
        >
          {q.isFetchingNextPage ? 'Carregando…' : 'Carregar mais'}
        </button>
      )}
    </section>
  );
}

function EdgeRow({ edge, entityId }: { edge: Edge; entityId: number }) {
  const out = edge.src.id === entityId;
  const closed = isClosed(edge);
  const other = out ? edge.dst : edge.src;
  const relation = relationLabel(edge.type, out ? 'out' : 'in');
  const details = attrLabels(edge.type, edge.attrs);
  const from = formatDate(edge.valid_from);
  return (
    <tr className={edge.kind === 'inference' ? 'edge-row edge-row--inference' : 'edge-row edge-row--fact'}>
      <td>{relation}</td>
      <td>
        <Link to={`/entidade/${encodeURIComponent(other.id)}`}>{other.name}</Link>
        <span className="muted small"> · {kindLabel(other.kind)}</span>
      </td>
      <td>{details.length > 0 ? details.join(' · ') : <span className="muted">—</span>}</td>
      <td>
        {from ? `${closed ? 'início' : 'desde'} ${from}` : <span className="muted">início desconhecido</span>}
        {closed && (
          <>
            <br />
            <span>{closedLabel(edge)}</span>
          </>
        )}
      </td>
      <td>
        <EdgeBadge edge={edge} />
      </td>
      <td>
        <span className="muted small">{edge.source_key}</span>{' '}
        <ProvenancePopover relationshipId={edge.relationship_id} edge={edge} label={`fonte do vínculo com ${other.name}`} />
      </td>
    </tr>
  );
}
