import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { Link, useParams } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { ApiError, apiGet } from '../api/client';
import { useJobRunner } from '../api/jobs';
import type { ControlMode, ControlResult, Edge, EntityResponse, Member } from '../api/types';
import { kindLabel, relationLabel } from '../api/format';
import { EdgeBadge } from '../components/EdgeBadge';
import { EdgeLegend } from '../components/EdgeLegend';
import { EdgeTable, EntityLink, HighlightButton, edgeDetails, useScrollIntoView } from '../components/EdgeTable';
import { ProvenancePopover } from '../components/ProvenancePopover';
import { ErrorNotice } from '../components/ErrorNotice';
import { GraphPanel } from '../components/GraphView';
import { JobProgress } from '../components/JobProgress';
import { edgeElementId, toElements } from '../components/graphStyle';
import { usePageHeading } from '../usePageHeading';

const ID = /^[1-9]\d{0,17}$/;
/** Members listed at first (and per "mostrar mais"); the group may be huge. */
const MEMBERS_STEP = 300;

const MODES: { mode: ControlMode; label: string; explain: string }[] = [
  {
    mode: 'documented',
    label: 'Controle documentado',
    explain: 'segue apenas controle declarado, participação acima de 50 % e sócio único.',
  },
  {
    mode: 'corporate_links',
    label: 'Vínculos societários',
    explain: 'segue todo vínculo societário; mostra quem está ligado, não quem manda.',
  },
];

const STOP_REASONS: Record<string, string> = {
  fund: 'fundo de investimento',
  foreign: 'sócio estrangeiro',
  too_many_partners: 'muitos sócios',
  too_large: 'grupo grande demais para exibir por inteiro',
};

