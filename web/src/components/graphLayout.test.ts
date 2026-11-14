import { describe, expect, test } from 'vitest';
import { computeLayout } from './graphLayout';

const COLUMN_GAP = 250; // graphLayout.COLUMN_GAP

const leaves = (parent: string, n: number, prefix = 'l') =>
  Array.from({ length: n }, (_, i) => `${prefix}${parent}_${i}`);

function star(n: number) {
  const kids = leaves('r', n);
  return { nodes: ['r', ...kids], edges: kids.map((k) => ['r', k] as [string, string]) };
}

function minDistance(pos: Record<string, { x: number; y: number }>) {
  const pts = Object.values(pos);
  let min = Infinity;
  for (let i = 0; i < pts.length; i++) {
    for (let j = i + 1; j < pts.length; j++) {
      min = Math.min(min, Math.hypot(pts[i]!.x - pts[j]!.x, pts[i]!.y - pts[j]!.y));
    }
  }
  return min;
}

const dist = (p: { x: number; y: number }) => Math.hypot(p.x, p.y);

describe('computeLayout', () => {
  test('sem nós não há posições', () => {
    expect(computeLayout([], [])).toEqual({});
    expect(computeLayout([], [], 'x', 'flow')).toEqual({});
  });

  test('um nó só fica na origem', () => {
    expect(computeLayout(['a'], [])).toEqual({ a: { x: 0, y: 0 } });
  });

  test('a raiz pedida fica na origem, qualquer que seja a ordem dos nós', () => {
    const s = star(5);
    const pos = computeLayout(['l0', ...s.nodes], s.edges, 'r');
    expect(pos.r).toEqual({ x: 0, y: 0 });
    for (const k of leaves('r', 5)) expect(dist(pos[k]!)).toBeGreaterThan(0);
  });

  test('sem raiz pedida, o nó mais ligado vira a raiz', () => {
    const s = star(4);
    const pos = computeLayout([...s.nodes].reverse(), s.edges);
    expect(pos.r).toEqual({ x: 0, y: 0 });
  });

  test('a mesma entrada dá sempre a mesma saída', () => {
    const kids = leaves('r', 12);
    const nodes = ['r', ...kids, ...leaves('l_r_3', 30, 'c')];
    const edges: [string, string][] = [
      ...kids.map((k) => ['r', k] as [string, string]),
      ...leaves('l_r_3', 30, 'c').map((c) => ['l_r_3', c] as [string, string]),
    ];
    const a = computeLayout(nodes, edges, 'r');
    const b = computeLayout([...nodes], edges.map((e) => [...e] as [string, string]), 'r');
    expect(b).toEqual(a);
  });

  test('os filhos de um vizinho expandido ficam mais longe da raiz que o pai', () => {
    const nodes = ['r', 'a', 'b', 'c', 'a1', 'a2', 'a3'];
    const edges: [string, string][] = [
      ['r', 'a'], ['r', 'b'], ['r', 'c'], ['a', 'a1'], ['a', 'a2'], ['a', 'a3'],
    ];
    const pos = computeLayout(nodes, edges, 'r');
    for (const k of ['a1', 'a2', 'a3']) expect(dist(pos[k]!)).toBeGreaterThan(dist(pos.a!));
  });

  test('estrela de 60 folhas: nenhum par de nós mais perto que 60 px', () => {
    const s = star(60);
    expect(minDistance(computeLayout(s.nodes, s.edges, 'r'))).toBeGreaterThanOrEqual(60);
  });

  test('raiz com 12 vizinhos, um deles com 30 filhos: nenhum par mais perto que 60 px', () => {
    const kids = leaves('r', 12);
    const grand = leaves('x', 30, 'g');
    const nodes = ['r', ...kids, ...grand];
    const edges: [string, string][] = [
      ...kids.map((k) => ['r', k] as [string, string]),
      ...grand.map((g) => [kids[4]!, g] as [string, string]),
    ];
    const pos = computeLayout(nodes, edges, 'r');
    expect(Object.keys(pos)).toHaveLength(nodes.length);
    expect(minDistance(pos)).toBeGreaterThanOrEqual(60);
    for (const g of grand) expect(dist(pos[g]!)).toBeGreaterThan(dist(pos[kids[4]!]!));
  });

  test('"flow" põe a profundidade d em x = d · espaçamento, com a raiz em x = 0', () => {
    const nodes = ['r', 'a', 'b', 'c', 'd'];
    const edges: [string, string][] = [['r', 'a'], ['r', 'b'], ['a', 'c'], ['c', 'd']];
    const pos = computeLayout(nodes, edges, 'r', 'flow');
    expect(pos.r!.x).toBe(0);
    expect(pos.a!.x).toBe(COLUMN_GAP);
    expect(pos.b!.x).toBe(COLUMN_GAP);
    expect(pos.c!.x).toBe(2 * COLUMN_GAP);
    expect(pos.d!.x).toBe(3 * COLUMN_GAP);
    // two in a column do not overlap
    expect(pos.a!.y).not.toBe(pos.b!.y);
  });

  test('nós inalcançáveis também ganham posição, sem cair em cima da raiz', () => {
    const nodes = ['r', 'a', 'z1', 'z2', 'z3'];
    const edges: [string, string][] = [['r', 'a'], ['z1', 'z2']]; // z3 is alone
    for (const mode of ['radial', 'flow'] as const) {
      const pos = computeLayout(nodes, edges, 'r', mode);
      expect(Object.keys(pos).sort()).toEqual([...nodes].sort());
      for (const p of Object.values(pos)) {
        expect(Number.isFinite(p.x)).toBe(true);
        expect(Number.isFinite(p.y)).toBe(true);
      }
      expect(minDistance(pos)).toBeGreaterThan(30);
    }
  });

  test('arestas com pontas desconhecidas e laços são ignoradas', () => {
    const pos = computeLayout(['r', 'a'], [['r', 'a'], ['a', 'a'], ['a', 'ghost']], 'r');
    expect(Object.keys(pos).sort()).toEqual(['a', 'r']);
  });
});
