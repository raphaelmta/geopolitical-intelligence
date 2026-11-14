// Positions for the drawing, computed without a canvas (tested in jsdom).
//
// "radial": the starting entity in the middle, what is linked to it on a
// ring around it, and what an expanded neighbour brings on the next ring, in
// the sector of that neighbour. The same input always gives the same
// drawing, and an expansion only widens rings: nothing jumps at random, as a
// force layout would.
//
// "flow": columns by distance from the start (paths read left to right).

export interface Point {
  x: number;
  y: number;
}

export type LayoutMode = 'radial' | 'flow';

/** Room a node needs along a ring: its label is about this wide. */
const SPACING = 112;
/** Distance between two rings. */
const RING_GAP = 150;
/** Distance between two rows of one crowded ring. */
const ROW_GAP = 104;
/** A ring may grow this much before it is split into rows. */
const RING_STRETCH = 1.7;
/** Sector share of a subtree is capped, so one hub does not squeeze its siblings. */
const WEIGHT_CAP = 12;
/** Rings are wider than tall: the canvas is, and names are written sideways. */
const STRETCH_X = 1.4;
const COLUMN_GAP = 250;
const FLOW_ROW_GAP = 96;

interface Tree {
  root: string;
  depth: Map<string, number>;
  children: Map<string, string[]>;
  order: string[];
}

/** Breadth-first tree over the undirected graph; unreachable nodes hang from the root. */
function spanningTree(nodes: string[], edges: [string, string][], rootId?: string): Tree | null {
  if (nodes.length === 0) return null;
  const known = new Set(nodes);
  const adj = new Map<string, string[]>(nodes.map((n) => [n, []]));
  for (const [a, b] of edges) {
    if (a === b || !known.has(a) || !known.has(b)) continue;
    if (!adj.get(a)!.includes(b)) adj.get(a)!.push(b);
    if (!adj.get(b)!.includes(a)) adj.get(b)!.push(a);
  }
  let root = rootId != null && known.has(rootId) ? rootId : nodes[0]!;
  if (rootId == null || !known.has(rootId)) {
    for (const n of nodes) if (adj.get(n)!.length > adj.get(root)!.length) root = n;
  }
  const depth = new Map<string, number>([[root, 0]]);
  const children = new Map<string, string[]>(nodes.map((n) => [n, []]));
  const order: string[] = [];
  const walk = (start: string) => {
    const queue = [start];
    while (queue.length) {
      const n = queue.shift()!;
      order.push(n);
      for (const m of adj.get(n)!) {
        if (depth.has(m)) continue;
        depth.set(m, depth.get(n)! + 1);
        children.get(n)!.push(m);
        queue.push(m);
      }
    }
  };
  walk(root);
  for (const n of nodes) {
    if (depth.has(n)) continue;
    depth.set(n, 1);
    children.get(root)!.push(n);
    walk(n);
  }
  return { root, depth, children, order };
}

function radial(tree: Tree): Record<string, Point> {
  const weight = new Map<string, number>();
  for (const n of [...tree.order].reverse()) {
    const kids = tree.children.get(n)!;
    const sum = kids.reduce((s, k) => s + weight.get(k)!, 0);
    weight.set(n, kids.length ? Math.min(Math.max(sum, 1), WEIGHT_CAP) : 1);
  }
  const out: Record<string, Point> = { [tree.root]: { x: 0, y: 0 } };
  const maxDepth = Math.max(...tree.depth.values());
  // start/end angle of the sector each node may give to its children
  const sector = new Map<string, [number, number]>([[tree.root, [-Math.PI / 2, (3 * Math.PI) / 2]]]);
  let inner = 0; // outermost radius used so far
  for (let d = 1; d <= maxDepth; d++) {
    const parents = tree.order.filter((n) => tree.depth.get(n) === d - 1 && tree.children.get(n)!.length > 0);
    const base = inner + RING_GAP;
    // One radius for the whole ring: the widest any parent needs, within the stretch.
    let ring = base;
    for (const p of parents) {
      const kids = tree.children.get(p)!;
      const [a, b] = sector.get(p)!;
      const total = kids.reduce((s, k) => s + weight.get(k)!, 0);
      const narrowest = Math.min(...kids.map((k) => ((b - a) * weight.get(k)!) / total));
      const full = b - a >= 2 * Math.PI - 1e-9;
      if (kids.length > 1 || full) ring = Math.max(ring, Math.min(SPACING / narrowest, base * RING_STRETCH));
    }
    let outer = ring;
    for (const p of parents) {
      const kids = tree.children.get(p)!;
      const [a, b] = sector.get(p)!;
      const span = b - a;
      const total = kids.reduce((s, k) => s + weight.get(k)!, 0);
      const narrowest = Math.min(...kids.map((k) => (span * weight.get(k)!) / total));
      if (kids.length === 1 || ring * narrowest >= SPACING - 1e-6) {
        // Classic: every child in the middle of its own share of the sector.
        let at = a;
        for (const k of kids) {
          const share = (span * weight.get(k)!) / total;
          const mid = at + share / 2;
          out[k] = { x: STRETCH_X * ring * Math.cos(mid), y: ring * Math.sin(mid) };
          sector.set(k, [at, at + share]);
          at += share;
        }
        continue;
      }
      // Crowded: rows of the same sector, each row a little further out.
      let i = 0;
      for (let row = 0; i < kids.length; row++) {
        const r = ring + row * ROW_GAP;
        const capacity = Math.max(1, Math.floor((r * span) / SPACING));
        const here = kids.slice(i, i + capacity);
        const step = span / here.length;
        here.forEach((k, j) => {
          const mid = a + step * (j + 0.5);
          out[k] = { x: STRETCH_X * r * Math.cos(mid), y: r * Math.sin(mid) };
          const share = Math.max(step, (span * weight.get(k)!) / total);
          sector.set(k, [mid - share / 2, mid + share / 2]);
        });
        i += here.length;
        outer = Math.max(outer, r);
      }
    }
    inner = outer;
  }
  return out;
}

function flow(tree: Tree): Record<string, Point> {
  const columns = new Map<number, string[]>();
  for (const n of tree.order) {
    const d = tree.depth.get(n)!;
    columns.set(d, [...(columns.get(d) ?? []), n]);
  }
  const out: Record<string, Point> = {};
  for (const [d, ids] of columns) {
    ids.forEach((id, i) => {
      out[id] = { x: d * COLUMN_GAP, y: (i - (ids.length - 1) / 2) * FLOW_ROW_GAP };
    });
  }
  return out;
}

/** Position of every node; edges are [source, target] pairs of node ids. */
export function computeLayout(
  nodes: string[],
  edges: [string, string][],
  root?: string,
  mode: LayoutMode = 'radial',
): Record<string, Point> {
  const tree = spanningTree(nodes, edges, root);
  if (!tree) return {};
  return mode === 'flow' ? flow(tree) : radial(tree);
}
