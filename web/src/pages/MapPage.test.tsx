import { beforeEach, describe, expect, test, vi } from 'vitest';
import { act, fireEvent, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MapPage } from './MapPage';
import { renderAt } from '../test/render';
import { json, mockFetch, type Call } from '../test/fetchMock';
import { companyEntity, edge } from '../test/fixtures';
import { lastGraph, resetGraphs } from '../test/graphMock';

vi.mock('../components/graphAdapter', () => import('../test/graphMock'));

const at = { route: '/mapa/12', path: '/mapa/:id' };
const person = { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' };
const companyA = { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' };
const companyB = { id: 13, kind: 'company', name: 'EMPRESA FICTICIA B LTDA', natural_key: '90000003' };

const rootEdges = [edge({ relationship_id: 501, src: person, dst: companyA })];
const personEdges = [
  edge({ relationship_id: 501, src: person, dst: companyA }),
  edge({ relationship_id: 502, src: person, dst: companyB, kind: 'inference', confidence: 0.7 }),
];

function relCalls(calls: Call[], id: number) {
  return calls.filter((c) => c.url.pathname === `/api/v1/entities/${id}/relationships`);
}

beforeEach(() => resetGraphs());

function entitiesTable() {
  return screen.getByRole('table', { name: /Entidades no mapa/ });
}
function edgesTable() {
  return screen.getByRole('table', { name: /Vínculos no mapa/ });
}

describe('MapPage', () => {
  test('desenha os vizinhos da entidade de partida e lista os mesmos vínculos', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: '' }),
    });
    renderAt(<MapPage />, at);
    expect(await screen.findByRole('heading', { level: 1, name: 'Mapa: EMPRESA FICTICIA A LTDA' })).toBeTruthy();
    const row = (await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' })).closest('tr')!;
    expect(within(row).getByText('fato')).toBeTruthy();
    expect(within(row).getByRole('button', { name: /fonte/ })).toBeTruthy();
    expect(within(row).getByText('é sócio de')).toBeTruthy();
    expect(within(row).getByText('sócio, participação não informada · qualificação 22')).toBeTruthy();
    const c = relCalls(calls, 12)[0]!.url.searchParams;
    expect(c.get('limit')).toBe('200');
    expect(c.get('state')).toBe('open');
    expect((await lastGraph()).ids()).toEqual(['e501', 'n11', 'n12']);
    // every node links to its entity page
    expect(within(entitiesTable()).getByRole('link', { name: 'PESSOA FICTICIA UM' }).getAttribute('href')).toBe(
      '/entidade/11',
    );
    expect(document.title).toBe('Mapa: EMPRESA FICTICIA A LTDA — Bancada');
  });

  test('lista e gráfico mostram a identidade de cada vínculo pela base, e encerrado pelo campo closed', async () => {
    const p2 = { ...person, id: 14, name: 'PESSOA DOIS' };
    const p3 = { ...person, id: 15, name: 'PESSOA TRES' };
    const p4 = { ...person, id: 16, name: 'PESSOA QUATRO' };
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({
        items: [
          edge({ relationship_id: 501, src: person, dst: companyA, identity_basis: 'auto', identity_confidence: 0.99999 }),
          edge({ relationship_id: 502, src: p2, dst: companyA, identity_basis: 'analyst', identity_confidence: 1, closed: true }),
          edge({ relationship_id: 503, src: p3, dst: companyA, identity_basis: 'unknown', identity_confidence: 1 }),
          edge({ relationship_id: 504, src: p4, dst: companyA, identity_basis: 'none', identity_confidence: 1 }),
        ],
        next_cursor: '',
      }),
    });
    renderAt(<MapPage />, at);
    const table = await screen.findByRole('table', { name: /Vínculos no mapa/ });
    const row = async (name: string) => (await within(table).findByRole('link', { name })).closest('tr')!;
    expect(within(await row('PESSOA FICTICIA UM')).getByText('identidade 99%')).toBeTruthy();
    const analyst = await row('PESSOA DOIS');
    expect(within(analyst).getByText('identidade: decisão de analista')).toBeTruthy();
    expect(analyst.textContent).toContain('encerrado (data desconhecida)');
    expect(within(await row('PESSOA TRES')).getByText('identidade: base não informada')).toBeTruthy();
    expect((await row('PESSOA QUATRO')).textContent).not.toContain('identidade');
    const g = await lastGraph();
    const els = g.handle.setElements.mock.calls.at(-1)![0];
    const byId = (id: string) => els.find((e) => e.data.id === id)!;
    expect(String(byId('e501').classes)).toContain('identity');
    expect(String(byId('e502').data.label)).toContain('identidade: decisão de analista');
    expect(String(byId('e502').classes)).toContain('closed');
    expect(String(byId('e504').classes)).not.toContain('identity');
  });

  test('expandir um nó busca vizinhos e acrescenta ao mapa sem apagar o que havia', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: '' }),
      '/api/v1/entities/11/relationships': json({ items: personEdges, next_cursor: '' }),
    });
    renderAt(<MapPage />, at);
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
    await userEvent.click(within(entitiesTable()).getByRole('button', { name: 'Expandir PESSOA FICTICIA UM' }));
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'EMPRESA FICTICIA B LTDA' });
    expect(within(edgesTable()).getAllByRole('row')).toHaveLength(3); // header + 501 + 502, 501 not repeated
    expect(within(edgesTable()).getByText('inferência · 70%')).toBeTruthy();
    expect((await lastGraph()).ids()).toEqual(['e501', 'e502', 'n11', 'n12', 'n13']);
    expect(relCalls(calls, 11)[0]!.url.searchParams.get('limit')).toBe('200');
    // the expanded node has no "Expandir" any more, and shows its count
    const personRow = within(entitiesTable()).getByRole('link', { name: 'PESSOA FICTICIA UM' }).closest('tr')!;
    expect(within(personRow).queryByRole('button', { name: /Expandir/ })).toBeNull();
    expect(personRow.textContent).toContain('2 vínculos carregados');
  });

  test('duplo clique (ou Enter) num nó do gráfico também expande', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: '' }),
      '/api/v1/entities/11/relationships': json({ items: personEdges, next_cursor: '' }),
    });
    renderAt(<MapPage />, at);
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
    const g0 = await lastGraph();
    act(() => g0.callbacks.onActivate('n11'));
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'EMPRESA FICTICIA B LTDA' });
    expect(relCalls(calls, 11)).toHaveLength(1);
  });

  test('filtro por tipo de vínculo e por data refaz a consulta com type e at', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: '' }),
      '/api/v1/entities/11/relationships': json({ items: personEdges, next_cursor: '' }),
    });
    renderAt(<MapPage />, at);
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
    await userEvent.click(within(entitiesTable()).getByRole('button', { name: 'Expandir PESSOA FICTICIA UM' }));
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'EMPRESA FICTICIA B LTDA' });

    const filters = screen.getByRole('group', { name: 'Tipos de vínculo' });
    await userEvent.click(within(filters).getByRole('checkbox', { name: 'Sociedade' }));
    await userEvent.click(within(filters).getByRole('checkbox', { name: 'Controle' }));
    fireEvent.change(screen.getByLabelText('Situação em'), { target: { value: '2024-01-01' } });
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Vínculos' }), 'all');
    const before = calls.length;
    await userEvent.click(screen.getByRole('button', { name: 'Aplicar filtros' }));
    await vi.waitFor(() => expect(calls.length).toBe(before + 2));

    // every expanded node (the start and the one expanded) is queried again
    for (const id of [12, 11]) {
      const p = relCalls(calls, id).at(-1)!.url.searchParams;
      expect(p.get('type')).toBe('partner_of,controls');
      expect(p.get('at')).toBe('2024-01-01');
      expect(p.get('state')).toBe('all');
    }
    expect(await screen.findByText(/situação em 01\/01\/2024/)).toBeTruthy();
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'EMPRESA FICTICIA B LTDA' });
  });

  test('nó com mais vizinhos: aviso e "carregar mais 200" segue o cursor', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: 'c1' }),
      '/api/v1/entities/12/relationships?cursor=c1': json({
        items: [edge({ relationship_id: 503, src: companyB, dst: companyA, type: 'shareholder_of', attrs: {} })],
        next_cursor: '',
      }),
    });
    renderAt(<MapPage />, at);
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
    const rootRow = within(entitiesTable()).getAllByRole('link', { name: 'EMPRESA FICTICIA A LTDA' })[0]!.closest('tr')!;
    expect(rootRow.textContent).toContain('1 vínculo carregado');
    await userEvent.click(within(rootRow).getByRole('button', { name: 'há mais vínculos — carregar mais 200' }));
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'EMPRESA FICTICIA B LTDA' });
    expect(relCalls(calls, 12).map((c) => c.url.searchParams.get('cursor'))).toEqual([null, 'c1']);
    expect(rootRow.textContent).toContain('2 vínculos carregados');
    expect(within(rootRow).queryByRole('button', { name: /carregar mais/ })).toBeNull();
  });

  test('limite de exibição: para de acrescentar, conta carregados e exibidos e explica os controles desativados', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({
        items: [
          edge({ relationship_id: 1, src: person, dst: companyA }),
          edge({ relationship_id: 2, src: companyB, dst: companyA }),
          edge({ relationship_id: 3, src: { ...person, id: 14, name: 'OUTRA' }, dst: companyA }),
        ],
        next_cursor: 'c1',
      }),
    });
    renderAt(<MapPage edgeLimit={2} />, at);
    const note = await screen.findByText('O mapa atingiu o limite de exibição; use filtros ou abra a entidade.');
    expect(within(edgesTable()).getAllByRole('row')).toHaveLength(3); // header + 2
    const rootRow = within(entitiesTable()).getAllByRole('link', { name: 'EMPRESA FICTICIA A LTDA' })[0]!.closest('tr')!;
    expect(rootRow.textContent).toContain('3 vínculos carregados, 2 exibidos');
    const more = within(rootRow).getByRole('button', { name: /carregar mais/ }) as HTMLButtonElement;
    expect(more.disabled).toBe(true);
    const expandBtn = within(entitiesTable()).getByRole('button', { name: 'Expandir PESSOA FICTICIA UM' }) as HTMLButtonElement;
    expect(expandBtn.disabled).toBe(true);
    expect(document.getElementById(expandBtn.getAttribute('aria-describedby')!)!.contains(note)).toBe(true);
    expect(rootRow.textContent).toContain('limite do mapa atingido');
  });

  test('exatamente no limite, com "carregar mais" pendente, o aviso já aparece', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: 'c1' }),
    });
    renderAt(<MapPage edgeLimit={1} />, at);
    expect(await screen.findByText('O mapa atingiu o limite de exibição; use filtros ou abra a entidade.')).toBeTruthy();
    const rootRow = within(entitiesTable()).getAllByRole('link', { name: 'EMPRESA FICTICIA A LTDA' })[0]!.closest('tr')!;
    expect(rootRow.textContent).toContain('1 vínculo carregado');
    expect(rootRow.textContent).not.toContain('exibido');
    expect((within(rootRow).getByRole('button', { name: /carregar mais/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  test('resposta do filtro antigo que chega depois da do filtro novo é descartada', async () => {
    let releaseOld: (r: Response) => void = () => {};
    const old = new Promise<Response>((r) => (releaseOld = r));
    const oldEdge = edge({ relationship_id: 900, src: { ...person, id: 90, name: 'VINCULO ANTIGO' }, dst: companyA });
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': () => old, // first (unfiltered) answer hangs
      '/api/v1/entities/12/relationships?type=controls': json({ items: rootEdges, next_cursor: '' }),
    });
    renderAt(<MapPage />, at);
    await screen.findByRole('heading', { level: 1 });
    await userEvent.click(within(screen.getByRole('group', { name: 'Tipos de vínculo' })).getByRole('checkbox', { name: 'Controle' }));
    await userEvent.click(screen.getByRole('button', { name: 'Aplicar filtros' }));
    await within(edgesTable()).findByRole('link', { name: 'PESSOA FICTICIA UM' });
    await act(async () => {
      releaseOld(json({ items: [oldEdge], next_cursor: '' }));
      await old;
    });
    await vi.waitFor(() => expect(relCalls(calls, 12)).toHaveLength(2));
    expect(screen.queryByText('VINCULO ANTIGO')).toBeNull();
    expect(within(edgesTable()).getAllByRole('row')).toHaveLength(2);
    expect((await lastGraph()).ids()).toEqual(['e501', 'n11', 'n12']);
  });

  test('falha ao expandir um nó mantém o mapa e mostra o erro só desse nó', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: '' }),
      '/api/v1/entities/11/relationships': json({ error: 'internal' }, 500),
    });
    renderAt(<MapPage />, at);
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
    await userEvent.click(within(entitiesTable()).getByRole('button', { name: 'Expandir PESSOA FICTICIA UM' }));
    const alerts = await screen.findAllByRole('alert');
    expect(alerts).toHaveLength(1);
    expect(alerts[0]!.closest('div')!.parentElement!.textContent).toContain('Vínculos de PESSOA FICTICIA UM:');
    expect(within(edgesTable()).getAllByRole('row')).toHaveLength(2);
    expect((await lastGraph()).ids()).toEqual(['e501', 'n11', 'n12']);
    expect(within(alerts[0]!).getByRole('button', { name: 'Tentar novamente' })).toBeTruthy();
  });

  test('selecionar no gráfico rola a linha para a vista sem mover o foco', async () => {
    const scroll = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: scroll });
    try {
      mockFetch({
        '/api/v1/entities/12': json(companyEntity()),
        '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: '' }),
      });
      renderAt(<MapPage />, at);
      await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
      const focused = document.activeElement;
      const g0 = await lastGraph();
      act(() => g0.callbacks.onSelect('e501'));
      const row = within(edgesTable()).getByRole('button', { name: 'Destacar no gráfico o vínculo nº 501' }).closest('tr')!;
      expect(scroll).toHaveBeenCalledWith({ block: 'nearest' });
      expect(scroll.mock.contexts).toContain(row);
      expect(document.activeElement).toBe(focused);
    } finally {
      delete (HTMLElement.prototype as Partial<HTMLElement>).scrollIntoView;
    }
  });

  test('selecionar uma linha destaca no gráfico, e selecionar no gráfico marca a linha', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: '' }),
    });
    renderAt(<MapPage />, at);
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
    const g = await lastGraph();
    await userEvent.click(within(edgesTable()).getByRole('button', { name: 'Destacar no gráfico o vínculo nº 501' }));
    expect(g.handle.highlight).toHaveBeenLastCalledWith('e501');
    const btn = within(edgesTable()).getByRole('button', { name: 'Destacar no gráfico o vínculo nº 501' });
    expect(btn.getAttribute('aria-pressed')).toBe('true');

    act(() => g.callbacks.onSelect('n11'));
    const personRow = within(entitiesTable()).getByRole('link', { name: 'PESSOA FICTICIA UM' }).closest('tr')!;
    expect(personRow.className).toContain('is-selected');
    expect(btn.getAttribute('aria-pressed')).toBe('false');
  });

  test('a lista acessível existe mesmo sem o gráfico, e a legenda explica fato e inferência', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: rootEdges, next_cursor: '' }),
    });
    renderAt(<MapPage />, at);
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
    await userEvent.click(screen.getByRole('button', { name: 'Ocultar o gráfico' }));
    expect(edgesTable()).toBeTruthy();
    expect(screen.getByRole('complementary', { name: 'Legenda' }).textContent).toContain('inferência');
    expect(screen.getByRole('list', { name: 'Tipos de entidade' }).textContent).toContain('Empresa');
  });

  test('erro ao carregar vizinhos mostra a mensagem e tenta de novo', async () => {
    let n = 0;
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': () =>
        ++n === 1 ? json({ error: 'internal' }, 500) : json({ items: rootEdges, next_cursor: '' }),
    });
    renderAt(<MapPage />, at);
    await userEvent.click(await screen.findByRole('button', { name: 'Tentar novamente' }));
    await within(await screen.findByRole('table', { name: /Vínculos no mapa/ })).findByRole('link', { name: 'PESSOA FICTICIA UM' });
  });

  test('id que não é número não chama a API', async () => {
    const { fn } = mockFetch({});
    renderAt(<MapPage />, { route: '/mapa/abc', path: '/mapa/:id' });
    expect(await screen.findByRole('heading', { name: 'Entidade não encontrada' })).toBeTruthy();
    expect(fn).not.toHaveBeenCalled();
  });
});
