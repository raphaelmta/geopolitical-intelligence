import { useEffect, useId, useMemo, useRef, useState, type FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { apiGet } from '../api/client';
import { useJobRunner } from '../api/jobs';
import type { Edge, EntityRef, EntityResponse, Hop, PathsParams, PathsResult } from '../api/types';
import { relationLabel } from '../api/format';
import { EdgeBadge } from '../components/EdgeBadge';
import { EdgeLegend } from '../components/EdgeLegend';
import { EdgePeriod, EntityLink, HighlightButton, edgeDetails, useScrollIntoView } from '../components/EdgeTable';
import { EntityPicker } from '../components/EntityPicker';
import { ErrorNotice } from '../components/ErrorNotice';
import { GraphDetails } from '../components/GraphDetails';
import { GraphPanel } from '../components/GraphView';
import { JobProgress } from '../components/JobProgress';
import { ProvenancePopover } from '../components/ProvenancePopover';
import { RelTypeFilter } from '../components/RelTypeFilter';
import { edgeElementId, entitiesOf, toElements } from '../components/graphStyle';
import { identityMerged, pathHasDoubt } from '../api/edges';
import { usePageHeading } from '../usePageHeading';

const MAX_DEPTH = 6;
const ID = /^[1-9]\d{0,17}$/;

function idParam(v: string | null): string | null {
  return v && ID.test(v) ? v : null;
}

/**
 * Fills a picker once from ?a= or ?b= (the entity's name comes from the
 * API), unless the user already picked or cleared that field meanwhile.
 */
function usePrefill(id: string | null, set: (e: EntityRef) => void, touched: { current: boolean }) {
  const done = useRef(false);
  const q = useQuery({
    queryKey: ['entity', id],
    queryFn: () => apiGet<EntityResponse>(`/entities/${encodeURIComponent(id!)}`),
    enabled: id != null,
  });
  useEffect(() => {
    if (!done.current && q.data) {
      done.current = true;
      if (!touched.current) set(q.data.entity);
    }
  }, [q.data, set, touched]);
  return q;
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

export function PathsPage() {
  const [params] = useSearchParams();
  const h1 = usePageHeading('Caminhos entre duas entidades');
  const [a, setA] = useState<EntityRef | null>(null);
  const [b, setB] = useState<EntityRef | null>(null);
  const [depth, setDepth] = useState(MAX_DEPTH);
  const [types, setTypes] = useState<string[]>([]);
  const [at, setAt] = useState('');
  const [problem, setProblem] = useState('');
  const [selected, setSelected] = useState<string | null>(null);
  const runner = useJobRunner();
  const touchedA = useRef(false);
  const touchedB = useRef(false);
  const prefillA = usePrefill(idParam(params.get('a')), setA, touchedA);
  const prefillB = usePrefill(idParam(params.get('b')), setB, touchedB);
  const pickA = (e: EntityRef | null) => {
    touchedA.current = true;
    setA(e);
  };
  const pickB = (e: EntityRef | null) => {
    touchedB.current = true;
    setB(e);
  };
  const depthId = useId();
  const atId = useId();

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!a || !b) return setProblem('Escolha as duas entidades.');
    if (a.id === b.id) return setProblem('Escolha duas entidades diferentes.');
    setProblem('');
    setSelected(null);
    const p: PathsParams = { a: a.id, b: b.id, max_depth: depth };
    if (types.length) p.rel_types = types;
    if (/^\d{4}-\d{2}-\d{2}$/.test(at)) p.at = at;
    runner.run({ type: 'paths', params: p });
  };

  const job = runner.state.job;
  const result = job?.status === 'done' ? (job.result as PathsResult | null) : null;
  const sentDepth = runner.request?.type === 'paths' ? runner.request.params.max_depth || MAX_DEPTH : MAX_DEPTH;

  return (
    <>
      <h1 ref={h1} tabIndex={-1}>
        Caminhos entre duas entidades
      </h1>
      <p className="lede">
        Procura as ligações mais curtas entre duas entidades, elo por elo. Cada elo mostra se é fato ou inferência e
        de onde veio.
      </p>
      <form className="filters" onSubmit={submit} aria-label="Busca de caminhos" noValidate>
        <div className="filters__row">
          <EntityPicker label="De" value={a} onChange={pickA} />
          <EntityPicker label="Até" value={b} onChange={pickB} />
        </div>
        {(prefillA.isError || prefillB.isError) && (
          <ErrorNotice error={prefillA.error ?? prefillB.error} />
        )}
        <div className="filters__row">
          <div className="field">
            <label htmlFor={depthId}>Profundidade máxima</label>
            <select id={depthId} value={depth} onChange={(e) => setDepth(Number(e.target.value))}>
              {[1, 2, 3, 4, 5, 6].map((d) => (
                <option key={d} value={d}>
                  {d}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor={atId}>Situação em</label>
            <input id={atId} type="date" value={at} onChange={(e) => setAt(e.target.value)} />
          </div>
        </div>
        <RelTypeFilter value={types} onChange={setTypes} />
        <button type="submit" className="button button--primary" disabled={runner.sending}>
          Buscar caminhos
        </button>
        {problem && (
          <p className="hint hint--warning" role="alert">
            {problem}
          </p>
        )}
      </form>

      {runner.sendError != null && <ErrorNotice error={runner.sendError} onRetry={runner.retry} />}
      {runner.request && runner.sendError == null && <JobProgress state={runner.state} onRetry={runner.retry} onResume={runner.resume} />}

      {result && (
        <PathsResultView
          result={result}
          depth={sentDepth}
          selected={selected}
          onSelect={setSelected}
        />
      )}
    </>
  );
}

function PathsResultView({
  result,
  depth,
  selected,
  onSelect,
}: {
  result: PathsResult;
  depth: number;
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  const paths = result.paths ?? [];
  const hubs = result.stopped_hubs ?? [];
  const union = useMemo(() => paths.flatMap((p) => p.map((h) => h.edge)), [paths]);
  // The drawing reads left to right from the entity the paths start at.
  const start = paths[0]?.[0] ? ends(paths[0][0])[0] : undefined;
  const elements = useMemo(() => toElements(union, [], start?.id), [union, start?.id]);
  const entities = useMemo(() => entitiesOf(union), [union]);
  const h = useId();
  return (
    <section className="section" aria-labelledby={h}>
      <h2 id={h}>Resultado</h2>
      <EdgeLegend />
      {paths.length === 0 ? (
        <p className="empty">Nenhum caminho encontrado até a profundidade {depth}.</p>
      ) : (
        <p>
          {plural(paths.length, 'caminho', 'caminhos')} de {plural(paths[0]!.length, 'elo', 'elos')} (os mais curtos).
          Um caminho vale o que vale o seu elo mais fraco.
        </p>
      )}
      {result.truncated && (
        <div className="notice">
          <p>Resultado truncado: há mais caminhos do mesmo tamanho.</p>
        </div>
      )}
      {hubs.length > 0 && (
        <div className="notice">
          <p>
            Entidades muito conectadas não são atravessadas, para que vínculos triviais (mesmo partido, mesmo cartório)
            não inundem o resultado. A busca não passou por:
          </p>
          <ul>
            {hubs.map((s) => (
              <li key={s.entity.id}>
                <span>{`parou em ${s.entity.name} (${plural(s.degree, 'vínculo', 'vínculos')})`}</span> ·{' '}
                <EntityLink entity={{ ...s.entity, name: `página de ${s.entity.name}` }} />
              </li>
            ))}
          </ul>
        </div>
      )}
      {paths.length > 0 && (
        <>
          <GraphPanel
            label="Gráfico dos caminhos encontrados"
            elements={elements}
            selectedId={selected}
            onSelect={onSelect}
            layout="flow"
            details={
              <GraphDetails
                edges={union}
                entities={entities}
                selected={selected}
                onSelect={onSelect}
                actions={(n) => (
                <Link className="button button--small" to={`/entidade/${encodeURIComponent(n.id)}`}>
                  Abrir página
                </Link>
              )}
              />
            }
          />
          {paths.map((p, i) => (
            <PathView key={i} hops={p} index={i} selected={selected} onSelect={onSelect} />
          ))}
        </>
      )}
    </section>
  );
}

/** Entity a hop leaves from and arrives at, in the walking direction. */
function ends(h: Hop): [EntityRef, EntityRef] {
  return h.forward ? [h.edge.src, h.edge.dst] : [h.edge.dst, h.edge.src];
}

function PathView({
  hops,
  index,
  selected,
  onSelect,
}: {
  hops: Hop[];
  index: number;
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  const h = useId();
  if (hops.length === 0) return null;
  const chain = [ends(hops[0]!)[0], ...hops.map((x) => ends(x)[1])];
  const inferences = hops.filter((x) => x.edge.kind === 'inference').length;
  const facts = hops.length - inferences;
  const merged = hops.filter((x) => identityMerged(x.edge)).length;
  const weak = pathHasDoubt(hops.map((x) => x.edge));
  return (
    <section className="path" aria-labelledby={h}>
      <h3 id={h}>
        Caminho {index + 1}: {chain.map((e) => e.name).join(' → ')}
      </h3>
      <p className="path__summary">
        {plural(hops.length, 'elo', 'elos')} — {plural(facts, 'fato', 'fatos')},{' '}
        {plural(inferences, 'inferência', 'inferências')}
        {merged > 0 && `, ${merged} com identidade unida`}
        {weak && (
          <>
            {' '}
            <span className="tag tag--inference">contém inferência ou identidade unida</span>
          </>
        )}
      </p>
      <ol className="hops">
        {hops.map((x, i) => (
          <HopItem key={`${i}-${x.edge.relationship_id}`} hop={x} selected={selected} onSelect={onSelect} />
        ))}
      </ol>
    </section>
  );
}

function HopItem({ hop, selected, onSelect }: { hop: Hop; selected: string | null; onSelect: (id: string) => void }) {
  const e: Edge = hop.edge;
  const [from, to] = ends(hop);
  const id = edgeElementId(e.relationship_id);
  const details = edgeDetails(e);
  const ref = useScrollIntoView<HTMLLIElement>(selected === id);
  return (
    <li ref={ref} className={`hop edge-row--${e.kind === 'inference' ? 'inference' : 'fact'}${selected === id ? ' is-selected' : ''}`}>
      <p className="hop__line">
        <EntityLink entity={from} /> <span className="hop__rel">{relationLabel(e.type, hop.forward ? 'out' : 'in')}</span>{' '}
        <EntityLink entity={to} />
      </p>
      <p className="hop__meta small">
        {details && <span>{details} · </span>}
        <span>
          <EdgePeriod edge={e} />
        </span>
      </p>
      <p className="hop__meta">
        <EdgeBadge edge={e} /> <span className="muted small">{e.source_key}</span>{' '}
        <ProvenancePopover relationshipId={e.relationship_id} edge={e} label={`fonte do vínculo entre ${from.name} e ${to.name}`} />{' '}
        <HighlightButton id={id} relationshipId={e.relationship_id} selected={selected === id} onSelect={onSelect} />
      </p>
    </li>
  );
}
