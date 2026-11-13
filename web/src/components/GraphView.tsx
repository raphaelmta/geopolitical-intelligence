import { Suspense, lazy, useId, useState } from 'react';
import { LEGEND_KINDS, type GraphElement } from './graphStyle';

// Cytoscape (about half of the app's code) is loaded only when a graph is
// shown: the list around it renders at once.
const GraphCanvas = lazy(() => import('./GraphCanvas'));

export interface GraphProps {
  /** Accessible name of the drawing. */
  label: string;
  elements: GraphElement[];
  selectedId: string | null;
  onSelect: (elementId: string) => void;
  /** Double-click or Enter on a node (expand it); absent when nothing to do. */
  onActivate?: (elementId: string) => void;
}

export interface GraphPanelProps extends GraphProps {
  /** Replaces the default instruction (which speaks of expanding nodes). */
  hint?: string;
  /** Extra class on the panel (graph-panel--embedded: the smaller drawing of the entity page). */
  className?: string;
}

const PHONE = '(max-width: 40rem)';

function media(query: string): MediaQueryList | null {
  return typeof window.matchMedia === 'function' ? window.matchMedia(query) : null;
}

/** Whether a GraphPanel starts open: not on a phone. */
export function graphStartsOpen(): boolean {
  return !(media(PHONE)?.matches ?? false);
}

/**
 * The drawing plus its controls and legend. The drawing is a companion of
 * the list on the same page, never the only way to read it; on a phone it
 * starts hidden behind "Mostrar o gráfico" (and is not mounted at all).
 */
export function GraphPanel({ hint, className, ...props }: GraphPanelProps) {
  const [open, setOpen] = useState(graphStartsOpen);
  const descId = useId();
  return (
    <section className={className ? `graph-panel ${className}` : 'graph-panel'} aria-label="Gráfico">
      <div className="graph-panel__bar">
        <button type="button" className="button" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
          {open ? 'Ocultar o gráfico' : 'Mostrar o gráfico'}
        </button>
        <p id={descId} className="muted small graph-panel__hint">
          {hint ?? (
            <>
              {props.onActivate
                ? 'Use a lista abaixo para navegar pelos vínculos; no gráfico, Enter expande o nó destacado.'
                : 'Use a lista abaixo para navegar pelos vínculos.'}{' '}
              Clique num elemento do gráfico para marcá-lo na lista
              {props.onActivate ? '; duplo clique num nó o expande' : ''}.
            </>
          )}
        </p>
      </div>
      {open && (
        <Suspense
          fallback={
            <p className="graph-view graph-view__loading" role="status">
              Carregando o gráfico…
            </p>
          }
        >
          <GraphCanvas {...props} describedBy={descId} />
        </Suspense>
      )}
      <GraphLegend />
    </section>
  );
}

/** Shapes per entity kind and line styles, as drawn on the canvas. */
export function GraphLegend() {
  return (
    <div className="graph-legend">
      <ul className="graph-legend__list" aria-label="Tipos de entidade">
        {LEGEND_KINDS.map(({ kind, label }) => (
          <li key={kind}>
            <span className={`legend-shape legend-shape--${kind}`} aria-hidden="true" /> {label}
          </li>
        ))}
      </ul>
      <ul className="graph-legend__list" aria-label="Linhas do gráfico">
        <li>
          <span className="edge-line edge-line--solid" aria-hidden="true" /> contínua: fato
        </li>
        <li>
          <span className="edge-line edge-line--dashed legend-line--inference" aria-hidden="true" /> tracejada: inferência
        </li>
        <li>
          <span className="edge-line edge-line--solid legend-line--closed" aria-hidden="true" /> barra no fim (⊣) em vez
          de seta, e mais clara: encerrado (contínua ou tracejada como acima)
        </li>
        <li>
          <span className="legend-square" aria-hidden="true" /> quadrado vazado no meio: fonte vencida
        </li>
        <li>
          <span className="legend-circle" aria-hidden="true" /> círculo vazado no início: o vínculo depende de uma
          identidade unida — registros de pessoa unidos automaticamente ou por um analista (o texto da linha diz qual)
        </li>
        <li>linha que volta à mesma entidade e linhas paralelas: vínculos distintos</li>
      </ul>
    </div>
  );
}
