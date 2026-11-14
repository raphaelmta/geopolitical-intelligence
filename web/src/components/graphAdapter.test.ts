import { describe, expect, test, vi } from 'vitest';
import { AUTO_LABEL_EDGES, CY_STYLESHEET_ID, createGraph, type GraphHandle } from './graphAdapter';
import { computeLayout } from './graphLayout';
import { toElements } from './graphStyle';
import { edge } from '../test/fixtures';
// The very file the bundler resolves for `import 'cytoscape'` (package.json "exports" → import).
import cytoscapeSource from 'cytoscape/dist/cytoscape.esm.mjs?raw';

// Keeps every Cytoscape instance the adapter creates, to drive taps and hovers
// the way a pointer would (jsdom has no canvas, so there is no real pointer).
const made = vi.hoisted(() => [] as import('cytoscape').Core[]);
vi.mock('cytoscape', async (importOriginal) => {
  const real = (await importOriginal<{ default: typeof import('cytoscape') }>()).default;
  const wrapped = Object.assign(
    (options?: import('cytoscape').CytoscapeOptions) => {
      const cy = real(options);
      made.push(cy);
      return cy;
    },
    real,
  );
  return { default: wrapped };
});
/** The Cytoscape instance behind the handle created last. */
const cyOf = (_g: GraphHandle) => made.at(-1)!;

