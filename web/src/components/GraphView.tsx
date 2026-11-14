import { Suspense, lazy, useEffect, useId, useState, type ReactNode } from 'react';
import { LEGEND_KINDS, type GraphElement } from './graphStyle';
import type { LayoutMode } from './graphLayout';
import { KindIcon } from './KindIcon';

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
  /** A click on the empty background (drop the selection). */
  onClear?: () => void;
  /** "radial" (default): around the starting entity; "flow": columns by distance from it. */
  layout?: LayoutMode;
}

export interface GraphPanelProps extends GraphProps {
  /** Replaces the default instruction. */
  hint?: string;
  /** Extra class on the panel (graph-panel--embedded: the smaller drawing of the entity page). */
  className?: string;
  /** What is shown beside the drawing about the selected element (GraphDetails). */
  details?: ReactNode;
  /** The drawing has entities marked "+" (links not loaded yet): the legend explains the mark. */
  expandable?: boolean;
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
 * The drawing with its controls, the details of what is selected and the
 * legend. The drawing is a companion of the lists on the same page, never
 * the only way to read them; on a phone it starts hidden behind "Mostrar o
 * gráfico" (and is not mounted at all).
 */
export function GraphPanel({ hint, className, details, expandable, ...props }: GraphPanelProps) {
  const [open, setOpen] = useState(graphStartsOpen);
  const [full, setFull] = useState(false);
  const descId = useId();

  // The full-window view covers the page: the page behind must not scroll.
  useEffect(() => {
    if (!full) return;
    document.documentElement.classList.add('no-scroll');
    return () => document.documentElement.classList.remove('no-scroll');
  }, [full]);

  const classes = ['graph-panel', className, full ? 'graph-panel--full' : ''].filter(Boolean).join(' ');
  return (
    <section className={classes} aria-label="Gráfico">
      <div className="graph-panel__bar">
        <p id={descId} className="muted small graph-panel__hint">
          {hint ??
            (props.onActivate
              ? 'Clique numa entidade ou numa linha para ver os detalhes; duplo clique (ou Enter) numa entidade traz os vínculos dela. Arraste para mover, role para aproximar. As listas abaixo têm tudo o que o gráfico mostra.'
              : 'Clique numa entidade ou numa linha para ver os detalhes. Arraste para mover, role para aproximar. As listas abaixo têm tudo o que o gráfico mostra.')}
        </p>
        <button
          type="button"
          className="button button--small"
          aria-expanded={open}
          onClick={() => {
            setOpen((o) => !o);
            setFull(false);
          }}
        >
          {open ? 'Ocultar o gráfico' : 'Mostrar o gráfico'}
        </button>
      </div>
      {open && (
        <div className={details ? 'graph-panel__body graph-panel__body--details' : 'graph-panel__body'}>
          <Suspense
            fallback={
              <p className="graph-view graph-view__loading" role="status">
                Carregando o gráfico…
              </p>
            }
          >
            <GraphCanvas {...props} describedBy={descId} full={full} onToggleFull={() => setFull((f) => !f)} />
          </Suspense>
          {details && (
            <aside className="graph-details" aria-label="Detalhes do que está selecionado" aria-live="polite">
              {details}
            </aside>
          )}
        </div>
      )}
      <GraphLegend expandable={expandable} />
    </section>
  );
}

/** Icons per entity kind and line styles, as drawn on the canvas. */
export function GraphLegend({ expandable = false }: { expandable?: boolean }) {
  return (
    <div className="graph-legend">
      <ul className="graph-legend__list" aria-label="Tipos de entidade">
        {LEGEND_KINDS.map(({ kind, label }) => (
          <li key={kind}>
            <KindIcon kind={kind} size="small" /> {label}
          </li>
        ))}
      </ul>
      <ul className="graph-legend__list" aria-label="Linhas do gráfico">
        <li>
          <span className="edge-line edge-line--solid legend-line--fact" aria-hidden="true" /> linha contínua: fato
        </li>
        <li>
          <span className="edge-line edge-line--dashed legend-line--inference" aria-hidden="true" /> linha tracejada: inferência
        </li>
        <li>
          <span className="edge-line edge-line--solid legend-line--closed" aria-hidden="true" /> barra no fim (⊣), mais
          clara: encerrado
        </li>
        <li>
          <span className="edge-line edge-line--solid legend-line--multiple" aria-hidden="true" /> linha grossa: mais de
          um vínculo
        </li>
        <li>
          <span className="legend-square" aria-hidden="true" /> quadrado vazado: fonte vencida
        </li>
        <li>
          <span className="legend-circle" aria-hidden="true" /> círculo vazado no início: identidade unida
        </li>
        {expandable && (
          <li>
            <span className="legend-plus" aria-hidden="true">
              +
            </span>{' '}
            vínculos ainda não trazidos (duplo clique)
          </li>
        )}
      </ul>
    </div>
  );
}
