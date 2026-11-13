// The only module that runs Cytoscape against the DOM (tests replace it:
// jsdom has no canvas). It keeps the drawing in step with a list of
// elements, adding and removing by id so an expansion never redraws the
// whole map from scratch.
import cytoscape from 'cytoscape';
import { graphStylesheet, readPalette, type GraphElement } from './graphStyle';

export interface GraphCallbacks {
  /** A node or edge was clicked/tapped. */
  onSelect(elementId: string): void;
  /** A node was double-clicked (expand it). */
  onActivate(elementId: string): void;
}

export interface GraphSnapshot {
  ids: string[];
  highlighted: string[];
  positions: Record<string, { x: number; y: number }>;
}

export interface GraphHandle {
  setElements(elements: GraphElement[]): void;
  /** Marks one element (and brings it into view), or clears the mark. */
  highlight(elementId: string | null): void;
  /** Re-reads the theme colors (light/dark change). */
  restyle(): void;
  fit(): void;
  destroy(): void;
  /** Element ids, highlighted ids and node positions (for tests and debugging). */
  snapshot(): GraphSnapshot;
}

/**
 * Cytoscape's renderer prepends an inline <style> to <head> unless an
 * element with this id already exists. The CSP (style-src 'self') would
 * block that tag, so a placeholder is put there first; the one rule it would
 * have set (position: relative on the container) is in styles.css.
 */
export const CY_STYLESHEET_ID = '__________cytoscape_stylesheet';

function placeStylesheetPlaceholder() {
  if (document.getElementById(CY_STYLESHEET_ID)) return;
  const marker = document.createElement('meta');
  marker.id = CY_STYLESHEET_ID;
  document.head.appendChild(marker);
}

/** Above this many nodes the force layout (cose) is too slow for the main thread. */
const COSE_MAX_NODES = 300;

/** headless: no renderer (tests; jsdom has no canvas). */
export function createGraph(
  container: HTMLElement,
  callbacks: GraphCallbacks,
  { headless = false }: { headless?: boolean } = {},
): GraphHandle {
  placeStylesheetPlaceholder();
  const cy = cytoscape({
    ...(headless ? { headless: true, styleEnabled: true } : { container }),
    style: graphStylesheet(readPalette()),
    elements: [],
    minZoom: 0.05,
    maxZoom: 4,
    boxSelectionEnabled: false,
    selectionType: 'single',
  });
  cy.on('tap', 'node, edge', (e) => callbacks.onSelect(e.target.id()));
  cy.on('dbltap', 'node', (e) => callbacks.onActivate(e.target.id()));

  let layout: cytoscape.Layouts | null = null;
  let highlighted: string | null = null;

  const cose = (n: number, randomize: boolean, fit: boolean): cytoscape.LayoutOptions => ({
    name: 'cose',
    animate: false,
    randomize,
    fit,
    padding: 24,
    numIter: n <= 100 ? 1000 : 300,
    coolingFactor: n <= 100 ? 0.99 : 0.95,
  });

  /** First drawing: lay out everything and fit it to the screen. */
  const layoutAll = () => {
    layout?.stop();
    const n = cy.nodes().length;
    layout = cy.layout(
      n <= COSE_MAX_NODES
        ? cose(n, true, true)
        : // Large maps: a cheap layout (one pass), most connected in the middle.
          {
            name: 'concentric',
            animate: false,
            fit: true,
            padding: 24,
            minNodeSpacing: 8,
            concentric: (node: cytoscape.NodeSingular) => node.degree(false),
            levelWidth: () => 4,
          },
    );
    layout.run();
  };

  /**
   * An expansion: nodes already on screen keep their positions (locked while
   * the force layout settles the new ones) and the viewport does not move.
   * Large maps skip the force layout: new nodes stay on their ring.
   */
  const layoutNew = (fresh: cytoscape.NodeCollection) => {
    const n = cy.nodes().length;
    if (n > COSE_MAX_NODES) return;
    layout?.stop();
    const old = cy.nodes().difference(fresh).nodes().filter((node) => !node.locked());
    old.lock();
    layout = cy.layout(cose(n, false, false));
    layout.one('layoutstop', () => old.unlock());
    layout.run();
  };

  /** Ring positions for new nodes around the node already on screen they hang from. */
  const ringPositions = (fresh: GraphElement[]) => {
    const byAnchor = new Map<string, string[]>();
    const anchored = new Set<string>();
    for (const e of fresh) {
      if (e.group !== 'edges') continue;
      for (const [a, b] of [
        [e.data.source!, e.data.target!],
        [e.data.target!, e.data.source!],
      ] as const) {
        if (anchored.has(b) || cy.getElementById(b).nonempty() || cy.getElementById(a).empty()) continue;
        anchored.add(b);
        byAnchor.set(a, [...(byAnchor.get(a) ?? []), b]);
      }
    }
    const out = new Map<string, cytoscape.Position>();
    for (const [a, ids] of byAnchor) {
      const p = cy.getElementById(a).position();
      const r = 90 + ids.length * 4;
      ids.forEach((id, i) => {
        const t = (2 * Math.PI * i) / ids.length;
        out.set(id, { x: p.x + r * Math.cos(t), y: p.y + r * Math.sin(t) });
      });
    }
    return out;
  };

  const mark = (id: string | null) => {
    cy.elements('.highlighted').removeClass('highlighted');
    highlighted = id;
    if (!id) return;
    const el = cy.getElementById(id);
    if (el.empty()) return;
    el.addClass('highlighted');
    const bb = el.boundingBox();
    const ext = cy.extent();
    if (bb.x1 < ext.x1 || bb.x2 > ext.x2 || bb.y1 < ext.y1 || bb.y2 > ext.y2) cy.center(el);
  };

  return {
    setElements(elements) {
      const want = new Map(elements.map((e) => [e.data.id, e]));
      let added = cy.collection();
      cy.batch(() => {
        cy.elements()
          .filter((el) => !want.has(el.id()))
          .remove();
        const fresh: GraphElement[] = [];
        for (const e of elements) {
          const el = cy.getElementById(e.data.id);
          if (el.empty()) fresh.push(e);
          else el.classes(e.classes);
        }
        if (fresh.length === 0) return;
        const placed = ringPositions(fresh);
        added = cy.add(
          fresh.map((e) => ({
            group: e.group,
            data: e.data,
            classes: e.classes,
            ...(e.group === 'nodes' && placed.has(e.data.id) ? { position: placed.get(e.data.id)! } : {}),
          })),
        );
      });
      const freshNodes = added.nodes();
      // A first drawing (or one where at most the starting node was kept,
      // as after a filter change) is laid out whole and fitted.
      if (freshNodes.nonempty()) {
        if (cy.nodes().length - freshNodes.length <= 1) layoutAll();
        else layoutNew(freshNodes);
      }
      mark(highlighted);
    },
    highlight: mark,
    restyle() {
      cy.style().fromJson(graphStylesheet(readPalette())).update();
    },
    fit() {
      cy.fit(undefined, 24);
    },
    destroy() {
      layout?.stop();
      cy.destroy();
    },
    snapshot() {
      const positions: GraphSnapshot['positions'] = {};
      cy.nodes().forEach((node) => {
        positions[node.id()] = { ...node.position() };
      });
      return {
        ids: cy.elements().map((el) => el.id()).sort(),
        highlighted: cy.elements('.highlighted').map((el) => el.id()),
        positions,
      };
    },
  };
}
