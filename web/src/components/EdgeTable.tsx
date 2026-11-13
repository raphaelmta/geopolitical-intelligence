import { useEffect, useRef } from 'react';
import { Link } from 'react-router';
import type { Edge, EntityRef } from '../api/types';
import { attrLabels } from '../api/attrs';
import { closedLabel, formatDate, kindLabel, relationLabel } from '../api/format';
import { EdgeBadge } from './EdgeBadge';
import { ProvenancePopover } from './ProvenancePopover';
import { edgeElementId } from './graphStyle';
import { isClosed } from '../api/edges';

export function EntityLink({ entity }: { entity: EntityRef }) {
  return <Link to={`/entidade/${encodeURIComponent(entity.id)}`}>{entity.name}</Link>;
}

/** Start and (for a closed link) end of an edge, as text. */
export function EdgePeriod({ edge }: { edge: Edge }) {
  const from = formatDate(edge.valid_from);
  const closed = isClosed(edge);
  return (
    <>
      {from ? `${closed ? 'início' : 'desde'} ${from}` : <span className="muted">início desconhecido</span>}
      {closed && (
        <>
          <br />
          <span>{closedLabel(edge)}</span>
        </>
      )}
    </>
  );
}

/**
 * A ref for a list row that scrolls it into view when it becomes the
 * selected one (selected from the graph); focus does not move.
 */
export function useScrollIntoView<T extends HTMLElement>(selected: boolean) {
  const ref = useRef<T>(null);
  useEffect(() => {
    const el = ref.current;
    if (selected && el && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'nearest' });
  }, [selected]);
  return ref;
}

export function edgeDetails(edge: Edge): string {
  return attrLabels(edge.type, edge.attrs).join(' · ');
}

/**
 * The accessible companion of a graph: one row per edge with both ends as
 * links, the relationship, its attributes, period, fact/inference badge and
 * "fonte". "destacar" marks the edge in the graph; the row of the element
 * selected in the graph is marked here.
 */
export function EdgeTable({
  edges,
  caption,
  selectedId,
  onSelect,
}: {
  edges: Edge[];
  caption: string;
  selectedId: string | null;
  onSelect: (elementId: string) => void;
}) {
  return (
    <div className="table-wrap">
      <table className="table table--edges">
        <caption>
          {caption} ({edges.length})
        </caption>
        <thead>
          <tr>
            <th scope="col">De</th>
            <th scope="col">Relação</th>
            <th scope="col">Para</th>
            <th scope="col">Detalhes</th>
            <th scope="col">Período</th>
            <th scope="col">Natureza</th>
            <th scope="col">Origem</th>
            <th scope="col">Gráfico</th>
          </tr>
        </thead>
        <tbody>
          {edges.map((e) => (
            <EdgeRow key={e.relationship_id} edge={e} selected={selectedId === edgeElementId(e.relationship_id)} onSelect={onSelect} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

function EdgeRow({ edge: e, selected, onSelect }: { edge: Edge; selected: boolean; onSelect: (id: string) => void }) {
  const ref = useScrollIntoView<HTMLTableRowElement>(selected);
  const id = edgeElementId(e.relationship_id);
  const details = edgeDetails(e);
  return (
    <tr
      ref={ref}
      className={`edge-row edge-row--${e.kind === 'inference' ? 'inference' : 'fact'}${selected ? ' is-selected' : ''}`}
    >
      <td>
        <EntityLink entity={e.src} />
        <span className="muted small"> · {kindLabel(e.src.kind)}</span>
      </td>
      <td>{relationLabel(e.type, 'out')}</td>
      <td>
        <EntityLink entity={e.dst} />
        <span className="muted small"> · {kindLabel(e.dst.kind)}</span>
      </td>
      <td>{details || <span className="muted">—</span>}</td>
      <td>
        <EdgePeriod edge={e} />
      </td>
      <td>
        <EdgeBadge edge={e} />
      </td>
      <td>
        <span className="muted small">{e.source_key}</span>{' '}
        <ProvenancePopover relationshipId={e.relationship_id} edge={e} label={`fonte do vínculo entre ${e.src.name} e ${e.dst.name}`} />
      </td>
      <td>
        <HighlightButton id={id} relationshipId={e.relationship_id} selected={selected} onSelect={onSelect} />
      </td>
    </tr>
  );
}

/** "destacar": marks one edge in the graph (and in the lists). */
export function HighlightButton({
  id,
  relationshipId,
  selected,
  onSelect,
}: {
  id: string;
  relationshipId: number;
  selected: boolean;
  onSelect: (id: string) => void;
}) {
  return (
    <button
      type="button"
      className="link-button"
      aria-pressed={selected}
      aria-label={`Destacar no gráfico o vínculo nº ${relationshipId}`}
      onClick={() => onSelect(id)}
    >
      destacar
    </button>
  );
}