export function ControlGroupPage() {
  const { id = '' } = useParams();
  if (!ID.test(id)) return <NotFound />;
  return <ControlGroup key={id} id={Number(id)} />;
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

function ControlGroup({ id }: { id: number }) {
  const entity = useQuery({
    queryKey: ['entity', String(id)],
    queryFn: () => apiGet<EntityResponse>(`/entities/${encodeURIComponent(id)}`),
  });
  const [mode, setMode] = useState<ControlMode>('documented');
  const [selected, setSelected] = useState<string | null>(null);
  const runner = useJobRunner();
  const { run } = runner;
  const sent = useRef('');
  const name = entity.data?.entity.name;
  const isCompany = entity.data?.entity.kind === 'company';
  const title = name ? `Grupo de controle: ${name}` : `Grupo de controle da entidade nº ${id}`;
  const h1 = usePageHeading(title, !entity.isPending);
  const groupName = useId();

  useEffect(() => {
    // once per mode (also under StrictMode's double effects)
    if (!isCompany || sent.current === mode) return;
    sent.current = mode;
    setSelected(null);
    run({ type: 'control_group', params: { company_id: id, mode } });
  }, [isCompany, mode, id, run]);

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

  const job = runner.state.job;
  const result = job?.status === 'done' ? (job.result as ControlResult | null) : null;
  const corporate = mode === 'corporate_links';

  return (
    <article>
      <header>
        <p className="eyebrow">Grupo de controle · {kindLabel(entity.data.entity.kind)}</p>
        <h1 ref={h1} tabIndex={-1}>
          {title}
        </h1>
        <p>
          <Link to={`/entidade/${encodeURIComponent(id)}`}>Abrir a página da empresa</Link> ·{' '}
          <Link to={`/mapa/${encodeURIComponent(id)}`}>Ver no mapa</Link>
        </p>
      </header>

      {!isCompany ? (
        <p className="notice">O grupo de controle só é calculado para empresas.</p>
      ) : (
        <>
          <fieldset className="modes">
            <legend>O que seguir</legend>
            {MODES.map((m) => (
              <div key={m.mode} className="modes__item">
                <input
                  type="radio"
                  id={`${groupName}-${m.mode}`}
                  name={groupName}
                  value={m.mode}
                  checked={mode === m.mode}
                  aria-describedby={`${groupName}-${m.mode}-explain`}
                  onChange={() => setMode(m.mode)}
                />{' '}
                <label htmlFor={`${groupName}-${m.mode}`} className="modes__label">
                  {m.label}
                </label>
                <p id={`${groupName}-${m.mode}-explain`} className="muted small modes__explain">
                  {m.explain}
                </p>
              </div>
            ))}
          </fieldset>
          {corporate && (
            <div className="notice notice--caveat">
              <p>Vínculo societário não é prova de controle.</p>
            </div>
          )}

          {runner.sendError != null && <ErrorNotice error={runner.sendError} onRetry={runner.retry} />}
          {runner.request && runner.sendError == null && <JobProgress state={runner.state} onRetry={runner.retry} onResume={runner.resume} />}

          {result && <GroupResult result={result} selected={selected} onSelect={setSelected} />}
        </>
      )}
    </article>
  );
}

function GroupResult({
  result,
  selected,
  onSelect,
}: {
  result: ControlResult;
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  const controllers = result.controllers ?? [];
  const members = result.members ?? [];
  const edges = result.edges ?? [];
  const stops = result.stops ?? [];
  const tooLarge = stops.some((s) => s.reason === 'too_large');
  const byId = useMemo(() => new Map(edges.map((e) => [e.relationship_id, e])), [edges]);
  const elements = useMemo(
    () => toElements(edges, [result.root, ...controllers], result.root.id),
    [edges, result.root, controllers],
  );
  const [shown, setShown] = useState(MEMBERS_STEP);
  const levels = useMemo(() => {
    const sorted = [...members].sort((x, y) => x.depth - y.depth);
    const visible = sorted.slice(0, shown);
    const m = new Map<number, Member[]>();
    for (const x of visible) m.set(x.depth, [...(m.get(x.depth) ?? []), x]);
    return [...m.entries()];
  }, [members, shown]);
  const hc = useId();
  const hm = useId();
  const hs = useId();
  const he = useId();

  return (
    <>
      <EdgeLegend />
      <section className="section" aria-labelledby={hc}>
        <h2 id={hc}>Controladores finais</h2>
        {controllers.length === 0 ? (
          <p className="muted">
            Nenhum controlador final identificado
            {tooLarge ? ': a busca parou antes de chegar ao topo do grupo.' : '.'}
          </p>
        ) : (
          <ul className="plain-list">
            {controllers.map((c) => (
              <li key={c.id}>
                <EntityLink entity={c} /> <span className="muted small">· {kindLabel(c.kind)}</span>
              </li>
            ))}
          </ul>
        )}
      </section>

      <GraphPanel
        label={`Gráfico do grupo: ${members.length} empresas e ${edges.length} vínculos`}
        elements={elements}
        selectedId={selected}
        onSelect={onSelect}
      />

      <section className="section" aria-labelledby={hm}>
        <h2 id={hm}>Empresas do grupo</h2>
        <p className="muted small section-note">
          Nível: quantos elos separam a empresa do controlador final mais próximo. Abaixo de cada uma, a cadeia que a
          liga a ele.
        </p>
        {levels.map(([depth, list]) => (
          <div key={depth} className="level">
            <h3>
              Nível {depth} <span className="muted small">({list.length})</span>
            </h3>
            <ul className="plain-list">
              {list.map((m) => (
                <li key={m.entity.id}>
                  <EntityLink entity={m.entity} />
                  {m.entity.id === result.root.id && <span className="tag tag--current">esta empresa</span>}
                  <Chain via={m.via ?? []} byId={byId} selected={selected} onSelect={onSelect} />
                </li>
              ))}
            </ul>
          </div>
        ))}
        {members.length > shown && (
          <button type="button" className="button" onClick={() => setShown((s) => s + MEMBERS_STEP)}>
            Mostrar mais {Math.min(MEMBERS_STEP, members.length - shown)} de {members.length - shown}
          </button>
        )}
      </section>

      <section className="section" aria-labelledby={hs}>
        <h2 id={hs}>Onde a busca parou</h2>
        {stops.length === 0 ? (
          <p className="muted">A busca não parou em nenhuma entidade.</p>
        ) : (
          <ul className="plain-list">
            {stops.map((s) => (
              <li key={`${s.entity.id}-${s.reason}`}>
                <EntityLink entity={s.entity} /> — {STOP_REASONS[s.reason] ?? `outro motivo (${s.reason})`}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="section" aria-labelledby={he}>
        <h2 id={he} className="visually-hidden">
          Vínculos
        </h2>
        <EdgeTable edges={edges} caption="Vínculos do grupo" selectedId={selected} onSelect={onSelect} />
      </section>
    </>
  );
}

/** The links from the final controller down to a member, each with its nature. */
function Chain({
  via,
  byId,
  selected,
  onSelect,
}: {
  via: number[];
  byId: Map<number, Edge>;
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  if (via.length === 0) return null;
  return (
    <ol className="chain small">
      {via.map((rid) => {
        const e = byId.get(rid);
        if (!e) return <li key={rid}>vínculo nº {rid} (fora do resultado)</li>;
        return <ChainHop key={rid} edge={e} selected={selected === edgeElementId(rid)} onSelect={onSelect} />;
      })}
    </ol>
  );
}

/** One link of a member's chain: its nature, its source, and "destacar". */
function ChainHop({ edge: e, selected, onSelect }: { edge: Edge; selected: boolean; onSelect: (id: string) => void }) {
  const ref = useScrollIntoView<HTMLLIElement>(selected);
  const details = edgeDetails(e);
  return (
    <li ref={ref} className={selected ? 'is-selected' : undefined}>
      {e.src.name} {relationLabel(e.type, 'out')} {e.dst.name}
      {details && <span className="muted"> · {details}</span>} <EdgeBadge edge={e} />{' '}
      <ProvenancePopover relationshipId={e.relationship_id} edge={e} label={`fonte do vínculo entre ${e.src.name} e ${e.dst.name}`} />{' '}
      <HighlightButton id={edgeElementId(e.relationship_id)} relationshipId={e.relationship_id} selected={selected} onSelect={onSelect} />
    </li>
  );
}
