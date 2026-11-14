// Pure graph data and styling for Cytoscape: no canvas needed, so all of it
// is tested in jsdom.
//
// What the drawing says without words: an icon and a color per kind of
// entity; a solid line for a fact and a dashed one for an inference; a bar
// instead of the arrow for a closed link. The words ("fato", "inferência ·
// NN%", "identidade NN%", "vencido", "encerrado") are in the details panel
// and in the lists beside the drawing, never in color alone.
//
// Links between the same two entities in the same direction are drawn as ONE
// line ("Sociedade · Administração", or "3 vínculos"): the panel lists each.
import type { StylesheetJson } from 'cytoscape';
import type { Edge, EntityRef } from '../api/types';
import { CONFIDENCE_MISSING, formatConfidence, kindLabel, relationTypeName } from '../api/format';
import { identityMerged, identityTag, isClosed } from '../api/edges';

export interface GraphElement {
  group: 'nodes' | 'edges';
  data: {
    id: string;
    label: string;
    source?: string;
    target?: string;
    /** Edge element ids of every relationship this line stands for (itself included). */
    members?: string[];
    /** What the pointer shows over the element: the full name and kind, or what the line stands for. */
    tip?: string;
  };
  classes: string;
}

export const nodeElementId = (entityId: number) => `n${entityId}`;
export const edgeElementId = (relationshipId: number) => `e${relationshipId}`;

/** Entity id of a node element id ("n12" → 12), or null for an edge. */
export function entityIdOf(elementId: string): number | null {
  return /^n\d+$/.test(elementId) ? Number(elementId.slice(1)) : null;
}

export const NODE_KINDS = ['person', 'company', 'party', 'public_body', 'foreign'] as const;

function kindClass(kind: string): string {
  return (NODE_KINDS as readonly string[]).includes(kind) ? `kind-${kind}` : 'kind-other';
}

/** The words of EdgeBadge, as plain text (for the canvas label). */
export function natureText(e: Edge): string {
  const parts = [
    e.kind === 'inference' ? `inferência · ${formatConfidence(e.confidence) ?? CONFIDENCE_MISSING}` : 'fato',
  ];
  const identity = identityTag(e);
  if (identity) parts.push(identity.text);
  if (e.stale) parts.push('vencido');
  if (isClosed(e)) parts.push('encerrado');
  return parts.join(' · ');
}

/** Longest node label drawn; the full name is in the panel and the lists. */
const LABEL_MAX = 38;

function shortName(name: string): string {
  return name.length > LABEL_MAX ? `${name.slice(0, LABEL_MAX - 1).trimEnd()}…` : name;
}

function nodeElement(ref: EntityRef, root: boolean, collapsed: boolean): GraphElement {
  return {
    group: 'nodes',
    data: { id: nodeElementId(ref.id), label: shortName(ref.name), tip: `${ref.name} · ${kindLabel(ref.kind)}` },
    classes: [kindClass(ref.kind), root ? 'root' : '', collapsed ? 'collapsed' : ''].filter(Boolean).join(' '),
  };
}

/** Relationships between the same two entities, same direction, each once. */
export function bundleEdges(edges: Edge[]): Edge[][] {
  const seen = new Set<number>();
  const bundles = new Map<string, Edge[]>();
  for (const e of edges) {
    if (seen.has(e.relationship_id)) continue;
    seen.add(e.relationship_id);
    const key = `${e.src.id}>${e.dst.id}`;
    const b = bundles.get(key);
    if (b) b.push(e);
    else bundles.set(key, [e]);
  }
  return [...bundles.values()];
}

/** The short text on a line: the type names, or how many links it stands for. */
export function bundleLabel(bundle: Edge[]): string {
  const names = [...new Set(bundle.map((e) => relationTypeName(e.type)))];
  if (names.length <= 2) return names.join(' · ');
  return `${bundle.length} vínculos`;
}

/** Every entity at an end of the edges (plus the extra ones), by id. */
export function entitiesOf(edges: Edge[], extra: EntityRef[] = []): Map<number, EntityRef> {
  const m = new Map<number, EntityRef>(extra.map((e) => [e.id, e]));
  for (const e of edges) {
    if (!m.has(e.src.id)) m.set(e.src.id, e.src);
    if (!m.has(e.dst.id)) m.set(e.dst.id, e.dst);
  }
  return m;
}

export interface ElementOptions {
  /** Entities whose own links were not loaded yet: drawn with a "+" mark. */
  collapsed?: ReadonlySet<number>;
}

/**
 * Cytoscape elements for a set of edges: one node per entity id and one line
 * per pair of entities and direction (see bundleEdges). The line takes the
 * element id of its first relationship and lists all of them in
 * data.members. extraNodes adds entities with no edge on screen (the
 * starting entity); root marks one node.
 */
