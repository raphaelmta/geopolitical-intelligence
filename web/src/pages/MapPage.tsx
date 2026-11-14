import { useCallback, useEffect, useId, useMemo, useRef, useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { ApiError, apiGet } from '../api/client';
import type { Edge, EntityRef, EntityResponse, RelationshipsPage } from '../api/types';
import { formatDate, kindLabel, relationTypeName } from '../api/format';
import { EdgeLegend } from '../components/EdgeLegend';
import { EdgeTable, EntityLink, useScrollIntoView } from '../components/EdgeTable';
import { ErrorNotice } from '../components/ErrorNotice';
import { GraphDetails } from '../components/GraphDetails';
import { GraphPanel } from '../components/GraphView';
import { RelTypeFilter } from '../components/RelTypeFilter';
import { entityIdOf, nodeElementId, toElements } from '../components/graphStyle';
import { usePageHeading } from '../usePageHeading';

/** Page size of a neighbour request (the API's maximum). */
const PAGE = 200;
/** Most edges drawn at once: past this the browser would struggle. */
export const MAP_EDGE_LIMIT = 2_000;
const ID = /^[1-9]\d{0,17}$/;

type StateFilter = 'open' | 'closed' | 'all';
interface Filters {
  types: string[];
  state: StateFilter;
  /** YYYY-MM-DD or "". */
  at: string;
}
const NO_FILTERS: Filters = { types: [], state: 'open', at: '' };

interface NodeLoad {
  edges: Edge[];
  /** Cursor of the next page; "" when everything was loaded. */
  next: string;
  loading: boolean;
  error: unknown;
  /** Cursor to ask again after an error. */
  failedCursor: string;
}
const EMPTY_LOAD: NodeLoad = { edges: [], next: '', loading: false, error: null, failedCursor: '' };

export function MapPage({ edgeLimit = MAP_EDGE_LIMIT }: { edgeLimit?: number }) {
  const { id = '' } = useParams();
  if (!ID.test(id)) return <NotFound />;
  return <MapExplorer key={id} rootId={Number(id)} edgeLimit={edgeLimit} />;
}

function NotFound() {
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

function relationshipsUrl(nodeId: number, f: Filters, cursor: string): string {
  const p = new URLSearchParams({ state: f.state, limit: String(PAGE) });
  if (f.types.length) p.set('type', f.types.join(','));
  if (f.at) p.set('at', f.at);
  if (cursor) p.set('cursor', cursor);
  return `/entities/${encodeURIComponent(nodeId)}/relationships?${p}`;
}

/**
 * Loads the neighbours of the expanded nodes under the current filters.
 * A filter change starts a new generation: every expanded node is asked
 * again and answers of the old generation are dropped.
 */
function useNeighbourhood(rootId: number, filters: Filters) {
  const [expanded, setExpanded] = useState<number[]>([rootId]);
  const [loads, setLoads] = useState<Record<number, NodeLoad>>({});
  const generation = useRef(0);
  const expandedRef = useRef(expanded);
  expandedRef.current = expanded;
  const filtersRef = useRef(filters);
  filtersRef.current = filters;

  const fetchPage = useCallback(async (nodeId: number, cursor: string) => {
    const gen = generation.current;
    const update = (f: (l: NodeLoad) => NodeLoad) =>
      setLoads((prev) => ({ ...prev, [nodeId]: f(prev[nodeId] ?? EMPTY_LOAD) }));
    update((l) => ({ ...l, loading: true, error: null }));
    try {
      const page = await apiGet<RelationshipsPage>(relationshipsUrl(nodeId, filtersRef.current, cursor));
      if (gen !== generation.current) return;
      update((l) => ({ ...l, edges: [...l.edges, ...page.items], next: page.next_cursor, loading: false }));
    } catch (error) {
      if (gen !== generation.current) return;
      update((l) => ({ ...l, loading: false, error, failedCursor: cursor }));
    }
  }, []);

  useEffect(() => {
    const gen = ++generation.current;
    setLoads({});
    for (const nodeId of expandedRef.current) void fetchPage(nodeId, '');
    return () => {
      // drop answers still on their way (unmount, or the next filter change)
      if (generation.current === gen) generation.current++;
    };
  }, [filters, fetchPage]);

  const expand = useCallback(
    (nodeId: number) => {
      if (expandedRef.current.includes(nodeId)) return;
      setExpanded((e) => [...e, nodeId]);
      void fetchPage(nodeId, '');
    },
    [fetchPage],
  );

  return { expanded, loads, expand, fetchPage };
}

function MapExplorer({ rootId, edgeLimit }: { rootId: number; edgeLimit: number }) {
  const entity = useQuery({
    queryKey: ['entity', String(rootId)],
    queryFn: () => apiGet<EntityResponse>(`/entities/${encodeURIComponent(rootId)}`),
  });
  const [filters, setFilters] = useState<Filters>(NO_FILTERS);
  const [selected, setSelected] = useState<string | null>(null);
  const { expanded, loads, expand, fetchPage } = useNeighbourhood(rootId, filters);
  const root = entity.data?.entity;
  const title = root ? `Mapa: ${root.name}` : `Mapa da entidade nº ${rootId}`;
  const h1 = usePageHeading(title, !entity.isPending);

  // Edges on screen: expanded nodes in the order they were expanded, each
  // relationship once, at most edgeLimit. At the limit (whether or not
  // something was cut) nothing more can be shown or loaded.
  const { edges, shownIds } = useMemo(() => {
    const shownIds = new Set<number>();
    const out: Edge[] = [];
    for (const nodeId of expanded) {
      for (const e of loads[nodeId]?.edges ?? []) {
        if (shownIds.has(e.relationship_id) || out.length >= edgeLimit) continue;
        shownIds.add(e.relationship_id);
        out.push(e);
      }
    }
    return { edges: out, shownIds };
  }, [expanded, loads, edgeLimit]);
  const capped = edges.length >= edgeLimit;
  const capNoteId = useId();

  // Entities drawn whose own links were not asked for yet: marked "+".
  const collapsed = useMemo(() => {
    const open = new Set(expanded);
    const out = new Set<number>();
    for (const e of edges) for (const end of [e.src, e.dst]) if (!open.has(end.id)) out.add(end.id);
    return out;
  }, [edges, expanded]);
  const elements = useMemo(
    () => toElements(edges, root ? [root] : [], rootId, { collapsed: capped ? undefined : collapsed }),
    [edges, root, rootId, collapsed, capped],
  );

  // Entities on screen, the starting one first.
  const nodes = useMemo(() => {
    const m = new Map<number, { entity: EntityRef; onMap: number }>();
    if (root) m.set(root.id, { entity: root, onMap: 0 });
    for (const e of edges) {
      for (const end of e.src.id === e.dst.id ? [e.src] : [e.src, e.dst]) {
        const n = m.get(end.id) ?? { entity: end, onMap: 0 };
        n.onMap++;
        m.set(end.id, n);
      }
    }
    return [...m.values()];
  }, [edges, root]);
  const entities = useMemo(() => new Map(nodes.map((n) => [n.entity.id, n.entity])), [nodes]);

  const activate = (elementId: string) => {
    const nodeId = entityIdOf(elementId);
    if (nodeId != null && !capped) expand(nodeId);
  };

  if (entity.isPending) return <p role="status">Carregando a entidade…</p>;
  if (entity.isError) {
    if (entity.error instanceof ApiError && entity.error.code === 'not_found') return <NotFound />;
    return (
      <>
        <h1 ref={h1} tabIndex={-1}>
          {title}
        </h1>
        <ErrorNotice error={entity.error} onRetry={() => void entity.refetch()} />
      </>
    );
  }

  const rootLoad = loads[rootId];
  const failed = expanded.filter((n) => loads[n]?.error);
  const nameOf = (n: number) => nodes.find((x) => x.entity.id === n)?.entity.name ?? `entidade nº ${n}`;

  return (
    <article className="map">
      <header>
        <p className="eyebrow">Mapa de vínculos · {kindLabel(entity.data.entity.kind)}</p>
        <h1 ref={h1} tabIndex={-1}>
          {title}
        </h1>
        <p className="map__links">
          <Link className="button button--small" to={`/entidade/${encodeURIComponent(rootId)}`}>
            Abrir a página da entidade
          </Link>
          <Link className="button button--small" to={`/caminhos?a=${encodeURIComponent(rootId)}`}>
            Caminhos a partir daqui
          </Link>
        </p>
      </header>

      <details className="fold">
        <summary>
          <span className="fold__title">Filtros</span>{' '}
          <span className="muted small" role="status">
            {describeFilters(filters)}
          </span>
        </summary>
        <FilterForm value={filters} onApply={setFilters} />
      </details>

      {capped && (
        <div className="notice" role="alert" id={capNoteId}>
          <p>O mapa atingiu o limite de exibição; use filtros ou abra a entidade.</p>
          <p className="small">
            Limite: {edgeLimit} vínculos na tela. Expandir e carregar mais ficam desativados até que um filtro reduza o
            mapa.
          </p>
        </div>
      )}
      {failed.map((n) => (
        <div key={n}>
          <p className="small">Vínculos de {nameOf(n)}:</p>
          <ErrorNotice error={loads[n]!.error} onRetry={() => void fetchPage(n, loads[n]!.failedCursor)} />
        </div>
      ))}
      {(!rootLoad || (rootLoad.loading && rootLoad.edges.length === 0)) && <p role="status">Carregando vínculos…</p>}
      {rootLoad && !rootLoad.loading && !rootLoad.error && edges.length === 0 && (
        <p className="empty">Nenhum vínculo com estes filtros.</p>
      )}

      <p className="map__summary">
        <strong>{nodes.length}</strong> {nodes.length === 1 ? 'entidade' : 'entidades'} e <strong>{edges.length}</strong>{' '}
        {edges.length === 1 ? 'vínculo' : 'vínculos'} no mapa
        {collapsed.size > 0 && !capped && (
          <span className="muted">
            {' '}
            · {collapsed.size} {collapsed.size === 1 ? 'entidade ainda pode' : 'entidades ainda podem'} ser expandidas
          </span>
        )}
      </p>

      <GraphPanel
        label={`Mapa de vínculos de ${entity.data.entity.name}: ${nodes.length} entidades e ${edges.length} vínculos`}
        elements={elements}
        selectedId={selected}
        onSelect={setSelected}
        onActivate={activate}
        onClear={() => setSelected(null)}
        expandable={collapsed.size > 0 && !capped}
        details={
          <GraphDetails
            edges={edges}
            entities={entities}
            selected={selected}
            onSelect={setSelected}
            actions={(n) => (
              <>
                {collapsed.has(n.id) && !capped && (
                  <button type="button" className="button button--primary button--small" onClick={() => expand(n.id)}>
                    Trazer vínculos
                  </button>
                )}
                <Link className="button button--small" to={`/entidade/${encodeURIComponent(n.id)}`}>
                  Abrir página
                </Link>
                {n.id !== rootId && (
                  <Link className="button button--small" to={`/mapa/${encodeURIComponent(n.id)}`}>
                    Mapa a partir daqui
                  </Link>
                )}
              </>
            )}
          />
        }
      />

      <EdgeLegend />

      <section className="section" aria-label="Entidades">
        <div className="table-wrap">
          <table className="table">
            <caption>Entidades no mapa ({nodes.length})</caption>
            <thead>
              <tr>
                <th scope="col">Entidade</th>
                <th scope="col">Vínculos</th>
                <th scope="col">Ações</th>
              </tr>
            </thead>
            <tbody>
              {nodes.map(({ entity: n, onMap }) => (
                <NodeRow
                  key={n.id}
                  entity={n}
                  onMap={onMap}
                  load={expanded.includes(n.id) ? (loads[n.id] ?? EMPTY_LOAD) : null}
                  capped={capped}
                  capNoteId={capNoteId}
                  shown={new Set(loads[n.id]?.edges.filter((e) => shownIds.has(e.relationship_id)).map((e) => e.relationship_id)).size}
                  selected={selected === nodeElementId(n.id)}
                  onExpand={() => expand(n.id)}
                  onMore={(cursor) => void fetchPage(n.id, cursor)}
                  onSelect={() => setSelected(nodeElementId(n.id))}
                />
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="section" aria-label="Vínculos">
        <EdgeTable edges={edges} caption="Vínculos no mapa" selectedId={selected} onSelect={setSelected} />
      </section>
    </article>
  );
}

function NodeRow({
  entity,
  onMap,
  load,
  capped,
  capNoteId,
  shown,
  selected,
  onExpand,
  onMore,
  onSelect,
}: {
  entity: EntityRef;
  onMap: number;
  /** null: not expanded. */
  load: NodeLoad | null;
  /** The map is at its display limit. */
  capped: boolean;
  capNoteId: string;
  /** How many of this node's loaded edges are on screen. */
  shown: number;
  selected: boolean;
  onExpand: () => void;
  onMore: (cursor: string) => void;
  onSelect: () => void;
}) {
  const loaded = load ? new Set(load.edges.map((e) => e.relationship_id)).size : 0;
  const ref = useScrollIntoView<HTMLTableRowElement>(selected);
  return (
    <tr ref={ref} className={selected ? 'is-selected' : undefined}>
      <td>
        <EntityLink entity={entity} />
        <span className="muted small"> · {kindLabel(entity.kind)}</span>
      </td>
      <td>
        {load ? (
          <>
            {loaded} {loaded === 1 ? 'vínculo carregado' : 'vínculos carregados'}
            {shown < loaded && `, ${shown} ${shown === 1 ? 'exibido' : 'exibidos'}`}
            {load.loading && <span className="muted small"> · carregando…</span>}
          </>
        ) : (
          <span className="muted">
            {onMap} no mapa · não expandida
          </span>
        )}
      </td>
      <td>
        <div className="row-actions">
          {!load && (
            <button
              type="button"
              className="button button--small"
              aria-label={`Expandir ${entity.name}`}
              disabled={capped}
              aria-describedby={capped ? capNoteId : undefined}
              onClick={onExpand}
            >
              Expandir
            </button>
          )}
          {load?.next && (
            <button
              type="button"
              className="button button--small"
              disabled={load.loading || capped}
              aria-describedby={capped ? capNoteId : undefined}
              onClick={() => onMore(load.next)}
            >
              há mais vínculos — carregar mais 200
            </button>
          )}
          {capped && (!load || load.next) && <span className="muted small">limite do mapa atingido</span>}
          <button
            type="button"
            className="link-button"
            aria-pressed={selected}
            aria-label={`Destacar no gráfico ${entity.name}`}
            onClick={onSelect}
          >
            destacar
          </button>
          <Link to={`/mapa/${encodeURIComponent(entity.id)}`} aria-label={`mapa a partir de ${entity.name}`}>
            mapa
          </Link>
        </div>
      </td>
    </tr>
  );
}

function describeFilters(f: Filters): string {
  const parts = [f.state === 'open' ? 'vínculos atuais' : f.state === 'closed' ? 'vínculos encerrados' : 'todos os vínculos'];
  parts.push(f.types.length ? f.types.map(relationTypeName).join(', ') : 'todos os tipos');
  if (f.at) parts.push(`situação em ${formatDate(f.at)}`);
  return `Mostrando: ${parts.join(' · ')}.`;
}

function FilterForm({ value, onApply }: { value: Filters; onApply: (f: Filters) => void }) {
  const [types, setTypes] = useState(value.types);
  const [state, setState] = useState<StateFilter>(value.state);
  const [at, setAt] = useState(value.at);
  const stateId = useId();
  const atId = useId();
  const atHint = useId();
  const submit = (e: FormEvent) => {
    e.preventDefault();
    onApply({ types, state, at: /^\d{4}-\d{2}-\d{2}$/.test(at) ? at : '' });
  };
  return (
    <form className="filters" onSubmit={submit} aria-label="Filtros do mapa">
      <RelTypeFilter value={types} onChange={setTypes} />
      <div className="filters__row">
        <div className="field">
          <label htmlFor={stateId}>Vínculos</label>
          <select id={stateId} value={state} onChange={(e) => setState(e.target.value as StateFilter)}>
            <option value="open">atuais</option>
            <option value="closed">encerrados</option>
            <option value="all">todos</option>
          </select>
        </div>
        <div className="field">
          <label htmlFor={atId}>Situação em</label>
          <input id={atId} type="date" value={at} aria-describedby={atHint} onChange={(e) => setAt(e.target.value)} />
          <p id={atHint} className="muted small field__hint">
            DD/MM/AAAA; só vínculos válidos nessa data. Vazio: sem data.
          </p>
        </div>
        <button type="submit" className="button button--primary">
          Aplicar filtros
        </button>
      </div>
    </form>
  );
}
