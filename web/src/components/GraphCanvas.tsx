import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { createGraph, type GraphHandle } from './graphAdapter';
import { entityIdOf } from './graphStyle';
import type { GraphProps } from './GraphView';

const DARK = '(prefers-color-scheme: dark)';

/** The Cytoscape canvas and its toolbar (lazy chunk: imports the library). */
export default function GraphCanvas({
  label,
  elements,
  selectedId,
  onSelect,
  onActivate,
  onClear,
  layout,
  describedBy,
  full,
  onToggleFull,
}: GraphProps & { describedBy: string; full: boolean; onToggleFull: () => void }) {
  const ref = useRef<HTMLDivElement>(null);
  const handle = useRef<GraphHandle | null>(null);
  const callbacks = useRef({ onSelect, onActivate, onClear });
  const [allLabels, setAllLabels] = useState(false);
  const [tip, setTip] = useState<{ text: string; x: number; y: number } | null>(null);
  const labelsId = useId();
  useEffect(() => {
    callbacks.current = { onSelect, onActivate, onClear };
  });

  useEffect(() => {
    const h = createGraph(
      ref.current!,
      {
        onSelect: (id) => callbacks.current.onSelect(id),
        onActivate: (id) => callbacks.current.onActivate?.(id),
        onClear: () => callbacks.current.onClear?.(),
        onHover: setTip,
      },
      { layout },
    );
    handle.current = h;
    const scheme = typeof window.matchMedia === 'function' ? window.matchMedia(DARK) : null;
    const restyle = () => h.restyle();
    scheme?.addEventListener('change', restyle);
    return () => {
      scheme?.removeEventListener('change', restyle);
      h.destroy();
      handle.current = null;
    };
    // The layout mode of a drawing never changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    handle.current?.setElements(elements);
  }, [elements]);

  useEffect(() => {
    handle.current?.highlight(selectedId);
  }, [selectedId]);

  useEffect(() => {
    handle.current?.setEdgeLabels(allLabels ? 'all' : 'auto');
  }, [allLabels]);

  // Entering or leaving the full-window view changes the canvas size.
  const firstSize = useRef(true);
  useEffect(() => {
    if (firstSize.current) {
      firstSize.current = false;
      return;
    }
    handle.current?.resize();
  }, [full]);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Enter' && selectedId && entityIdOf(selectedId) != null && onActivate) {
      e.preventDefault();
      onActivate(selectedId);
    } else if (e.key === 'Escape' && full) {
      e.preventDefault();
      onToggleFull();
    } else if (e.key === '+' || e.key === '=') {
      handle.current?.zoom(1.25);
    } else if (e.key === '-') {
      handle.current?.zoom(0.8);
    }
  };

  return (
    <div className="graph-view">
      <div
        ref={ref}
        className="graph-view__canvas"
        // Focusable and interactive (mouse gestures, Enter expands the
        // highlighted node), so not "img": "application" passes keys through,
        // and the description sends readers to the list, which has
        // everything the drawing has.
        role="application"
        aria-label={label}
        aria-describedby={describedBy}
        tabIndex={0}
        onKeyDown={onKeyDown}
      />
      {tip && (
        // Decorative: the same words are in the lists and in the panel.
        <div className="graph-tip" aria-hidden="true" style={{ left: tip.x, top: tip.y }}>
          {tip.text}
        </div>
      )}
      <div className="graph-tools" role="group" aria-label="Controles do gráfico">
        <button type="button" className="graph-tools__button" aria-label="Aproximar" title="Aproximar" onClick={() => handle.current?.zoom(1.25)}>
          +
        </button>
        <button type="button" className="graph-tools__button" aria-label="Afastar" title="Afastar" onClick={() => handle.current?.zoom(0.8)}>
          −
        </button>
        <button type="button" className="graph-tools__button graph-tools__button--text" onClick={() => handle.current?.fit()}>
          Ajustar à tela
        </button>
        <button
          type="button"
          className="graph-tools__button graph-tools__button--text"
          title="Devolve cada entidade ao seu lugar, depois de arrastar"
          onClick={() => handle.current?.arrange()}
        >
          Reorganizar
        </button>
        <button type="button" className="graph-tools__button graph-tools__button--text" aria-pressed={full} onClick={onToggleFull}>
          {full ? 'Sair da tela cheia' : 'Tela cheia'}
        </button>
        <span className="graph-tools__check">
          <input id={labelsId} type="checkbox" checked={allLabels} onChange={(e) => setAllLabels(e.target.checked)} />
          <label htmlFor={labelsId} title="Em mapas grandes, o tipo de cada vínculo aparece só ao passar o mouse ou selecionar">
            Nomes em todas as linhas
          </label>
        </span>
      </div>
    </div>
  );
}
