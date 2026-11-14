import { afterEach, describe, expect, test, vi } from 'vitest';
import cytoscape from 'cytoscape';
import {
  KIND_ICONS,
  NODE_KINDS,
  bundleEdges,
  bundleLabel,
  edgeElementId,
  entitiesOf,
  entityIdOf,
  graphStylesheet,
  natureText,
  nodeElementId,
  readPalette,
  toElements,
} from './graphStyle';
import { edge } from '../test/fixtures';

const person = { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' };
const companyA = { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' };
const companyB = { id: 13, kind: 'company', name: 'EMPRESA FICTICIA B LTDA', natural_key: '90000003' };

function byId(els: ReturnType<typeof toElements>, id: string) {
  return els.find((e) => e.data.id === id);
}

describe('toElements', () => {
  test('toElements cria um nó por entidade e uma linha por par e direção, sem duplicar nós', () => {
    const els = toElements([
      edge({ relationship_id: 1, src: person, dst: companyA }),
      edge({ relationship_id: 2, src: person, dst: companyB }),
      edge({ relationship_id: 3, type: 'administrator_of', src: person, dst: companyA }),
      edge({ relationship_id: 1, src: person, dst: companyA }), // same relationship twice
    ]);
    const nodes = els.filter((e) => e.group === 'nodes');
    const edges = els.filter((e) => e.group === 'edges');
    expect(nodes.map((n) => n.data.id).sort()).toEqual(['n11', 'n12', 'n13']);
    // e3 is drawn inside the line of e1 (same pair, same direction)
    expect(edges.map((e) => e.data.id).sort()).toEqual(['e1', 'e2']);
    const e1 = byId(els, 'e1')!;
    expect(e1.data.source).toBe('n11');
    expect(e1.data.target).toBe('n12');
    expect(e1.data.members).toEqual(['e1', 'e3']);
    expect(byId(els, 'e2')!.data.members).toEqual(['e2']);
    expect(byId(els, 'n11')!.data.label).toBe('PESSOA FICTICIA UM');
    expect(byId(els, 'n11')!.classes).toContain('kind-person');
    expect(byId(els, 'n12')!.classes).toContain('kind-company');
    expect(nodeElementId(11)).toBe('n11');
    expect(edgeElementId(1)).toBe('e1');
    expect(entityIdOf('n11')).toBe(11);
    expect(entityIdOf('e1')).toBeNull();
  });

  test('nome longo é encurtado no rótulo do nó; a dica traz o nome inteiro e o tipo', () => {
    const long = { ...companyA, name: 'EMPRESA FICTICIA DE NOME MUITO COMPRIDO PARTICIPACOES E SERVICOS LTDA' };
    const els = toElements([edge({ relationship_id: 1, src: person, dst: long })]);
    const n = byId(els, 'n12')!;
    expect(n.data.label.length).toBeLessThanOrEqual(38);
    expect(n.data.label.endsWith('…')).toBe(true);
    expect(n.data.tip).toBe(`${long.name} · Empresa`);
    expect(byId(els, 'n11')!.data.tip).toBe('PESSOA FICTICIA UM · Pessoa');
  });

  test('a opção collapsed marca só as entidades pedidas; root marca a entidade de partida', () => {
    const els = toElements([edge({ relationship_id: 1, src: person, dst: companyA })], [], 12, { collapsed: new Set([11]) });
    expect(byId(els, 'n11')!.classes.split(' ')).toContain('collapsed');
    expect(byId(els, 'n12')!.classes.split(' ')).not.toContain('collapsed');
    expect(byId(els, 'n12')!.classes.split(' ')).toContain('root');
  });

  test('toElements marca linha de inferência com classe "inference" e de fato com "fact"', () => {
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
    // the line says the kind of link; the nature words are in the panel and the lists
    expect(fact.data.label).toBe('Sociedade');
    expect(inference.data.label).toBe('Sociedade');
    expect(natureText(edge({ kind: 'fact' }))).toBe('fato');
    expect(natureText(edge({ kind: 'inference', confidence: 0.8 }))).toBe('inferência · 80%');
  });

  test('vínculos paralelos na mesma direção viram uma só linha, com os membros e a classe "multiple"', () => {
    const els = toElements([
      edge({ relationship_id: 7, type: 'partner_of', src: person, dst: companyA }),
      edge({ relationship_id: 5, type: 'administrator_of', src: person, dst: companyA }),
    ]);
    const lines = els.filter((e) => e.group === 'edges');
    expect(lines).toHaveLength(1);
    expect(lines[0]!.data.id).toBe('e7'); // the first one given
    expect(lines[0]!.data.members).toEqual(['e7', 'e5']);
    expect(lines[0]!.classes.split(' ')).toContain('multiple');
    expect(lines[0]!.data.label).toBe('Sociedade · Administração');
    expect(lines[0]!.data.tip).toBe('Sociedade · Administração');
  });

  test('direções opostas entre o mesmo par são duas linhas', () => {
    const els = toElements([
      edge({ relationship_id: 1, src: person, dst: companyA }),
      edge({ relationship_id: 2, type: 'controls', src: companyA, dst: person }),
    ]);
    const lines = els.filter((e) => e.group === 'edges');
    expect(lines.map((l) => l.data.id)).toEqual(['e1', 'e2']);
    expect(lines[1]!.data.source).toBe('n12');
    expect(lines[1]!.classes.split(' ')).not.toContain('multiple');
  });

  test('linha mista de fato e inferência é "fact"; só é "inference" se todos os membros forem', () => {
    const mixed = toElements([
      edge({ relationship_id: 1, kind: 'fact' }),
      edge({ relationship_id: 2, kind: 'inference', confidence: 0.6, type: 'controls' }),
    ]);
    expect(byId(mixed, 'e1')!.classes.split(' ')).toContain('fact');
    expect(byId(mixed, 'e1')!.classes.split(' ')).not.toContain('inference');
    const all = toElements([
      edge({ relationship_id: 1, kind: 'inference', confidence: 0.6 }),
      edge({ relationship_id: 2, kind: 'inference', confidence: 0.9, type: 'controls' }),
    ]);
    expect(byId(all, 'e1')!.classes.split(' ')).toContain('inference');
    expect(byId(all, 'e1')!.classes.split(' ')).not.toContain('fact');
  });

  test('"closed" só se todos os membros encerraram; "stale" e "identity" se algum', () => {
    const some = toElements([
      edge({ relationship_id: 1, closed: true }),
      edge({ relationship_id: 2, type: 'controls', stale: true, identity_basis: 'auto', identity_confidence: 0.9 }),
    ]);
    const c = byId(some, 'e1')!.classes.split(' ');
    expect(c).not.toContain('closed');
    expect(c).toEqual(expect.arrayContaining(['stale', 'identity', 'multiple']));
    const all = toElements([
      edge({ relationship_id: 1, closed: true }),
      edge({ relationship_id: 2, type: 'controls', closed: true }),
    ]);
    expect(byId(all, 'e1')!.classes.split(' ')).toContain('closed');
  });

  test.each([
    [['partner_of'], 'Sociedade'],
    [['partner_of', 'partner_of'], 'Sociedade'], // same name twice is one name
    [['partner_of', 'controls'], 'Sociedade · Controle'],
    [['partner_of', 'controls', 'donated_to'], '3 vínculos'],
    [['partner_of', 'partner_of', 'controls', 'donated_to'], '4 vínculos'],
  ])('rótulo da linha para %j: "%s"', (types, label) => {
    expect(bundleLabel(types.map((type, i) => edge({ relationship_id: i + 1, type })))).toBe(label);
  });

  test('bundleEdges agrupa por par e direção, na ordem de chegada, sem repetir vínculo', () => {
    const a = edge({ relationship_id: 1, src: person, dst: companyA });
    const b = edge({ relationship_id: 2, src: companyA, dst: person });
    const c = edge({ relationship_id: 3, src: person, dst: companyA });
    const groups = bundleEdges([a, b, c, a]);
    expect(groups.map((g) => g.map((e) => e.relationship_id))).toEqual([[1, 3], [2]]);
  });

  test('entitiesOf reúne as pontas dos vínculos e as entidades extras, por id', () => {
    const m = entitiesOf([edge({ relationship_id: 1, src: person, dst: companyA })], [companyB]);
    expect([...m.keys()].sort()).toEqual([11, 12, 13]);
    expect(m.get(12)).toBe(companyA);
  });

  test('as palavras de natureza vão para natureText, não para o rótulo', () => {
    expect(natureText(edge({ closed: true, stale: true, identity_basis: 'analyst', identity_confidence: 1 }))).toBe(
      'fato · identidade: decisão de analista · vencido · encerrado',
    );
    const els = toElements([
      edge({ relationship_id: 1, closed: true, stale: true, identity_basis: 'auto', identity_confidence: 0.9 }),
    ]);
    const label = byId(els, 'e1')!.data.label;
    expect(label).toBe('Sociedade');
    expect(label).not.toMatch(/encerrado|vencido|identidade|fato/);
  });

  test('encerrado, vencido e identidade unida viram classes', () => {
    const els = toElements([
      edge({ relationship_id: 1, closed: true, valid_to_after: '2026-01-01', valid_to_before: '2026-02-01' }),
      edge({ relationship_id: 2, stale: true, identity_basis: 'auto', identity_confidence: 0.9, src: person, dst: companyB }),
    ]);
    const closed = byId(els, 'e1')!;
    const stale = byId(els, 'e2')!;
    expect(closed.classes!.split(' ')).toEqual(expect.arrayContaining(['fact', 'closed']));
    expect(stale.classes!.split(' ')).toEqual(expect.arrayContaining(['fact', 'stale', 'identity']));
    expect(stale.classes!.split(' ')).not.toContain('closed');
    const words = natureText(edge({ stale: true, identity_basis: 'auto', identity_confidence: 0.9 }));
    expect(words).toContain('vencido');
    expect(words).toContain('identidade 90%');
  });

  test('encerrado vem do campo closed da API, não das datas', () => {
    const els = toElements([
      edge({ relationship_id: 1, closed: false, valid_to_before: '2026-02-01' }),
      edge({ relationship_id: 2, closed: true, src: person, dst: companyB }),
    ]);
    expect(byId(els, 'e1')!.classes.split(' ')).not.toContain('closed');
    expect(natureText(edge({ closed: false, valid_to_before: '2026-02-01' }))).not.toContain('encerrado');
    expect(byId(els, 'e2')!.classes.split(' ')).toContain('closed');
  });

  test.each([
    ['auto', 0.99999, 'identidade 99%'],
    ['analyst', 1, 'identidade: decisão de analista'],
    ['unknown', 0.9, 'identidade: base não informada'],
  ] as const)('identidade %s (confiança %s): classe "identity" e texto "%s"', (basis, c, text) => {
    const e = edge({ relationship_id: 1, identity_basis: basis, identity_confidence: c });
    expect(byId(toElements([e]), 'e1')!.classes.split(' ')).toContain('identity');
    expect(natureText(e)).toContain(text);
  });

  test('identidade "none" não marca a linha, mesmo com confiança abaixo de 1', () => {
    const e = edge({ relationship_id: 1, identity_basis: 'none', identity_confidence: 0.9 });
    expect(byId(toElements([e]), 'e1')!.classes.split(' ')).not.toContain('identity');
    expect(natureText(e)).not.toContain('identidade');
  });

  test('laço (entidade ligada a si mesma) é uma linha própria; paralelos entram na mesma linha', () => {
    const els = toElements([
      edge({ relationship_id: 1, src: companyA, dst: companyA, type: 'shareholder_of' }),
      edge({ relationship_id: 2, src: person, dst: companyA }),
      edge({ relationship_id: 3, src: person, dst: companyA }),
    ]);
    expect(els.filter((e) => e.group === 'nodes')).toHaveLength(2);
    const loop = byId(els, 'e1')!;
    expect(loop.data.source).toBe('n12');
    expect(loop.data.target).toBe('n12');
    expect(byId(els, 'e3')).toBeUndefined();
    expect(byId(els, 'e2')!.data.members).toEqual(['e2', 'e3']);
    expect(els.filter((e) => e.group === 'edges')).toHaveLength(2);
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
        edge({ relationship_id: 5, identity_basis: 'analyst', identity_confidence: 1, src: companyB, dst: person }),
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
    // every kind has its own icon image
    expect(Object.keys(KIND_ICONS).sort()).toEqual([...NODE_KINDS].sort());
    expect(cy.$id('n11').style('background-image')).toContain('data:image/svg+xml');
    expect(cy.$id('n11').style('background-image')).not.toBe(cy.$id('n12').style('background-image'));
    cy.destroy();
  });

  test('rótulo da linha só aparece com a classe "labelled"; "faded" esmaece; "collapsed" troca a imagem', () => {
    const warn = vi.spyOn(console, 'warn');
    const cy = cytoscape({
      headless: true,
      styleEnabled: true,
      style: graphStylesheet(readPalette()),
      elements: toElements(
        [edge({ relationship_id: 1 }), edge({ relationship_id: 2, src: person, dst: companyB })],
        [],
        11,
        { collapsed: new Set([13]) },
      ),
    });
    const line = cy.$id('e1');
    expect(line.style('label')).toBe('');
    line.addClass('labelled');
    expect(line.style('label')).toBe('Sociedade');
    cy.$id('n11').addClass('faded');
    expect(Number(cy.$id('n11').style('opacity'))).toBeLessThan(0.5);
    // the collapsed company carries the "+" mark, the other company does not
    expect(cy.$id('n13').style('background-image')).not.toBe(cy.$id('n12').style('background-image'));
    expect(warn).not.toHaveBeenCalled();
    cy.destroy();
  });

  test('cores vêm das variáveis CSS do tema, com reserva quando faltam', () => {
    expect(readPalette().fact).toMatch(/^#|rgb/);
    document.documentElement.style.setProperty('--fact', '#123456');
    expect(readPalette().fact).toBe('#123456');
  });
});
