// Stand-in for components/graphAdapter (jsdom has no canvas). Use with
//   vi.mock('../components/graphAdapter', () => import('../test/graphMock'));
// Every graph created is recorded in `graphs`, with its spies and callbacks.
import { vi } from 'vitest';
import type { ElementDefinition } from 'cytoscape';
import type { GraphCallbacks, GraphHandle, GraphSnapshot } from '../components/graphAdapter';

export interface FakeGraph {
  container: HTMLElement;
  callbacks: GraphCallbacks;
  handle: {
    setElements: ReturnType<typeof vi.fn<(els: ElementDefinition[]) => void>>;
    highlight: ReturnType<typeof vi.fn<(id: string | null) => void>>;
    restyle: ReturnType<typeof vi.fn<() => void>>;
    fit: ReturnType<typeof vi.fn<() => void>>;
    destroy: ReturnType<typeof vi.fn<() => void>>;
    snapshot: () => GraphSnapshot;
  };
  /** Element ids of the last setElements call. */
  ids(): string[];
}

export const graphs: FakeGraph[] = [];

export function createGraph(container: HTMLElement, callbacks: GraphCallbacks): GraphHandle {
  const handle = {
    setElements: vi.fn<(els: ElementDefinition[]) => void>(),
    highlight: vi.fn<(id: string | null) => void>(),
    restyle: vi.fn<() => void>(),
    fit: vi.fn<() => void>(),
    setEdgeLabels: vi.fn<(mode: 'auto' | 'all') => void>(),
    zoom: vi.fn<(factor: number) => void>(),
    arrange: vi.fn<() => void>(),
    resize: vi.fn<() => void>(),
    destroy: vi.fn<() => void>(),
    snapshot: () => ({ ids: [], highlighted: [], labelled: [], positions: {} }),
  };
  graphs.push({
    container,
    callbacks,
    handle,
    ids() {
      const last = handle.setElements.mock.calls.at(-1)?.[0] ?? [];
      return last.map((e) => String(e.data.id)).sort();
    },
  });
  return handle;
}

/**
 * The graph mounted most recently, once there is one (the canvas is a lazy
 * chunk: it mounts a moment after the page).
 */
export async function lastGraph(): Promise<FakeGraph> {
  await vi.waitFor(() => {
    if (graphs.length === 0) throw new Error('no graph was created');
  });
  return graphs.at(-1)!;
}

export function resetGraphs() {
  graphs.length = 0;
}
