import { describe, expect, test, vi } from 'vitest';
import { CY_STYLESHEET_ID, createGraph } from './graphAdapter';
import { toElements } from './graphStyle';
import { edge } from '../test/fixtures';
// The very file the bundler resolves for `import 'cytoscape'` (package.json "exports" → import).
import cytoscapeSource from 'cytoscape/dist/cytoscape.esm.mjs?raw';

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

  test('expandir não move os nós que já estavam na tela', () => {
    const g = createGraph(document.createElement('div'), { onSelect: vi.fn(), onActivate: vi.fn() }, { headless: true });
    g.setElements(
      toElements([
        edge({ relationship_id: 1, src: person, dst: companyA }),
        edge({ relationship_id: 3, src: { ...person, id: 14, name: 'OUTRA' }, dst: companyA }),
      ]),
    );
    const before = g.snapshot().positions;
    g.setElements(
      toElements([
        edge({ relationship_id: 1, src: person, dst: companyA }),
        edge({ relationship_id: 3, src: { ...person, id: 14, name: 'OUTRA' }, dst: companyA }),
        edge({ relationship_id: 2, src: person, dst: companyB }),
      ]),
    );
    const after = g.snapshot().positions;
    for (const id of ['n11', 'n12', 'n14']) expect(after[id]).toEqual(before[id]);
    expect(after.n13).toBeDefined();
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
