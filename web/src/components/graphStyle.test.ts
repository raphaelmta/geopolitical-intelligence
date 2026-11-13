import { afterEach, describe, expect, test, vi } from 'vitest';
import cytoscape from 'cytoscape';
import { edgeElementId, graphStylesheet, nodeElementId, readPalette, toElements } from './graphStyle';
import { edge } from '../test/fixtures';

const person = { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' };
const companyA = { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' };
const companyB = { id: 13, kind: 'company', name: 'EMPRESA FICTICIA B LTDA', natural_key: '90000003' };

function byId(els: ReturnType<typeof toElements>, id: string) {
  return els.find((e) => e.data.id === id);
}

describe('toElements', () => {
  test('toElements cria um nó por entidade e uma aresta por vínculo, sem duplicar nós', () => {
    const els = toElements([
      edge({ relationship_id: 1, src: person, dst: companyA }),
      edge({ relationship_id: 2, src: person, dst: companyB }),
      edge({ relationship_id: 3, type: 'administrator_of', src: person, dst: companyA }),
      edge({ relationship_id: 1, src: person, dst: companyA }), // same relationship twice
    ]);
    const nodes = els.filter((e) => e.group === 'nodes');
    const edges = els.filter((e) => e.group === 'edges');
    expect(nodes.map((n) => n.data.id).sort()).toEqual(['n11', 'n12', 'n13']);
    expect(edges.map((e) => e.data.id).sort()).toEqual(['e1', 'e2', 'e3']);
    const e1 = byId(els, 'e1')!;
    expect(e1.data.source).toBe('n11');
    expect(e1.data.target).toBe('n12');
    expect(byId(els, 'n11')!.data.label).toBe('PESSOA FICTICIA UM');
    expect(byId(els, 'n11')!.classes).toContain('kind-person');
    expect(byId(els, 'n12')!.classes).toContain('kind-company');
    expect(nodeElementId(11)).toBe('n11');
    expect(edgeElementId(1)).toBe('e1');
  });

  test('toElements marca aresta de inferência com classe "inference" e de fato com "fact"', () => {
    const els = toElements([
      edge({ relationship_id: 1, kind: 'fact' }),
      edge({ relationship_id: 2, kind: 'inference', confidence: 0.8, src: person, dst: companyB }),
    ]);
    const fact = byId(els, 'e1')!;
    const inference = byId(els, 'e2')!;
    expect(fact.classes!.split(' ')).toContain('fact');
    expect(fact.classes!.split(' ')).not.toContain('inference');
    expect(inference.classes!.split(' ')).toContain('inference');
    expect(inference.classes!.split(' ')).not.toContain('fact');
    // the label carries the same words as the list (never line style alone)
    expect(fact.data.label).toContain('fato');
    expect(inference.data.label).toContain('inferência · 80%');
  });

  test('encerrado, vencido e identidade unida viram classes e texto', () => {
    const els = toElements([
      edge({ relationship_id: 1, closed: true, valid_to_after: '2026-01-01', valid_to_before: '2026-02-01' }),
      edge({ relationship_id: 2, stale: true, identity_basis: 'auto', identity_confidence: 0.9, src: person, dst: companyB }),
    ]);
    const closed = byId(els, 'e1')!;
    const stale = byId(els, 'e2')!;
    expect(closed.classes!.split(' ')).toEqual(expect.arrayContaining(['fact', 'closed']));
    expect(closed.data.label).toContain('encerrado');
    expect(stale.classes!.split(' ')).toEqual(expect.arrayContaining(['fact', 'stale', 'identity']));
    expect(stale.classes!.split(' ')).not.toContain('closed');
    expect(stale.data.label).toContain('vencido');
    expect(stale.data.label).toContain('identidade 90%');
  });

  test('encerrado vem do campo closed da API, não das datas', () => {
    const els = toElements([
      edge({ relationship_id: 1, closed: false, valid_to_before: '2026-02-01' }),
      edge({ relationship_id: 2, closed: true, src: person, dst: companyB }),
    ]);
    expect(byId(els, 'e1')!.classes.split(' ')).not.toContain('closed');
    expect(byId(els, 'e1')!.data.label).not.toContain('encerrado');
    expect(byId(els, 'e2')!.classes.split(' ')).toContain('closed');
  });

  test.each([
    ['auto', 0.99999, 'identidade 99%'],
    ['analyst', 1, 'identidade: decisão de analista'],
    ['unknown', 0.9, 'identidade: base não informada'],
  ] as const)('identidade %s (confiança %s): classe "identity" e texto "%s"', (basis, c, text) => {
    const els = toElements([edge({ relationship_id: 1, identity_basis: basis, identity_confidence: c })]);
    expect(byId(els, 'e1')!.classes.split(' ')).toContain('identity');
    expect(byId(els, 'e1')!.data.label).toContain(text);
  });

  test('identidade "none" não marca a aresta, mesmo com confiança abaixo de 1', () => {
    const els = toElements([edge({ relationship_id: 1, identity_basis: 'none', identity_confidence: 0.9 })]);
    expect(byId(els, 'e1')!.classes.split(' ')).not.toContain('identity');
    expect(byId(els, 'e1')!.data.label).not.toContain('identidade');
  });

  test('laço (entidade ligada a si mesma) e vínculos paralelos ficam como arestas distintas', () => {
    const els = toElements([
      edge({ relationship_id: 1, src: companyA, dst: companyA, type: 'shareholder_of' }),
      edge({ relationship_id: 2, src: person, dst: companyA }),
      edge({ relationship_id: 3, src: person, dst: companyA }),
    ]);
    expect(els.filter((e) => e.group === 'nodes')).toHaveLength(2);
    const loop = byId(els, 'e1')!;
    expect(loop.data.source).toBe('n12');
    expect(loop.data.target).toBe('n12');
    expect(byId(els, 'e2')!.data.source).toBe(byId(els, 'e3')!.data.source);
    expect(els.filter((e) => e.group === 'edges')).toHaveLength(3);
  });

  test('nós extras (a entidade de partida sem vínculos) entram sem duplicar', () => {
    const els = toElements([edge({ relationship_id: 1, src: person, dst: companyA })], [companyA, companyB]);
    expect(els.filter((e) => e.group === 'nodes').map((n) => n.data.id).sort()).toEqual(['n11', 'n12', 'n13']);
  });
});

describe('graphStylesheet', () => {
  afterEach(() => {
    document.documentElement.style.removeProperty('--fact');
  });

  test('a folha de estilo é aceita pelo Cytoscape sem avisos e distingue fato de inferência', () => {
    const warn = vi.spyOn(console, 'warn');
    const style = graphStylesheet(readPalette());
    const cy = cytoscape({
      headless: true,
      styleEnabled: true,
      style,
      elements: toElements([
        edge({ relationship_id: 1, kind: 'fact' }),
        edge({ relationship_id: 2, kind: 'inference', confidence: 0.5, src: person, dst: companyB }),
        edge({ relationship_id: 3, stale: true, closed: true, valid_to_before: '2026-01-01', src: companyB, dst: companyA }),
        edge({
          relationship_id: 4, kind: 'inference', confidence: 0.4, closed: true, valid_to_after: '2025-01-01',
          src: companyA, dst: companyB,
        }),
        edge({ relationship_id: 5, identity_basis: 'analyst', identity_confidence: 1, src: person, dst: companyA }),
      ]),
    });
    expect(warn).not.toHaveBeenCalled();
    expect(cy.$id('e1').style('line-style')).toBe('solid');
    expect(cy.$id('e2').style('line-style')).toBe('dashed');
    // closed: a bar at the end instead of the arrow, line style untouched
    expect(cy.$id('e1').style('target-arrow-shape')).toBe('triangle');
    expect(cy.$id('e3').style('target-arrow-shape')).toBe('tee');
    expect(cy.$id('e3').style('line-style')).toBe('solid');
    expect(cy.$id('e4').style('target-arrow-shape')).toBe('tee');
    expect(cy.$id('e4').style('line-style')).toBe('dashed');
    // stale: hollow square mid-edge
    expect(cy.$id('e3').style('mid-target-arrow-shape')).toBe('square');
    expect(cy.$id('e1').style('mid-target-arrow-shape')).toBe('none');
    // merged identity: a hollow circle at the start of the line, line style untouched
    expect(cy.$id('e5').style('source-arrow-shape')).toBe('circle');
    expect(cy.$id('e5').style('source-arrow-fill')).toBe('hollow');
    expect(cy.$id('e5').style('line-style')).toBe('solid');
    expect(cy.$id('e1').style('source-arrow-shape')).toBe('none');
    expect(cy.$id('n11').style('shape')).not.toBe(cy.$id('n12').style('shape'));
    cy.destroy();
  });

  test('cores vêm das variáveis CSS do tema, com reserva quando faltam', () => {
    expect(readPalette().fact).toMatch(/^#|rgb/);
    document.documentElement.style.setProperty('--fact', '#123456');
    expect(readPalette().fact).toBe('#123456');
  });
});
