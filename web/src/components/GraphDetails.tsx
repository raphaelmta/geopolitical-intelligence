import type { ReactNode } from 'react';
import type { Edge, EntityRef } from '../api/types';
import { kindLabel, naturalKeyLabel, relationLabel } from '../api/format';
import { EdgeBadge } from './EdgeBadge';
import { EdgePeriod, EntityLink, edgeDetails } from './EdgeTable';
import { KindIcon } from './KindIcon';
import { ProvenancePopover } from './ProvenancePopover';
import { edgeElementId, entityIdOf, nodeElementId } from './graphStyle';

/** Links of one entity listed in the panel before "ver todos nas listas". */
const PANEL_LINKS = 12;

/**
 * What the panel beside the drawing says about the selected element: for an
 * entity, who it is, what can be done with it and its links on screen; for a
 * line, every relationship between the two entities, each with its nature
 * and its source. With nothing selected, how to use the drawing.
 */
export function GraphDetails({
  edges,
  entities,
  selected,
  onSelect,
  actions,
  idle,
}: {
  /** Every edge on screen. */
  edges: Edge[];
  /** Every entity on screen, by id. */
  entities: ReadonlyMap<number, EntityRef>;
  selected: string | null;
  onSelect: (elementId: string) => void;
  /** Buttons and links for the selected entity (expand, open, map…). */
  actions?: (entity: EntityRef) => ReactNode;
  /** Replaces the default text shown while nothing is selected. */
  idle?: ReactNode;
}) {
  const nodeId = selected != null ? entityIdOf(selected) : null;
  const entity = nodeId != null ? entities.get(nodeId) : undefined;
  if (entity) {
    const own = edges.filter((e) => e.src.id === entity.id || e.dst.id === entity.id);
    const key = naturalKeyLabel(entity.kind, entity.natural_key);
    return (
      <>
        <div className="graph-details__head">
          <KindIcon kind={entity.kind} size="large" />
          <div>
            <p className="eyebrow">{kindLabel(entity.kind)}</p>
            <h3 className="graph-details__title">{entity.name}</h3>
            {key && <p className="muted small mono graph-details__key">{key}</p>}
          </div>
        </div>
        {actions && <div className="graph-details__actions">{actions(entity)}</div>}
        <h4 className="graph-details__sub">
          {own.length} {own.length === 1 ? 'vínculo no gráfico' : 'vínculos no gráfico'}
        </h4>
        <ul className="graph-details__links">
          {own.slice(0, PANEL_LINKS).map((e) => {
            const out = e.src.id === entity.id;
            const other = out ? e.dst : e.src;
            return (
              <li key={e.relationship_id}>
                <span className="graph-details__relation">{relationLabel(e.type, out ? 'out' : 'in')}</span>{' '}
                {other.id === entity.id ? (
                  'a própria entidade'
                ) : (
                  <button type="button" className="link-button" onClick={() => onSelect(nodeElementId(other.id))}>
                    {other.name}
                  </button>
                )}{' '}
                <EdgeBadge edge={e} />
              </li>
            );
          })}
        </ul>
        {own.length > PANEL_LINKS && (
          <p className="muted small">E mais {own.length - PANEL_LINKS}; todos estão nas listas abaixo.</p>
        )}
      </>
    );
  }

  const edge = selected != null && nodeId == null ? edges.find((e) => edgeElementId(e.relationship_id) === selected) : undefined;
  if (edge) {
    // Everything between the two entities, in either direction.
    const between = edges.filter(
      (e) => (e.src.id === edge.src.id && e.dst.id === edge.dst.id) || (e.src.id === edge.dst.id && e.dst.id === edge.src.id),
    );
    return (
      <>
        <p className="eyebrow">
          {between.length} {between.length === 1 ? 'vínculo entre' : 'vínculos entre'}
        </p>
        <h3 className="graph-details__title graph-details__pair">
          <span>
            <KindIcon kind={edge.src.kind} size="small" /> <EntityLink entity={edge.src} />
          </span>
          <span>
            <KindIcon kind={edge.dst.kind} size="small" /> <EntityLink entity={edge.dst} />
          </span>
        </h3>
        <ul className="graph-details__links graph-details__links--cards">
          {between.map((e) => {
            const details = edgeDetails(e);
            return (
              <li key={e.relationship_id} className={selected === edgeElementId(e.relationship_id) ? 'is-selected' : undefined}>
                <p className="graph-details__sentence">
                  {e.src.name} <strong>{relationLabel(e.type, 'out')}</strong> {e.dst.name}
                </p>
                <p>
                  <EdgeBadge edge={e} />
                </p>
                <p className="small">
                  <EdgePeriod edge={e} />
                </p>
                {details && <p className="muted small">{details}</p>}
                <p className="small">
                  <ProvenancePopover
                    relationshipId={e.relationship_id}
                    edge={e}
                    label={`fonte do vínculo nº ${e.relationship_id}, no painel`}
                  />{' '}
                  <span className="muted">· {e.source_key}</span>
                </p>
              </li>
            );
          })}
        </ul>
      </>
    );
  }

  return (
    <div className="graph-details__idle">
      {idle ?? (
        <>
          <h3 className="graph-details__title">Como ler o gráfico</h3>
          <ul>
            <li>Cada círculo é uma pessoa, empresa, partido ou órgão; cada linha, um ou mais vínculos entre os dois.</li>
            <li>Clique num círculo ou numa linha para ver os detalhes aqui.</li>
            <li>Passe o mouse para realçar só o que está ligado àquele ponto.</li>
          </ul>
        </>
      )}
    </div>
  );
}