export function toElements(
  edges: Edge[],
  extraNodes: EntityRef[] = [],
  root?: number,
  { collapsed }: ElementOptions = {},
): GraphElement[] {
  const nodes = new Map<number, GraphElement>();
  const out: GraphElement[] = [];
  const addNode = (ref: EntityRef) => {
    if (!nodes.has(ref.id)) nodes.set(ref.id, nodeElement(ref, ref.id === root, collapsed?.has(ref.id) ?? false));
  };
  extraNodes.forEach(addNode);
  for (const bundle of bundleEdges(edges)) {
    const first = bundle[0]!;
    addNode(first.src);
    addNode(first.dst);
    // A line is dashed only when nothing in it is a fact, and "closed" only
    // when nothing in it is current.
    const classes = [bundle.every((e) => e.kind === 'inference') ? 'inference' : 'fact'];
    if (bundle.every(isClosed)) classes.push('closed');
    if (bundle.some((e) => e.stale)) classes.push('stale');
    if (bundle.some(identityMerged)) classes.push('identity');
    if (bundle.length > 1) classes.push('multiple');
    out.push({
      group: 'edges',
      data: {
        id: edgeElementId(first.relationship_id),
        source: nodeElementId(first.src.id),
        target: nodeElementId(first.dst.id),
        label: bundleLabel(bundle),
        tip: [...new Set(bundle.map((e) => relationTypeName(e.type)))].join(' · '),
        members: bundle.map((e) => edgeElementId(e.relationship_id)),
      },
      classes: classes.join(' '),
    });
  }
  return [...nodes.values(), ...out];
}

export interface Palette {
  text: string;
  muted: string;
  surface: string;
  fact: string;
  inference: string;
  stale: string;
  identity: string;
  focus: string;
  line: string;
  edge: string;
  person: string;
  company: string;
  party: string;
  publicBody: string;
  foreign: string;
  other: string;
}

/** Custom property and fallback (light theme) of each palette entry. */
const TOKENS: Record<keyof Palette, [string, string]> = {
  text: ['--text', '#1b1f24'],
  muted: ['--muted', '#555c66'],
  surface: ['--surface', '#ffffff'],
  fact: ['--fact', '#1b1f24'],
  inference: ['--inference', '#6b3fa0'],
  stale: ['--stale-text', '#6b3d00'],
  identity: ['--identity', '#7a5200'],
  focus: ['--focus', '#b35c00'],
  line: ['--line', '#d6d3cb'],
  edge: ['--edge', '#7b828c'],
  person: ['--node-person', '#2f6f9f'],
  company: ['--node-company', '#3d7a4a'],
  party: ['--node-party', '#a0452f'],
  publicBody: ['--node-public-body', '#6a5d2b'],
  foreign: ['--node-foreign', '#5b5f66'],
  other: ['--line-strong', '#8a8f98'],
};

/** Colors from the stylesheet's custom properties (light or dark theme). */
export function readPalette(): Palette {
  const cs = getComputedStyle(document.documentElement);
  const p = {} as Palette;
  for (const [key, [prop, fallback]] of Object.entries(TOKENS) as [keyof Palette, [string, string]][]) {
    p[key] = cs.getPropertyValue(prop).trim() || fallback;
  }
  return p;
}

/** Glyph per entity kind, as the inside of a 24×24 SVG (the legend draws the same). */
export const KIND_ICONS: Record<string, string> = {
  person: '<circle cx="12" cy="8" r="4"/><path d="M4 21c0-4.6 3.5-7.5 8-7.5s8 2.9 8 7.5z"/>',
  company:
    '<path fill-rule="evenodd" d="M4 21V3h10v6h6v12h-5v-4h-4v4zM7 6v2h2V6zm4 0v2h1V6zM7 10v2h2v-2zm4 0v2h1v-2zM7 14v2h2v-2zm9-2v2h2v-2zm0 4v2h2v-2z"/>',
  party: '<path d="M5 3h2v18H5zM8.5 4H20l-3 4 3 4H8.5z"/>',
  public_body: '<path d="M12 2.5 21.5 8v2h-19V8zM5 11.5h2.2V18H5zm4.6 0h2.2V18H9.6zm4.6 0h2.2V18h-2.2zm4.6 0H21V18h-2.2zM3 19.2h18v2.3H3z"/>',
  foreign:
    '<g fill="none" stroke-width="1.8"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c3.2 3 3.2 15 0 18M12 3c-3.2 3-3.2 15 0 18"/></g>',
};

/**
 * The image of a node: the glyph of its kind and, for an entity whose links
 * were not loaded yet, a "+" mark. ONE image 1.5 times the node (the node is
 * the middle two thirds), so the mark sits on the node's upper right edge.
 */
function nodeUri(inner: string, glyph: string, plus: { fill: string; ink: string } | null): string {
  const svg =
    // width and height: without them the browser gives the image no size of its own
    `<svg xmlns="http://www.w3.org/2000/svg" width="144" height="144" viewBox="0 0 36 36">` +
    `<g transform="translate(11.04 11.04) scale(0.58)" fill="${glyph}" stroke="${glyph}" stroke-width="0">${inner}</g>` +
    (plus
      ? `<circle cx="27.5" cy="8.5" r="5.6" fill="${plus.fill}" stroke="${plus.ink}" stroke-width="1"/>` +
        `<path d="M27.5 5.6v5.8M24.6 8.5h5.8" fill="none" stroke="${plus.ink}" stroke-width="1.5" stroke-linecap="round"/>`
      : '') +
    `</svg>`;
  return `data:image/svg+xml;utf8,${encodeURIComponent(svg)}`;
}

