// The only module that runs Cytoscape against the DOM (tests replace it:
// jsdom has no canvas). It keeps the drawing in step with a list of
// elements, adding and removing by id, and moves the nodes to the positions
// of graphLayout (animated, so an expansion is seen growing out of the node
// that was expanded).
import cytoscape from 'cytoscape';
import { graphStylesheet, readPalette, type GraphElement } from './graphStyle';
import { computeLayout, type LayoutMode } from './graphLayout';

export interface GraphCallbacks {
  /** A node or line was clicked/tapped (a line answers with its first relationship). */
  onSelect(elementId: string): void;
  /** A node was double-clicked (expand it). */
  onActivate(elementId: string): void;
  /** The empty background was clicked: nothing is selected any more. */
  onClear?(): void;
  /** The pointer is over an element (its tip, at a point of the canvas), or left it (null). */
  onHover?(tip: { text: string; x: number; y: number } | null): void;
}

export interface GraphSnapshot {
  ids: string[];
  highlighted: string[];
  labelled: string[];
  positions: Record<string, { x: number; y: number }>;
}

/** Whether the words on the lines are drawn: "auto" = only on small drawings. */
export type EdgeLabels = 'auto' | 'all';

export interface GraphHandle {
  setElements(elements: GraphElement[]): void;
  /** Marks one element (and brings it into view), or clears the mark. */
  highlight(elementId: string | null): void;
  setEdgeLabels(mode: EdgeLabels): void;
  /** Re-reads the theme colors (light/dark change). */
  restyle(): void;
  fit(): void;
  zoom(factor: number): void;
  /** Puts every node back in its computed place (after dragging). */
  arrange(): void;
  /** The container changed size. */
  resize(): void;
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

/** With more lines than this, "auto" keeps the words for the pointer and the selection. */
export const AUTO_LABEL_EDGES = 28;
const PADDING = 56;
/** Below this zoom the names can no longer be read. */
const READABLE_ZOOM = 0.5;
/** A drawing of two or three nodes is not blown up to fill the canvas. */
const MAX_FIT_ZOOM = 1.2;

export interface GraphOptions {
  /** No renderer (tests; jsdom has no canvas). */
  headless?: boolean;
  layout?: LayoutMode;
}

export function createGraph(
  container: HTMLElement,
  callbacks: GraphCallbacks,
  { headless = false, layout: mode = 'radial' }: GraphOptions = {},
): GraphHandle {
  placeStylesheetPlaceholder();
  const cy = cytoscape({
    ...(headless ? { headless: true, styleEnabled: true } : { container }),
    style: graphStylesheet(readPalette()),
    elements: [],
    minZoom: 0.08,
    maxZoom: 3,
    boxSelectionEnabled: false,
    selectionType: 'single',
  });

  let highlighted: string | null = null;
  let hovered: string | null = null;
  let labels: EdgeLabels = 'auto';
  let first = true;
  /** Relationship element id → id of the line that stands for it. */
  const lineOf = new Map<string, string>();

  const resolve = (id: string | null) => (id == null ? null : (lineOf.get(id) ?? id));

  /** Fades what is not next to the hovered (else the selected) element and decides the words. */
  const emphasise = () => {
    const focus = resolve(hovered) ?? resolve(highlighted);
    const el = focus ? cy.getElementById(focus) : cy.collection();
    cy.batch(() => {
      cy.elements().removeClass('faded near');
      if (el.nonempty()) {
        // a node with what touches it, or a line with its two ends
        const near = around(el);
        cy.elements().difference(near).addClass('faded');
        near.edges().addClass('near');
      }
      const all = labels === 'all' || cy.edges().length <= AUTO_LABEL_EDGES;
      cy.edges().forEach((e) => {
        e.toggleClass('labelled', all || e.hasClass('near'));
      });
    });
  };

  /** The selection being made comes from a click on the drawing: the view stays where it is. */
  let tapping = false;
  cy.on('tap', 'node, edge', (e) => {
    tapping = true;
    callbacks.onSelect(e.target.id());
    // the highlight arrives in an effect of the page; let it see the flag
    setTimeout(() => {
      tapping = false;
    }, 100);
  });
  cy.on('tap', (e) => {
    if (e.target === cy) callbacks.onClear?.();
  });
  cy.on('dbltap', 'node', (e) => callbacks.onActivate(e.target.id()));
  cy.on('mouseover', 'node, edge', (e) => {
    hovered = e.target.id();
    emphasise();
    const tip = e.target.data('tip') as string | undefined;
    if (tip) callbacks.onHover?.({ text: tip, x: e.renderedPosition.x, y: e.renderedPosition.y });
  });
  cy.on('mouseout', 'node, edge', () => {
    hovered = null;
    emphasise();
    callbacks.onHover?.(null);
  });
  cy.on('viewport', () => callbacks.onHover?.(null));

  let running: cytoscape.Layouts | null = null;
  /**
   * After an expansion: everything, when everything still reads at that
   * size; otherwise what just arrived and what it hangs from.
   */
  const fitZoom = (eles: cytoscape.CollectionReturnValue) => {
    const bb = eles.boundingBox();
    return Math.min((cy.width() - 2 * PADDING) / Math.max(bb.w, 1), (cy.height() - 2 * PADDING) / Math.max(bb.h, 1));
  };

  /** Brings a set of elements into view, never blown up past MAX_FIT_ZOOM. */
  const show = (eles: cytoscape.CollectionReturnValue, duration: number) => {
    if (eles.empty()) return;
    const zoom = Math.max(cy.minZoom(), Math.min(fitZoom(eles), MAX_FIT_ZOOM));
    const bb = eles.boundingBox();
    const pan = { x: cy.width() / 2 - zoom * (bb.x1 + bb.w / 2), y: cy.height() / 2 - zoom * (bb.y1 + bb.h / 2) };
    cy.stop();
    if (headless || duration === 0) cy.viewport({ zoom, pan });
    else cy.animate({ zoom, pan, duration, easing: 'ease-out' });
  };

  const around = (el: cytoscape.CollectionReturnValue) =>
    el.nodes().closedNeighborhood().union(el.edges()).union(el.edges().connectedNodes());

  /**
   * After an expansion: everything, when everything still reads at that
   * size; otherwise what just arrived and what it hangs from.
   */
  const frame = (fresh: cytoscape.NodeCollection) => {
    const all = cy.elements();
    show(fitZoom(all) >= READABLE_ZOOM || fresh.empty() ? all : fresh.closedNeighborhood(), 320);
  };

  const place = (animate: boolean, fresh?: cytoscape.NodeCollection) => {
    const root = cy.nodes('.root').first();
    const positions = computeLayout(
      cy.nodes().map((n) => n.id()),
      cy.edges().map((e) => [e.source().id(), e.target().id()] as [string, string]),
      root.nonempty() ? root.id() : undefined,
      mode,
    );
    running?.stop();
    running = cy.layout({
      name: 'preset',
      positions: (node: cytoscape.NodeSingular) => positions[node.id()] ?? node.position(),
      fit: false,
      padding: PADDING,
      animate: animate && !headless,
      animationDuration: 320,
      animationEasing: 'ease-out',
    } as cytoscape.LayoutOptions);
    running.one('layoutstop', () => (fresh ? frame(fresh) : show(cy.elements(), animate ? 200 : 0)));
    running.run();
  };

  const mark = (id: string | null) => {
    cy.elements('.highlighted').removeClass('highlighted');
    highlighted = id;
    const el = id ? cy.getElementById(resolve(id)!) : cy.collection();
    if (el.nonempty()) {
      el.addClass('highlighted');
      const bb = el.boundingBox();
      const ext = cy.extent();
      const outside = bb.x1 < ext.x1 || bb.x2 > ext.x2 || bb.y1 < ext.y1 || bb.y2 > ext.y2;
      // Chosen in a list while the drawing is too small to read (or looking
      // elsewhere): go to it and what touches it.
      if (!tapping && (outside || cy.zoom() < READABLE_ZOOM)) {
        show(around(el), 280);
      }
    }
    emphasise();
  };

  return {
    setElements(elements) {
      const want = new Map(elements.map((e) => [e.data.id, e]));
      let changed = false;
      let added = cy.collection();
      cy.batch(() => {
        const gone = cy.elements().filter((el) => !want.has(el.id()));
        if (gone.nonempty()) changed = true;
        gone.remove();
        const fresh: GraphElement[] = [];
        for (const e of elements) {
          const el = cy.getElementById(e.data.id);
          if (el.empty()) fresh.push(e);
          else {
            // keep the classes this module owns
            const own = ['highlighted', 'faded', 'near', 'labelled'].filter((c) => el.hasClass(c));
            el.classes([e.classes, ...own].join(' '));
            el.data(e.data);
          }
        }
        if (fresh.length === 0) return;
        changed = true;
        // A new node starts where the node it hangs from is, and grows out of it.
        const start = new Map<string, cytoscape.Position>();
        for (const e of fresh) {
          if (e.group !== 'edges') continue;
          for (const [a, b] of [
            [e.data.source!, e.data.target!],
            [e.data.target!, e.data.source!],
          ] as const) {
            const anchor = cy.getElementById(a);
            if (anchor.nonempty() && !start.has(b)) start.set(b, { ...anchor.position() });
          }
        }
        added = cy.add(
          fresh.map((e) => ({
            group: e.group,
            data: e.data,
            classes: e.classes,
            ...(e.group === 'nodes' && start.has(e.data.id) ? { position: start.get(e.data.id)! } : {}),
          })),
        );
      });
      lineOf.clear();
      for (const e of elements) for (const m of e.data.members ?? []) lineOf.set(m, e.data.id);
      if (changed && cy.nodes().nonempty()) {
        place(!first, first ? undefined : added.nodes());
        first = false;
      }
      mark(highlighted);
    },
    highlight: mark,
    setEdgeLabels(m) {
      labels = m;
      emphasise();
    },
    restyle() {
      cy.style().fromJson(graphStylesheet(readPalette())).update();
    },
    fit() {
      show(cy.elements(), 200);
    },
    zoom(factor) {
      const level = Math.min(cy.maxZoom(), Math.max(cy.minZoom(), cy.zoom() * factor));
      cy.zoom({ level, renderedPosition: { x: cy.width() / 2, y: cy.height() / 2 } });
    },
    arrange() {
      place(true);
    },
    resize() {
      cy.resize();
      show(cy.elements(), 0);
    },
    destroy() {
      running?.stop();
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
        labelled: cy.edges('.labelled').map((el) => el.id()).sort(),
        positions,
      };
    },
  };
}
