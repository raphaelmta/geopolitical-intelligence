import { useEffect, useRef, type KeyboardEvent } from 'react';
import { createGraph, type GraphHandle } from './graphAdapter';
import { entityIdOf } from './graphStyle';
import type { GraphProps } from './GraphView';

const DARK = '(prefers-color-scheme: dark)';

/** The Cytoscape canvas (lazy chunk: imports the library). */
export default function GraphCanvas({ label, elements, selectedId, onSelect, onActivate, describedBy }: GraphProps & { describedBy: string }) {
  const ref = useRef<HTMLDivElement>(null);
  const handle = useRef<GraphHandle | null>(null);
  const callbacks = useRef({ onSelect, onActivate });
  useEffect(() => {
    callbacks.current = { onSelect, onActivate };
  });

  useEffect(() => {
    const h = createGraph(ref.current!, {
      onSelect: (id) => callbacks.current.onSelect(id),
      onActivate: (id) => callbacks.current.onActivate?.(id),
    });
    handle.current = h;
    const scheme = typeof window.matchMedia === 'function' ? window.matchMedia(DARK) : null;
    const restyle = () => h.restyle();
    scheme?.addEventListener('change', restyle);
    return () => {
      scheme?.removeEventListener('change', restyle);
      h.destroy();
      handle.current = null;
    };
  }, []);

  useEffect(() => {
    handle.current?.setElements(elements);
  }, [elements]);

  useEffect(() => {
    handle.current?.highlight(selectedId);
  }, [selectedId]);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Enter' && selectedId && entityIdOf(selectedId) != null && onActivate) {
      e.preventDefault();
      onActivate(selectedId);
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
      <div className="graph-view__tools">
        <button type="button" className="link-button" onClick={() => handle.current?.fit()}>
          Ajustar à tela
        </button>
      </div>
    </div>
  );
}

