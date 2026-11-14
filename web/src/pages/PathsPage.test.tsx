import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { act, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { PathsPage } from './PathsPage';
import { renderAt } from '../test/render';
import { json, mockFetch, type Call } from '../test/fetchMock';
import { edge, personEntity } from '../test/fixtures';
import type { Job, PathsResult } from '../api/types';
import { lastGraph, layouts, resetGraphs } from '../test/graphMock';

vi.mock('../components/graphAdapter', () => import('../test/graphMock'));

const JOB = '0193b6c2-7f00-7000-8000-000000000001';
const person = { id: 11, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' };
const companyA = { id: 12, kind: 'company', name: 'EMPRESA FICTICIA A LTDA', natural_key: '90000002' };
const companyC = { id: 13, kind: 'company', name: 'EMPRESA FICTICIA C LTDA', natural_key: '90000003' };
const pt = { id: 30, kind: 'party', name: 'PT1', natural_key: '' };

function job(overrides: Partial<Job> = {}): Job {
  return {
    id: JOB,
    type: 'paths',
    params: { a: 11, b: 13 },
    status: 'running',
    progress: 0.4,
    result: null,
    error: '',
    created_at: '2026-11-11T12:00:00Z',
    finished_at: null,
    ...overrides,
  };
}

function done(result: PathsResult): Job {
  return job({ status: 'done', progress: 1, result });
}

const twoHops: PathsResult = {
  paths: [
    [
      { edge: edge({ relationship_id: 501, src: person, dst: companyA }), forward: true },
      // walked backwards: from A to C over "C is partner of A"
      { edge: edge({ relationship_id: 502, src: companyC, dst: companyA, attrs: { role: '49' } }), forward: false },
    ],
  ],
  stopped_hubs: [],
  truncated: false,
};

const routes = (jobReply: Parameters<typeof mockFetch>[0][string]) => ({
  '/api/v1/entities/11': json(personEntity()),
  '/api/v1/entities/13': json(personEntity({ entity: companyC })),
  '/api/v1/jobs': json({ id: JOB }, 202),
  [`/api/v1/jobs/${JOB}`]: jobReply,
});

function posts(calls: Call[]) {
  return calls
    .filter((c) => c.url.pathname === '/api/v1/jobs' && c.init?.method === 'POST')
    .map((c) => JSON.parse(String(c.init!.body)));
}

const route = { route: '/caminhos?a=11&b=13', path: '/caminhos' };

beforeEach(() => resetGraphs());
afterEach(() => vi.useRealTimers());

async function submit(user: { click: (el: Element) => Promise<void> } = userEvent) {
  // both pickers are prefilled from ?a=&b=
  await screen.findByText('EMPRESA FICTICIA C LTDA');
  await user.click(screen.getByRole('button', { name: 'Buscar caminhos' }));
}

describe('PathsPage', () => {
  test('caminhos: envia a tarefa, mostra progresso e lista cada elo com EdgeBadge', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    let n = 0;
    const { calls } = mockFetch(routes(() => json(++n === 1 ? job() : done(twoHops))));
    renderAt(<PathsPage />, route);
    expect(await screen.findByRole('heading', { level: 1, name: 'Caminhos entre duas entidades' })).toBeTruthy();
    await submit(user);
    expect(await screen.findByText(/em andamento 40%/)).toBeTruthy();
    expect(screen.getByRole('progressbar')).toBeTruthy();
    expect(posts(calls)).toEqual([{ type: 'paths', params: { a: 11, b: 13, max_depth: 6 } }]);

    await act(() => vi.advanceTimersByTimeAsync(2000));
    const path = await screen.findByRole('region', { name: /Caminho 1/ });
    expect(within(path).getByRole('heading', { name: /Caminho 1/ }).textContent).toContain(
      'PESSOA FICTICIA UM → EMPRESA FICTICIA A LTDA → EMPRESA FICTICIA C LTDA',
    );
    expect(path.textContent).toContain('2 elos — 2 fatos, 0 inferências');
    expect(path.textContent).not.toContain('contém inferência');
    const hops = within(path).getAllByRole('listitem');
    expect(hops).toHaveLength(2);
    // forward hop: the person is the partner
    expect(within(hops[0]!).getByText('é sócio de')).toBeTruthy();
    // backward hop: walking from A to C, A "has C as partner"
    expect(within(hops[1]!).getByText('tem como sócio')).toBeTruthy();
    expect(within(hops[1]!).getByRole('link', { name: 'EMPRESA FICTICIA C LTDA' }).getAttribute('href')).toBe(
      '/entidade/13',
    );
    expect(hops[1]!.textContent).toContain('qualificação 49');
    for (const h of hops) {
      expect(within(h).getByText('fato')).toBeTruthy();
      expect(within(h).getByRole('button', { name: /fonte/ })).toBeTruthy();
    }
    expect(screen.getByText('Tarefa: concluída', { exact: false })).toBeTruthy();
    expect((await lastGraph()).ids()).toEqual(['e501', 'e502', 'n11', 'n12', 'n13']);
    // paths read left to right, and the panel beside the drawing starts with the instructions
    expect(layouts.at(-1)).toBe('flow');
    expect(screen.getByRole('complementary', { name: 'Detalhes do que está selecionado' })).toBeTruthy();
  });

  test('caminhos: mostra "parou em PT1 (601 vínculos)" quando há stopped_hubs', async () => {
    mockFetch(
      routes(
        json(
          done({
            paths: [],
            stopped_hubs: [{ entity: pt, degree: 601 }],
            truncated: false,
          }),
        ),
      ),
    );
    renderAt(<PathsPage />, route);
    await submit();
    expect(await screen.findByText('parou em PT1 (601 vínculos)')).toBeTruthy();
    expect(screen.getByText(/muito conectadas não são atravessadas/)).toBeTruthy();
    expect(screen.getByText('Nenhum caminho encontrado até a profundidade 6.')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'página de PT1' }).getAttribute('href')).toBe('/entidade/30');
  });

  test('caminhos: resultado truncado avisa', async () => {
    mockFetch(routes(json(done({ ...twoHops, truncated: true }))));
    renderAt(<PathsPage />, route);
    await submit();
    expect(await screen.findByText('Resultado truncado: há mais caminhos do mesmo tamanho.')).toBeTruthy();
  });

  test('caminho com elo inferido ou identidade unida mostra "contém inferência ou identidade unida"', async () => {
    mockFetch(
      routes(
        json(
          done({
            paths: [
              [
                { edge: edge({ relationship_id: 501, src: person, dst: companyA, kind: 'inference', confidence: 0.6 }), forward: true },
                { edge: edge({ relationship_id: 502, src: companyC, dst: companyA }), forward: false },
              ],
              [
                {
                  edge: edge({ relationship_id: 503, src: person, dst: companyC, identity_basis: 'auto', identity_confidence: 0.8 }),
                  forward: true,
                },
              ],
              [
                {
                  edge: edge({ relationship_id: 504, src: person, dst: companyC, identity_basis: 'analyst', identity_confidence: 1 }),
                  forward: true,
                },
              ],
              [
                // a job result stored before identity_basis existed
                {
                  edge: edge({ relationship_id: 505, src: person, dst: companyC, identity_basis: 'unknown', identity_confidence: 1 }),
                  forward: true,
                },
              ],
              [
                {
                  edge: edge({ relationship_id: 506, src: person, dst: companyC, identity_basis: 'none', identity_confidence: 1 }),
                  forward: true,
                },
              ],
            ],
            stopped_hubs: null,
            truncated: false,
          }),
        ),
      ),
    );
    renderAt(<PathsPage />, route);
    await submit();
    const tag = 'contém inferência ou identidade unida';
    const first = await screen.findByRole('region', { name: /Caminho 1/ });
    expect(first.textContent).toContain('2 elos — 1 fato, 1 inferência');
    expect(within(first).getByText(tag)).toBeTruthy();
    expect(within(first).getByText('inferência · 60%')).toBeTruthy();
    const second = screen.getByRole('region', { name: /Caminho 2/ });
    expect(second.textContent).toContain('1 elo — 1 fato, 0 inferências, 1 com identidade unida');
    expect(within(second).getByText(tag)).toBeTruthy();
    expect(within(second).getByText('identidade 80%')).toBeTruthy();
    const third = screen.getByRole('region', { name: /Caminho 3/ });
    expect(within(third).getByText(tag)).toBeTruthy();
    expect(within(third).getByText('identidade: decisão de analista')).toBeTruthy();
    expect(third.textContent).not.toContain('100%');
    const fourth = screen.getByRole('region', { name: /Caminho 4/ });
    expect(within(fourth).getByText(tag)).toBeTruthy();
    expect(within(fourth).getByText('identidade: base não informada')).toBeTruthy();
    const fifth = screen.getByRole('region', { name: /Caminho 5/ });
    expect(fifth.textContent).not.toContain('identidade');
    expect(fifth.textContent).not.toContain('contém inferência');
    // no invented aggregate confidence for a path
    expect(first.querySelector('.path__summary')!.textContent).not.toMatch(/\d+%/);
  });

  test('elo de resultado antigo sem "closed": encerrado pelas datas; com "closed", pelo campo', async () => {
    mockFetch(
      routes(
        json(
          done({
            paths: [
              [
                { edge: edge({ relationship_id: 501, src: person, dst: companyA, closed: undefined, valid_to_before: '2026-02-01' }), forward: true },
                { edge: edge({ relationship_id: 502, src: companyC, dst: companyA, closed: true }), forward: false },
              ],
            ],
            stopped_hubs: null,
            truncated: false,
          }),
        ),
      ),
    );
    renderAt(<PathsPage />, route);
    await submit();
    const hops = within(await screen.findByRole('region', { name: /Caminho 1/ })).getAllByRole('listitem');
    expect(hops[0]!.textContent).toContain('saiu antes de 01/02/2026');
    expect(hops[1]!.textContent).toContain('encerrado (data desconhecida)');
  });

  test('caminhos: tarefa failed mostra o erro e botão de tentar de novo', async () => {
    let n = 0;
    const { calls } = mockFetch(
      routes(() => json(++n === 1 ? job({ status: 'failed', error: 'timeout' }) : done(twoHops))),
    );
    renderAt(<PathsPage />, route);
    await screen.findByText('EMPRESA FICTICIA C LTDA');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Profundidade máxima' }), '3');
    await userEvent.click(within(screen.getByRole('group', { name: 'Tipos de vínculo' })).getByRole('checkbox', { name: 'Doação eleitoral' }));
    await userEvent.click(screen.getByRole('button', { name: 'Buscar caminhos' }));
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/tempo limite/);
    expect(screen.getByText('Tarefa: falhou', { exact: false })).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: 'Tentar de novo' }));
    await screen.findByRole('region', { name: /Caminho 1/ });
    const sent = posts(calls);
    expect(sent).toHaveLength(2);
    expect(sent[1]).toEqual(sent[0]);
    expect(sent[0]).toEqual({ type: 'paths', params: { a: 11, b: 13, max_depth: 3, rel_types: ['donated_to'] } });
  });

  test('o preenchimento por ?a= não sobrescreve uma escolha feita enquanto ele carregava', async () => {
    let release: (r: Response) => void = () => {};
    const slow = new Promise<Response>((r) => (release = r));
    mockFetch({
      '/api/v1/entities/11': () => slow,
      '/api/v1/search': json({ items: [companyA] }),
    });
    renderAt(<PathsPage />, { route: '/caminhos?a=11', path: '/caminhos' });
    await userEvent.type(await screen.findByRole('combobox', { name: 'De' }), 'empresa');
    const list = await screen.findByRole('listbox', { name: 'Resultados para De' });
    await userEvent.click(within(list).getByRole('option'));
    expect(screen.getByText('EMPRESA FICTICIA A LTDA')).toBeTruthy();
    await act(async () => {
      release(json(personEntity()));
      await slow;
    });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByText('EMPRESA FICTICIA A LTDA')).toBeTruthy();
    expect(screen.queryByText('PESSOA FICTICIA UM')).toBeNull();
  });

  test('sem as duas entidades não envia', async () => {
    const { fn } = mockFetch({});
    renderAt(<PathsPage />, { route: '/caminhos', path: '/caminhos' });
    await userEvent.click(await screen.findByRole('button', { name: 'Buscar caminhos' }));
    expect(screen.getByText('Escolha as duas entidades.')).toBeTruthy();
    expect(fn).not.toHaveBeenCalled();
  });
});