const person = { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' };
const companyA = { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' };
const companyB = { id: 13, kind: 'company', name: 'EMPRESA FICTICIA B LTDA', natural_key: '90000003' };

// The real adapter, with Cytoscape in headless mode (no canvas in jsdom).
describe('graphAdapter (Cytoscape sem tela)', () => {
  test('acrescenta e remove por id, mantém posições, destaca e se desfaz', () => {
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true });
    const first = toElements([edge({ relationship_id: 1, src: person, dst: companyA })]);
    g.setElements(first);
    expect(g.snapshot().ids).toEqual(['e1', 'n11', 'n12']);
    g.highlight('e1');
    expect(g.snapshot().highlighted).toEqual(['e1']);
    g.setElements(
      toElements([
        edge({ relationship_id: 1, src: person, dst: companyA }),
        edge({ relationship_id: 2, src: person, dst: companyB, kind: 'inference', confidence: 0.5 }),
      ]),
    );
    expect(g.snapshot().ids).toEqual(['e1', 'e2', 'n11', 'n12', 'n13']);
    expect(g.snapshot().highlighted).toEqual(['e1']); // kept across an expansion
    g.setElements(first);
    expect(g.snapshot().ids).toEqual(['e1', 'n11', 'n12']);
    g.highlight('n404');
    expect(g.snapshot().highlighted).toEqual([]);
    g.highlight('n12');
    expect(g.snapshot().highlighted).toEqual(['n12']);
    expect(() => g.restyle()).not.toThrow();
    g.destroy();
  });

  test('expandir refaz as posições pelo layout, igual a um gráfico novo com os mesmos elementos', () => {
    const base = [
      edge({ relationship_id: 1, src: person, dst: companyA }),
      edge({ relationship_id: 3, src: { ...person, id: 14, name: 'OUTRA' }, dst: companyA }),
    ];
    const expanded = [...base, edge({ relationship_id: 2, src: person, dst: companyB })];
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true });
    g.setElements(toElements(base, [], 12));
    expect(g.snapshot().positions.n12).toEqual({ x: 0, y: 0 }); // the root is the origin
    g.setElements(toElements(expanded, [], 12));
    const after = g.snapshot().positions;
    expect(Object.keys(after).sort()).toEqual(['n11', 'n12', 'n13', 'n14']);
    // no random placement: a fresh graph with the same elements lands on the same points
    const h = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true });
    h.setElements(toElements(expanded, [], 12));
    expect(h.snapshot().positions).toEqual(after);
    const layout = computeLayout(
      ['n12', 'n11', 'n14', 'n13'],
      [['n11', 'n12'], ['n14', 'n12'], ['n11', 'n13']],
      'n12',
    );
    for (const id of Object.keys(layout)) {
      expect(after[id]!.x).toBeCloseTo(layout[id]!.x, 5);
      expect(after[id]!.y).toBeCloseTo(layout[id]!.y, 5);
    }
    g.destroy();
    h.destroy();
  });

  test('arrange devolve os nós ao lugar calculado depois de arrastados', () => {
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true });
    g.setElements(toElements([edge({ relationship_id: 1, src: person, dst: companyA })], [], 12));
    const placed = g.snapshot().positions;
    g.arrange();
    expect(g.snapshot().positions).toEqual(placed);
    expect(() => {
      g.zoom(1.25);
      g.zoom(0.8);
      g.fit();
      g.resize();
    }).not.toThrow();
    g.destroy();
  });

  test('layout "flow": cada profundidade numa coluna', () => {
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true, layout: 'flow' });
    g.setElements(
      toElements(
        [edge({ relationship_id: 1, src: person, dst: companyA }), edge({ relationship_id: 2, src: companyA, dst: companyB })],
        [],
        11,
      ),
    );
    const pos = g.snapshot().positions;
    expect(pos.n11!.x).toBe(0);
    expect(pos.n12!.x).toBeGreaterThan(pos.n11!.x);
    expect(pos.n13!.x).toBeGreaterThan(pos.n12!.x);
    g.destroy();
  });

  test('destacar pelo id de um vínculo marca a linha que o reúne', () => {
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true });
    g.setElements(
      toElements([
        edge({ relationship_id: 1, src: person, dst: companyA }),
        edge({ relationship_id: 2, type: 'administrator_of', src: person, dst: companyA }),
      ]),
    );
    expect(g.snapshot().ids).toEqual(['e1', 'n11', 'n12']); // e2 has no element of its own
    g.highlight('e2');
    expect(g.snapshot().highlighted).toEqual(['e1']);
    g.highlight('e1');
    expect(g.snapshot().highlighted).toEqual(['e1']);
    g.highlight(null);
    expect(g.snapshot().highlighted).toEqual([]);
    // the member id keeps working when the elements change underneath
    g.highlight('e2');
    g.setElements(
      toElements([
        edge({ relationship_id: 1, src: person, dst: companyA }),
        edge({ relationship_id: 2, type: 'administrator_of', src: person, dst: companyA }),
        edge({ relationship_id: 3, src: person, dst: companyB }),
      ]),
    );
    expect(g.snapshot().highlighted).toEqual(['e1']);
    g.destroy();
  });

  test('"auto" mostra as palavras em todas as linhas de um mapa pequeno; "all" também nos grandes', () => {
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true });
    const small = Array.from({ length: 3 }, (_, i) =>
      edge({ relationship_id: i + 1, src: person, dst: { ...companyA, id: 100 + i, name: `EMPRESA ${i}` } }),
    );
    g.setElements(toElements(small, [], 11));
    expect(g.snapshot().labelled).toEqual(['e1', 'e2', 'e3']);

    const many = Array.from({ length: AUTO_LABEL_EDGES + 1 }, (_, i) =>
      edge({ relationship_id: i + 1, src: person, dst: { ...companyA, id: 100 + i, name: `EMPRESA ${i}` } }),
    );
    g.setElements(toElements(many, [], 11));
    expect(g.snapshot().ids.filter((id) => id.startsWith('e'))).toHaveLength(AUTO_LABEL_EDGES + 1);
    expect(g.snapshot().labelled).toEqual([]);
    g.setEdgeLabels('all');
    expect(g.snapshot().labelled).toHaveLength(AUTO_LABEL_EDGES + 1);
    g.setEdgeLabels('auto');
    expect(g.snapshot().labelled).toEqual([]);
    // the selected node brings the words of the lines that touch it only
    g.setElements(
      toElements([...many, edge({ relationship_id: 900, src: companyB, dst: { ...companyA, id: 500, name: 'LONGE' } })], [], 11),
    );
    g.highlight('n500');
    expect(g.snapshot().labelled).toEqual(['e900']);
    g.destroy();
  });

  test('clicar no fundo chama onClear; clicar num nó chama onSelect com o id', () => {
    const onSelect = vi.fn();
    const onClear = vi.fn();
    const g = createGraph(document.createElement('div'), { onSelect, onActivate: vi.fn(), onClear }, { headless: true });
    g.setElements(toElements([edge({ relationship_id: 1, src: person, dst: companyA })]));
    cyOf(g).emit('tap');
    expect(onClear).toHaveBeenCalledTimes(1);
    cyOf(g).$id('n11').emit('tap');
    expect(onSelect).toHaveBeenCalledWith('n11');
    expect(onClear).toHaveBeenCalledTimes(1); // a tap on an element is not a tap on the background
    g.destroy();
  });

  test('passar o mouse num nó dá a dica e esmaece o que não é vizinho', () => {
    const onHover = vi.fn();
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn(), onHover }, { headless: true });
    g.setElements(
      toElements([
        edge({ relationship_id: 1, src: person, dst: companyA }),
        edge({ relationship_id: 2, src: companyB, dst: { ...companyA, id: 14, name: 'LONGE' } }),
      ]),
    );
    const cy = cyOf(g);
    cy.$id('n11').emit({ type: 'mouseover', renderedPosition: { x: 5, y: 6 } } as never);
    expect(onHover).toHaveBeenLastCalledWith({ text: 'PESSOA FICTICIA UM · Pessoa', x: 5, y: 6 });
    expect(cy.$id('n13').hasClass('faded')).toBe(true);
    expect(cy.$id('n12').hasClass('faded')).toBe(false);
    expect(cy.$id('e1').hasClass('near')).toBe(true);
    cy.$id('n11').emit('mouseout');
    expect(onHover).toHaveBeenLastCalledWith(null);
    expect(cy.elements('.faded').length).toBe(0);
    g.destroy();
  });

  test('não injeta <style> no documento (CSP style-src self)', () => {
    const before = document.head.querySelectorAll('style').length;
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true });
    expect(document.getElementById(CY_STYLESHEET_ID)?.tagName).toBe('META');
    expect(document.head.querySelectorAll('style').length).toBe(before);
    g.destroy();
  });
});

// The placeholder only works while Cytoscape looks for exactly this id
// before injecting its <style>: an upgrade that renames it must fail here.
describe('Cytoscape instalado', () => {
  test('o renderizador ainda procura o id do marcador antes de criar o <style>', () => {
    const src = cytoscapeSource;
    const decl = src.match(/var stylesheetId = '([^']+)';/);
    expect(decl?.[1]).toBe(CY_STYLESHEET_ID);
    expect(src).toContain('document.getElementById(stylesheetId) != null');
    expect(src).toMatch(/if \(!stylesheetAlreadyExists\) \{\s*var stylesheet = document\.createElement\('style'\)/);
  });
});
