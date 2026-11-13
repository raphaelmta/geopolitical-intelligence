import { beforeEach, describe, expect, test, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ControlGroupPage } from './ControlGroupPage';
import { renderAt } from '../test/render';
import { json, mockFetch, type Call } from '../test/fetchMock';
import { companyEntity, edge, provenance } from '../test/fixtures';
import type { ControlResult, Job } from '../api/types';
import { lastGraph, resetGraphs } from '../test/graphMock';

vi.mock('../components/graphAdapter', () => import('../test/graphMock'));

const JOB1 = '0193b6c2-7f00-7000-8000-000000000001';
const JOB2 = '0193b6c2-7f00-7000-8000-000000000002';
const holding = { id: 20, kind: 'company', name: 'HOLDING FICTICIA SA', natural_key: '90000020' };
const root = { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' };
const sub = { id: 13, kind: 'company', name: 'SUBSIDIARIA FICTICIA LTDA', natural_key: '90000003' };
const owner = { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' };

const result: ControlResult = {
  root,
  controllers: [owner],
  members: [
    { entity: holding, depth: 1, via: [601] },
    { entity: root, depth: 2, via: [601, 602] },
    { entity: sub, depth: 3, via: [601, 602, 603] },
  ],
  edges: [
    edge({ relationship_id: 601, type: 'partner_of', src: owner, dst: holding, attrs: { role: '49' } }),
    edge({ relationship_id: 602, type: 'shareholder_of', src: holding, dst: root, attrs: { percent: '50.5' } }),
    edge({ relationship_id: 603, type: 'controls', src: root, dst: sub, attrs: {}, kind: 'inference', confidence: 0.75 }),
  ],
  stops: [],
};

function job(id: string, overrides: Partial<Job> = {}): Job {
  return {
    id,
    type: 'control_group',
    params: {},
    status: 'done',
    progress: 1,
    result,
    error: '',
    created_at: '2026-11-11T12:00:00Z',
    finished_at: '2026-11-11T12:00:02Z',
    ...overrides,
  };
}

function posts(calls: Call[]) {
  return calls
    .filter((c) => c.url.pathname === '/api/v1/jobs' && c.init?.method === 'POST')
    .map((c) => JSON.parse(String(c.init!.body)));
}

const at = { route: '/grupo/12', path: '/grupo/:id' };

function routes(first: ControlResult = result, second: ControlResult = result) {
  let n = 0;
  return {
    '/api/v1/entities/12': json(companyEntity()),
    '/api/v1/jobs': () => json({ id: ++n === 1 ? JOB1 : JOB2 }, 202),
    [`/api/v1/jobs/${JOB1}`]: json(job(JOB1, { result: first })),
    [`/api/v1/jobs/${JOB2}`]: json(job(JOB2, { result: second })),
  };
}

beforeEach(() => resetGraphs());

describe('ControlGroupPage', () => {
  test('mostra controladores finais, membros por nível com a cadeia e os vínculos com natureza', async () => {
    const { calls } = mockFetch(routes());
    renderAt(<ControlGroupPage />, at);
    expect(
      await screen.findByRole('heading', { level: 1, name: 'Grupo de controle: EMPRESA FICTICIA A LTDA' }),
    ).toBeTruthy();
    const controllers = await screen.findByRole('region', { name: 'Controladores finais' });
    expect(within(controllers).getByRole('link', { name: 'PESSOA FICTICIA UM' }).getAttribute('href')).toBe('/entidade/11');
    expect(posts(calls)).toEqual([{ type: 'control_group', params: { company_id: 12, mode: 'documented' } }]);

    const members = screen.getByRole('region', { name: 'Empresas do grupo' });
    expect(within(members).getByRole('heading', { name: /Nível 3/ })).toBeTruthy();
    const subItem = within(members).getByRole('link', { name: 'SUBSIDIARIA FICTICIA LTDA' }).closest('li')!;
    // the chain from the final controller down to the member
    expect(subItem.textContent).toContain('PESSOA FICTICIA UM');
    expect(subItem.textContent).toContain('HOLDING FICTICIA SA');
    expect(within(subItem).getByText('inferência · 75%')).toBeTruthy();

    const edges = screen.getByRole('table', { name: /Vínculos do grupo/ });
    expect(within(edges).getByText('acionista, participação 50,5%')).toBeTruthy();
    expect(within(edges).getAllByRole('button', { name: /fonte/ })).toHaveLength(3);
    expect((await lastGraph()).ids()).toEqual(['e601', 'e602', 'e603', 'n11', 'n12', 'n13', 'n20']);
    expect(screen.getByText(/segue apenas controle declarado, participação acima de 50 % e sócio único/)).toBeTruthy();
    expect(screen.queryByText('Vínculo societário não é prova de controle.')).toBeNull();
  });

  test('grupo: alterna entre "Controle documentado" e "Vínculos societários" e reenvia a tarefa', async () => {
    const { calls } = mockFetch(routes());
    renderAt(<ControlGroupPage />, at);
    await screen.findByRole('region', { name: 'Controladores finais' });
    const documented = screen.getByRole('radio', { name: 'Controle documentado' });
    expect((documented as HTMLInputElement).checked).toBe(true);
    await userEvent.click(screen.getByRole('radio', { name: 'Vínculos societários' }));
    expect(await screen.findByText('Vínculo societário não é prova de controle.')).toBeTruthy();
    expect(screen.getByText(/segue todo vínculo societário; mostra quem está ligado, não quem manda/)).toBeTruthy();
    await vi.waitFor(() => expect(posts(calls)).toHaveLength(2));
    expect(posts(calls)[1]).toEqual({ type: 'control_group', params: { company_id: 12, mode: 'corporate_links' } });
    await vi.waitFor(() => expect(calls.some((c) => c.url.pathname === `/api/v1/jobs/${JOB2}`)).toBe(true));
    await userEvent.click(documented);
    await vi.waitFor(() => expect(posts(calls)).toHaveLength(3));
    expect(posts(calls)[2]!.params.mode).toBe('documented');
  });

  test('cada elo da cadeia de um membro tem "fonte" (abre a proveniência dele) e "destacar"', async () => {
    const { calls } = mockFetch({
      ...routes(),
      '/api/v1/relationships/603/provenance': json(provenance({ relationship_id: 603 })),
    });
    renderAt(<ControlGroupPage />, at);
    const members = await screen.findByRole('region', { name: 'Empresas do grupo' });
    const subItem = within(members).getByRole('link', { name: 'SUBSIDIARIA FICTICIA LTDA' }).closest('li')!;
    const hops = within(subItem).getAllByRole('listitem');
    expect(hops).toHaveLength(3);
    for (const h of hops) {
      expect(within(h).getByRole('button', { name: /fonte/ })).toBeTruthy();
      expect(within(h).getByRole('button', { name: /Destacar no gráfico/ })).toBeTruthy();
    }
    await userEvent.click(within(hops[2]!).getByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('heading', { name: 'Fonte do vínculo nº 603' })).toBeTruthy();
    await within(dialog).findByText('Receita Federal — CNPJ');
    expect(calls.some((c) => c.url.pathname === '/api/v1/relationships/603/provenance')).toBe(true);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Fechar' }));
    await userEvent.click(within(hops[2]!).getByRole('button', { name: /Destacar no gráfico/ }));
    expect((await lastGraph()).handle.highlight).toHaveBeenLastCalledWith('e603');
  });

  test('lista e cadeia mostram a identidade dos vínculos pela base', async () => {
    const withBases: ControlResult = {
      ...result,
      edges: [
        edge({ relationship_id: 601, type: 'partner_of', src: owner, dst: holding, attrs: { role: '49' }, identity_basis: 'analyst', identity_confidence: 1 }),
        edge({ relationship_id: 602, type: 'shareholder_of', src: holding, dst: root, attrs: { percent: '50.5' }, identity_basis: 'auto', identity_confidence: 0.9 }),
        edge({ relationship_id: 603, type: 'controls', src: root, dst: sub, attrs: {}, identity_basis: 'unknown', identity_confidence: 1 }),
      ],
    };
    mockFetch(routes(withBases));
    renderAt(<ControlGroupPage />, at);
    const members = await screen.findByRole('region', { name: 'Empresas do grupo' });
    const subItem = within(members).getByRole('link', { name: 'SUBSIDIARIA FICTICIA LTDA' }).closest('li')!;
    const hops = within(subItem).getAllByRole('listitem');
    expect(within(hops[0]!).getByText('identidade: decisão de analista')).toBeTruthy();
    expect(within(hops[1]!).getByText('identidade 90%')).toBeTruthy();
    expect(within(hops[2]!).getByText('identidade: base não informada')).toBeTruthy();
    const edges = screen.getByRole('table', { name: /Vínculos do grupo/ });
    expect(within(edges).getByText('identidade: decisão de analista')).toBeTruthy();
    expect(within(edges).getByText('identidade 90%')).toBeTruthy();
    expect(within(edges).getByText('identidade: base não informada')).toBeTruthy();
    expect(edges.textContent).not.toContain('100%');
  });

  test('grupo: vínculo sem percentual mostra "sócio, participação não informada"', async () => {
    mockFetch(routes());
    renderAt(<ControlGroupPage />, at);
    const edges = await screen.findByRole('table', { name: /Vínculos do grupo/ });
    const row = within(edges).getByText('sócio, participação não informada · qualificação 49').closest('tr')!;
    expect(within(row).getByRole('link', { name: 'HOLDING FICTICIA SA' })).toBeTruthy();
    expect(within(row).getByText('fato')).toBeTruthy();
  });

  test('grupo: lista paradas com motivo em português', async () => {
    const fund = { id: 40, kind: 'company', name: 'FUNDO FICTICIO FIA', natural_key: '90000040' };
    const foreign = { id: 41, kind: 'foreign', name: 'FOREIGN HOLDING LTD', natural_key: '' };
    const crowd = { id: 42, kind: 'company', name: 'MUITOS SOCIOS SA', natural_key: '90000042' };
    mockFetch(
      routes({
        ...result,
        stops: [
          { entity: fund, reason: 'fund' },
          { entity: foreign, reason: 'foreign' },
          { entity: crowd, reason: 'too_many_partners' },
          { entity: { ...crowd, id: 43, name: 'NOVO MOTIVO SA' }, reason: 'something_new' },
        ],
      }),
    );
    renderAt(<ControlGroupPage />, at);
    const stops = await screen.findByRole('region', { name: 'Onde a busca parou' });
    const text = (name: string) => within(stops).getByRole('link', { name }).closest('li')!.textContent;
    expect(text('FUNDO FICTICIO FIA')).toContain('fundo de investimento');
    expect(text('FOREIGN HOLDING LTD')).toContain('sócio estrangeiro');
    expect(text('MUITOS SOCIOS SA')).toContain('muitos sócios');
    expect(text('NOVO MOTIVO SA')).toContain('outro motivo (something_new)');
  });

  test('grupo grande demais: aviso claro', async () => {
    mockFetch(
      routes({ root, controllers: [], members: [{ entity: root, depth: 0, via: [] }], edges: [], stops: [{ entity: root, reason: 'too_large' }] }),
    );
    renderAt(<ControlGroupPage />, at);
    const stops = await screen.findByRole('region', { name: 'Onde a busca parou' });
    expect(stops.textContent).toContain('grupo grande demais para exibir por inteiro');
    expect(screen.getByRole('region', { name: 'Controladores finais' }).textContent).toContain(
      'Nenhum controlador final identificado',
    );
  });

  test('tarefa que falha mostra o erro e tenta de novo com o mesmo modo', async () => {
    let n = 0;
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/jobs': () => json({ id: ++n === 1 ? JOB1 : JOB2 }, 202),
      [`/api/v1/jobs/${JOB1}`]: json(job(JOB1, { status: 'failed', result: null, error: 'entity not found' })),
      [`/api/v1/jobs/${JOB2}`]: json(job(JOB2)),
    });
    renderAt(<ControlGroupPage />, at);
    expect((await screen.findByRole('alert')).textContent).toMatch(/Entidade não encontrada/);
    await userEvent.click(screen.getByRole('button', { name: 'Tentar de novo' }));
    await screen.findByRole('region', { name: 'Controladores finais' });
    expect(posts(calls)[1]).toEqual(posts(calls)[0]);
  });
});
