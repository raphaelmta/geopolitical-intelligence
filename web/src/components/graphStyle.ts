// Pure graph data and styling for Cytoscape: no canvas needed, so all of it
// is tested in jsdom. Fact = solid line, inference = dashed line, and every
// edge also carries the same words as the list ("fato", "inferência · NN%",
// "identidade NN%" / "identidade: decisão de analista", "vencido",
// "encerrado") as its label: never color alone.
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

function nodeElement(ref: EntityRef, root: boolean): GraphElement {
  return {
    group: 'nodes',
    data: { id: nodeElementId(ref.id), label: ref.name },
    classes: [kindClass(ref.kind), root ? 'root' : ''].filter(Boolean).join(' '),
  };
}

/**
 * Cytoscape elements for a set of edges: one node per entity id, one edge
 * per relationship id (a repeated relationship is kept once; parallel
 * relationships and self-loops stay distinct). extraNodes adds entities with
 * no edge on screen (the starting entity); root marks one node.
 */
export function toElements(edges: Edge[], extraNodes: EntityRef[] = [], root?: number): GraphElement[] {
  const nodes = new Map<number, GraphElement>();
  const out: GraphElement[] = [];
  const seen = new Set<number>();
  const addNode = (ref: EntityRef) => {
    if (!nodes.has(ref.id)) nodes.set(ref.id, nodeElement(ref, ref.id === root));
  };
  extraNodes.forEach(addNode);
  for (const e of edges) {
    if (seen.has(e.relationship_id)) continue;
    seen.add(e.relationship_id);
    addNode(e.src);
    addNode(e.dst);
    const classes = [e.kind === 'inference' ? 'inference' : 'fact'];
    if (isClosed(e)) classes.push('closed');
    if (e.stale) classes.push('stale');
    if (identityMerged(e)) classes.push('identity');
    out.push({
      group: 'edges',
      data: {
        id: edgeElementId(e.relationship_id),
        source: nodeElementId(e.src.id),
        target: nodeElementId(e.dst.id),
        label: `${relationTypeName(e.type)} · ${natureText(e)}`,
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

/** Shape per entity kind (the legend shows the same). */
export const KIND_SHAPES: Record<string, string> = {
  person: 'ellipse',
  company: 'round-rectangle',
  party: 'diamond',
  public_body: 'hexagon',
  foreign: 'triangle',
};

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
        width: 22,
        height: 22,
        'background-color': p.other,
        'border-width': 1,
        'border-color': p.text,
        color: p.text,
        'font-size': 11,
        'min-zoomed-font-size': 8,
        'text-valign': 'bottom',
        'text-margin-y': 4,
        'text-wrap': 'ellipsis',
        'text-max-width': '160px',
        'text-background-color': p.surface,
        'text-background-opacity': 0.85,
        'text-background-padding': '1px',
      },
    },
    ...NODE_KINDS.map((k) => ({
      selector: `node.kind-${k}`,
      style: { shape: KIND_SHAPES[k], 'background-color': kindColor[k] },
    })),
    { selector: 'node.root', style: { 'border-width': 3, width: 30, height: 30 } },
    {
      selector: 'edge',
      style: {
        width: 1.5,
        'curve-style': 'bezier',
        'line-color': p.fact,
        'target-arrow-color': p.fact,
        'target-arrow-shape': 'triangle',
        'arrow-scale': 0.8,
        label: 'data(label)',
        color: p.muted,
        'font-size': 9,
        'min-zoomed-font-size': 10,
        'text-rotation': 'autorotate',
        'text-background-color': p.surface,
        'text-background-opacity': 0.85,
        'text-background-padding': '1px',
      },
    },
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
    // at the start of the line; the words are in the label.
    { selector: 'edge.closed', style: { 'target-arrow-shape': 'tee', 'arrow-scale': 1.2, opacity: 0.55, width: 1 } },
    {
      selector: 'edge.stale',
      style: { 'mid-target-arrow-shape': 'square', 'mid-target-arrow-fill': 'hollow', 'mid-target-arrow-color': p.stale },
    },
    {
      selector: 'edge.identity',
      style: { 'source-arrow-shape': 'circle', 'source-arrow-fill': 'hollow', 'source-arrow-color': p.identity },
    },
    {
      selector: 'node.highlighted',
      style: { 'border-width': 4, 'border-color': p.focus, 'overlay-color': p.focus, 'overlay-opacity': 0.15, 'overlay-padding': 6 },
    },
    {
      selector: 'edge.highlighted',
      style: { width: 4, opacity: 1, 'overlay-color': p.focus, 'overlay-opacity': 0.2, 'overlay-padding': 4 },
    },
  ] as StylesheetJson;
}