export const LEGEND_KINDS = NODE_KINDS.map((k) => ({ kind: k, label: kindLabel(k) }));

export function graphStylesheet(p: Palette): StylesheetJson {
  const kindColor: Record<string, string> = {
    person: p.person,
    company: p.company,
    party: p.party,
    public_body: p.publicBody,
    foreign: p.foreign,
  };
  return [
    {
      selector: 'node',
      style: {
        label: 'data(label)',
        shape: 'ellipse',
        width: 42,
        height: 42,
        'background-color': p.other,
        'border-width': 2,
        'border-color': p.surface,
        color: p.text,
        'font-size': 14,
        'line-height': 1.15,
        'min-zoomed-font-size': 7,
        'text-valign': 'bottom',
        'text-margin-y': 6,
        'text-wrap': 'wrap',
        'text-max-width': '150px',
        'text-background-color': p.surface,
        'text-background-opacity': 0.9,
        'text-background-padding': '2px',
        'text-background-shape': 'roundrectangle',
        'transition-property': 'opacity',
        'transition-duration': 120,
      },
    },
    ...NODE_KINDS.map((k) => ({
      selector: `node.kind-${k}`,
      style: {
        'background-color': kindColor[k],
        'background-image': nodeUri(KIND_ICONS[k]!, p.surface, null),
        'background-width': '150%',
        'background-height': '150%',
        'background-clip': 'none',
        'background-image-containment': 'over',
        'bounds-expansion': 14,
      },
    })),
    ...NODE_KINDS.map((k) => ({
      selector: `node.kind-${k}.collapsed`,
      style: {
        'background-image': nodeUri(KIND_ICONS[k]!, p.surface, { fill: p.surface, ink: p.text }),
      },
    })),
    {
      selector: 'node.root',
      style: {
        width: 54,
        height: 54,
        'font-size': 15,
        'font-weight': 700,
        'text-max-width': '170px',
        'border-width': 3,
        'border-color': p.text,
      },
    },
    {
      selector: 'edge',
      style: {
        width: 1.6,
        'curve-style': 'bezier',
        'control-point-step-size': 46,
        'line-color': p.edge,
        'target-arrow-color': p.edge,
        'target-arrow-shape': 'triangle',
        'arrow-scale': 0.9,
        'source-distance-from-node': 3,
        'target-distance-from-node': 3,
        color: p.muted,
        'font-size': 11,
        'min-zoomed-font-size': 7,
        'text-rotation': 'autorotate',
        'text-background-color': p.surface,
        'text-background-opacity': 1,
        'text-background-padding': '2px',
        'text-background-shape': 'roundrectangle',
        'transition-property': 'opacity',
        'transition-duration': 120,
      },
    },
    // The words on a line are shown when the drawing is small enough to
    // carry them, and always for the links of the element under the pointer
    // or selected (graphAdapter decides).
    { selector: 'edge.labelled', style: { label: 'data(label)' } },
    { selector: 'edge.multiple', style: { width: 2.6 } },
    { selector: 'edge.fact', style: { 'line-style': 'solid' } },
    {
      selector: 'edge.inference',
      style: {
        'line-style': 'dashed',
        'line-dash-pattern': [6, 4],
        'line-color': p.inference,
        'target-arrow-color': p.inference,
      },
    },
    // Closed: a bar at the end (⊣) instead of the arrow, plus faded and
    // thinner. The bar does not depend on zoom or opacity, and leaves the
    // line style (solid = fact, dashed = inference) untouched.
    // Stale: a hollow square marker mid-edge.
    // Merged identity (automatic, analyst or unknown basis): a hollow circle
    // at the start of the line; the words are in the panel.
    { selector: 'edge.closed', style: { 'target-arrow-shape': 'tee', 'arrow-scale': 1.2, opacity: 0.6, width: 1.2 } },
    {
      selector: 'edge.stale',
      style: { 'mid-target-arrow-shape': 'square', 'mid-target-arrow-fill': 'hollow', 'mid-target-arrow-color': p.stale },
    },
    {
      selector: 'edge.identity',
      style: { 'source-arrow-shape': 'circle', 'source-arrow-fill': 'hollow', 'source-arrow-color': p.identity },
    },
    // Everything that is not near the element under the pointer or selected.
    { selector: '.faded', style: { opacity: 0.13 } },
    { selector: 'edge.near', style: { color: p.text, 'z-index': 5 } },
    {
      selector: 'node.highlighted',
      style: {
        'border-width': 3,
        'border-color': p.focus,
        'underlay-color': p.focus,
        'underlay-opacity': 0.22,
        'underlay-padding': 9,
        'underlay-shape': 'ellipse',
        'z-index': 10,
      },
    },
    {
      selector: 'edge.highlighted',
      style: {
        width: 3.4,
        opacity: 1,
        'line-color': p.focus,
        'target-arrow-color': p.focus,
        color: p.text,
        'font-weight': 700,
        'z-index': 10,
      },
    },
  ] as StylesheetJson;
}
