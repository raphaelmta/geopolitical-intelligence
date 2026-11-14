import { act } from 'react';
import { beforeEach, describe, expect, test, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { EntityPage } from './EntityPage';
import { renderAt } from '../test/render';
import { json, mockFetch } from '../test/fetchMock';
import { companyEntity, edge, personEntity, provenance } from '../test/fixtures';
import { graphs, lastGraph, resetGraphs } from '../test/graphMock';
import type { EntityResponse, PersonRecord } from '../api/types';

vi.mock('../components/graphAdapter', () => import('../test/graphMock'));

beforeEach(() => resetGraphs());

const at = { route: '/entidade/12', path: '/entidade/:id' };

describe('EntityPage', () => {
  test('página da entidade separa vínculos atuais de encerrados', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships?state=open': json({
        items: [edge({ relationship_id: 1, src: { id: 21, kind: 'person', name: 'SOCIA ATUAL', natural_key: '' } })],
        next_cursor: '',
      }),
      '/api/v1/entities/12/relationships?state=closed': json({
        items: [
          edge({
            relationship_id: 2,
            src: { id: 22, kind: 'person', name: 'SOCIO ANTIGO', natural_key: '' },
            closed: true,
            valid_to_after: '2026-10-01',
            valid_to_before: '2026-11-01',
          }),
        ],
        next_cursor: '',
      }),
    });
    renderAt(<EntityPage />, at);

    expect(await screen.findByRole('heading', { level: 1, name: 'EMPRESA FICTICIA A LTDA' })).toBeTruthy();
    expect(screen.getByText(/90\.000\.002/)).toBeTruthy();

    const current = screen.getByRole('region', { name: 'Vínculos atuais' });
    const closed = screen.getByRole('region', { name: 'Vínculos encerrados' });
    await within(current).findByRole('link', { name: 'SOCIA ATUAL' });
    await within(closed).findByRole('link', { name: 'SOCIO ANTIGO' });
    expect(within(current).queryByText('SOCIO ANTIGO')).toBeNull();
    expect(within(closed).queryByText('SOCIA ATUAL')).toBeNull();
    expect(within(closed).getByText('saiu entre 01/10/2026 e 01/11/2026')).toBeTruthy();
    expect(within(closed).getByText('A data de encerramento é o primeiro dia em que o vínculo já não valia.')).toBeTruthy();
    expect(document.title).toBe('EMPRESA FICTICIA A LTDA — Bancada');
    expect(document.activeElement).toBe(screen.getByRole('heading', { level: 1 }));
    expect(within(current).getByText('tem como sócio')).toBeTruthy();
    expect(within(current).getByRole('link', { name: 'SOCIA ATUAL' }).getAttribute('href')).toBe('/entidade/21');
    // every link shows fact or inference
    expect(within(current).getByText('fato')).toBeTruthy();
    expect(within(closed).getByText('fato')).toBeTruthy();
  });

  test('cada vínculo mostra natureza, identidade, vencido e atributos', async () => {
    mockFetch({
      '/api/v1/entities/11': json(personEntity()),
      '/api/v1/entities/11/relationships?state=open': json({
        items: [
          edge({
            relationship_id: 3,
            type: 'controls',
            kind: 'inference',
            confidence: 0.8,
            identity_basis: 'auto',
            identity_confidence: 0.95,
            stale: true,
            attrs: { role: 'controller', percent: '51.5', reference_date: '2026-01-31' },
          }),
        ],
        next_cursor: '',
      }),
      '/api/v1/entities/11/relationships?state=closed': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, { route: '/entidade/11', path: '/entidade/:id' });
    const current = await screen.findByRole('region', { name: 'Vínculos atuais' });
    const row = (await within(current).findByRole('link', { name: 'EMPRESA FICTICIA A LTDA' })).closest('tr')!;
    expect(within(row).getByText('controla')).toBeTruthy();
    expect(within(row).getByText('inferência · 80%')).toBeTruthy();
    expect(within(row).getByText('identidade 95%')).toBeTruthy();
    expect(within(row).getByText('vencido')).toBeTruthy();
    expect(row.textContent).toContain('51,5%');
    expect(row.textContent).toContain('referência 31/01/2026');
    expect(row.textContent).not.toContain('controller');
    expect(row.className).toContain('edge-row--inference');
    expect(within(closed()).getByText(/Nenhum vínculo encerrado/)).toBeTruthy();
  });

  test('identidade de cada vínculo pela base: automática, de analista, não informada, nenhuma', async () => {
    const other = (id: number, name: string) => ({ id, kind: 'company', name, natural_key: String(90000000 + id) });
    mockFetch({
      '/api/v1/entities/11': json(personEntity()),
      '/api/v1/entities/11/relationships?state=open': json({
        items: [
          edge({ relationship_id: 1, dst: other(1, 'EMPRESA AUTO'), identity_basis: 'auto', identity_confidence: 0.99999 }),
          edge({ relationship_id: 2, dst: other(2, 'EMPRESA ANALISTA'), identity_basis: 'analyst', identity_confidence: 1 }),
          edge({ relationship_id: 3, dst: other(3, 'EMPRESA ANTIGA'), identity_basis: 'unknown', identity_confidence: 1 }),
          edge({ relationship_id: 4, dst: other(4, 'EMPRESA UNICA'), identity_basis: 'none', identity_confidence: 1 }),
        ],
        next_cursor: '',
      }),
      '/api/v1/entities/11/relationships?state=closed': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, { route: '/entidade/11', path: '/entidade/:id' });
    const current = await screen.findByRole('region', { name: 'Vínculos atuais' });
    const row = async (name: string) => (await within(current).findByRole('link', { name })).closest('tr')!;
    expect(within(await row('EMPRESA AUTO')).getByText('identidade 99%')).toBeTruthy();
    const analyst = await row('EMPRESA ANALISTA');
    expect(within(analyst).getByText('identidade: decisão de analista')).toBeTruthy();
    expect(within(analyst).getByText('fato')).toBeTruthy();
    expect(analyst.textContent).not.toContain('%');
    expect(within(await row('EMPRESA ANTIGA')).getByText('identidade: base não informada')).toBeTruthy();
    expect((await row('EMPRESA UNICA')).textContent).not.toContain('identidade');
  });

  test('encerrado vem do campo closed de cada vínculo', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships?state=open': json({
        items: [
          edge({ relationship_id: 1, src: { id: 21, kind: 'person', name: 'FECHADO SEM DATA', natural_key: '' }, closed: true }),
          edge({
            relationship_id: 2,
            src: { id: 22, kind: 'person', name: 'ABERTO', natural_key: '' },
            closed: false,
            valid_to_after: '2026-01-01',
          }),
        ],
        next_cursor: '',
      }),
      '/api/v1/entities/12/relationships?state=closed': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, at);
    const current = await screen.findByRole('region', { name: 'Vínculos atuais' });
    const shut = (await within(current).findByRole('link', { name: 'FECHADO SEM DATA' })).closest('tr')!;
    expect(shut.textContent).toContain('encerrado (data desconhecida)');
    const open = within(current).getByRole('link', { name: 'ABERTO' }).closest('tr')!;
    expect(open.textContent).toContain('desde 01/01/2020');
    expect(open.textContent).not.toMatch(/saiu|encerrado/);
  });

  test('registros da pessoa: nome da base e de onde cada um foi lido', async () => {
    mockFetch({
      '/api/v1/entities/11': json(
        personEntity({
          records: [
            rec(31, 11, {
              record_ref: 'K3241.K03200Y0.D61010.SOCIOCSV:42',
              first_collection_id: 7,
              first_seen_reference_date: '2026-10-01',
            }),
            rec(35, 11, {
              source_key: 'tse',
              context_key: 'donor:2024:250000000001',
              record_ref: 'receitas_candidatos_2024_BRASIL.csv:1021',
              first_collection_id: 9,
              first_seen_reference_date: null,
            }),
            rec(36, 11, { source_key: 'fonte-nova', context_key: 'x:1' }),
          ],
        }),
      ),
      '/api/v1/entities/11/relationships': json(NO_RELS),
    });
    renderAt(<EntityPage />, person11);
    const table = await screen.findByRole('table', { name: /3 registros/ });
    expect(within(table).getByRole('columnheader', { name: 'Registro' })).toBeTruthy();
    const receita = within(table).getByText('60000001').closest('tr')!;
    expect(within(receita).getByText('Receita Federal — CNPJ')).toBeTruthy();
    expect(within(receita).getByText('K3241.K03200Y0.D61010.SOCIOCSV:42')).toBeTruthy();
    expect(receita.textContent).toContain('coleta nº 7, referência 01/10/2026');
    const tse = within(table).getByText('donor:2024:250000000001').closest('tr')!;
    expect(within(tse).getByText('TSE — candidaturas e contas')).toBeTruthy();
    expect(tse.textContent).toContain('coleta nº 9');
    expect(tse.textContent).not.toContain('referência');
    // an unknown key, and an older server without the provenance fields
    const other = within(table).getByText('x:1').closest('tr')!;
    expect(within(other).getByText('fonte-nova')).toBeTruthy();
    expect(other.textContent).toContain('não informado');
  });

  test('clicar na fonte de um vínculo abre a proveniência dele', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships?state=open': json({ items: [edge({ relationship_id: 77 })], next_cursor: '' }),
      '/api/v1/entities/12/relationships?state=closed': json({ items: [], next_cursor: '' }),
      '/api/v1/relationships/77/provenance': json(provenance({ relationship_id: 77 })),
    });
    renderAt(<EntityPage />, at);
    const current = await screen.findByRole('region', { name: 'Vínculos atuais' });
    await userEvent.click(await within(current).findByRole('button', { name: /fonte/ }));
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText('Receita Federal — CNPJ')).toBeTruthy();
    expect(calls.some((c) => c.url.pathname === '/api/v1/relationships/77/provenance')).toBe(true);
  });

  test('carregar mais segue o next_cursor e para no cursor vazio', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships?state=open': json({
        items: [edge({ relationship_id: 1, src: { id: 21, kind: 'person', name: 'PRIMEIRA', natural_key: '' } })],
        next_cursor: 'MTIz',
      }),
      '/api/v1/entities/12/relationships?state=open&cursor=MTIz': json({
        items: [edge({ relationship_id: 2, src: { id: 22, kind: 'person', name: 'SEGUNDA', natural_key: '' } })],
        next_cursor: '',
      }),
      '/api/v1/entities/12/relationships?state=closed': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, at);
    const current = await screen.findByRole('region', { name: 'Vínculos atuais' });
    await within(current).findByRole('link', { name: 'PRIMEIRA' });
    await userEvent.click(within(current).getByRole('button', { name: 'Carregar mais' }));
    await within(current).findByRole('link', { name: 'SEGUNDA' });
    expect(within(current).getByRole('link', { name: 'PRIMEIRA' })).toBeTruthy();
    expect(within(current).queryByRole('button', { name: 'Carregar mais' })).toBeNull();
    const open = calls.filter((c) => c.url.searchParams.get('state') === 'open');
    expect(open.map((c) => c.url.searchParams.get('cursor'))).toEqual([null, 'MTIz']);
  });

  test('carregar mais não repete um vínculo que já veio na página anterior', async () => {
    const first = edge({ relationship_id: 1, src: { id: 21, kind: 'person', name: 'PRIMEIRA', natural_key: '' } });
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships?state=open': json({ items: [first], next_cursor: 'c1' }),
      '/api/v1/entities/12/relationships?state=open&cursor=c1': json({
        items: [first, edge({ relationship_id: 2, src: { id: 22, kind: 'person', name: 'SEGUNDA', natural_key: '' } })],
        next_cursor: '',
      }),
      '/api/v1/entities/12/relationships?state=closed': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, at);
    const current = await screen.findByRole('region', { name: 'Vínculos atuais' });
    await within(current).findByRole('link', { name: 'PRIMEIRA' });
    await userEvent.click(within(current).getByRole('button', { name: 'Carregar mais' }));
    await within(current).findByRole('link', { name: 'SEGUNDA' });
    expect(within(current).getAllByRole('link', { name: 'PRIMEIRA' })).toHaveLength(1);
    expect(within(current).getAllByRole('row')).toHaveLength(3); // header + 2
  });

  test('filtro de tipo de vínculo vai para a API', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, at);
    await userEvent.selectOptions(await screen.findByRole('combobox', { name: 'Tipo de vínculo' }), 'controls');
    await screen.findAllByText(/Nenhum vínculo/);
    const typed = calls.filter((c) => c.url.searchParams.get('type') === 'controls');
    expect(typed.map((c) => c.url.searchParams.get('state')).sort()).toEqual(['closed', 'open']);
  });

  test('página da entidade lista "possível mesma pessoa" sem juntar', async () => {
    mockFetch({
      '/api/v1/entities/11': json(
        personEntity({
          possible_same: [
            { entity_id: 40, name: 'PESSOA FICTICIA UM', records: 3, shared_fragment: true },
            { entity_id: 41, name: 'PESSOA FICTICIA UM', records: 1, shared_fragment: false },
          ],
        }),
      ),
      '/api/v1/entities/11/relationships': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, { route: '/entidade/11', path: '/entidade/:id' });
    const section = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    expect(section.textContent).toContain('não foram unidas automaticamente');
    const links = within(section).getAllByRole('link');
    expect(links.map((l) => l.getAttribute('href'))).toEqual(['/entidade/40', '/entidade/41']);
    expect(section.textContent).toContain('3 registros');
    expect(section.textContent).toContain('mesmo trecho do CPF');
    expect(section.textContent).not.toContain('pode haver mais');
  });

  test('"possível mesma pessoa": 20 candidatos avisam que pode haver mais; lista vazia', async () => {
    const many = Array.from({ length: 20 }, (_, i) => ({
      entity_id: 100 + i, name: 'PESSOA FICTICIA UM', records: 1, shared_fragment: false,
    }));
    mockFetch({
      '/api/v1/entities/11': json(personEntity({ possible_same: many })),
      '/api/v1/entities/13': json(personEntity({ entity: { id: 13, kind: 'person', name: 'OUTRA', natural_key: '' } })),
      '/api/v1/entities/11/relationships': json({ items: [], next_cursor: '' }),
      '/api/v1/entities/13/relationships': json({ items: [], next_cursor: '' }),
    });
    const { unmount } = renderAt(<EntityPage />, { route: '/entidade/11', path: '/entidade/:id' });
    const section = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    expect(section.textContent).toContain('Mostrando as 20 primeiras; pode haver mais.');
    unmount();
    renderAt(<EntityPage />, { route: '/entidade/13', path: '/entidade/:id' });
    const empty = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    expect(empty.textContent).toContain('Nenhuma outra ficha com este nome normalizado foi encontrada');
  });

  test('versões cadastrais: atual primeiro, encerradas com o intervalo', async () => {
    mockFetch({
      '/api/v1/entities/12': json(
        companyEntity({
          versions: [
            {
              id: 4, name: 'EMPRESA NOVA LTDA', legal_nature: '2062', size: '', uf: 'SP', city: '7107', cnae: '',
              status: '02', capital: '1000.00', open: true, first_collection_id: 9, last_seen_collection_id: 9,
              closed_collection_id: null, record_ref: 'EMPRECSV:15', recorded_at: '2026-11-01T10:00:00Z',
            },
            {
              id: 3, name: 'EMPRESA VELHA LTDA', legal_nature: '2062', size: '', uf: 'SP', city: '7107', cnae: '',
              status: '02', capital: null, open: false, first_collection_id: 7, last_seen_collection_id: 8,
              closed_collection_id: 9, record_ref: 'EMPRECSV:14', recorded_at: '2026-10-01T10:00:00Z',
              first_seen_reference_date: '2026-08-01', last_seen_reference_date: '2026-09-01',
              closed_reference_date: '2026-10-01',
            },
          ],
        }),
      ),
      '/api/v1/entities/12/relationships': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, at);
    const list = await screen.findByRole('list', { name: 'Versões cadastrais' });
    const items = list.querySelectorAll('li');
    expect(items[0]!.textContent).toContain('EMPRESA NOVA LTDA');
    expect(items[0]!.textContent).toContain('R$ 1.000,00');
    expect(items[1]!.textContent).toContain('EMPRESA VELHA LTDA');
    expect(items[1]!.textContent).toMatch(/coleta 7.*coleta 8/);
    expect(items[1]!.textContent).toContain('encerrada na coleta 9');
    expect(items[1]!.textContent).toContain('vista de 01/08/2026 a 01/09/2026 · encerrada em 01/10/2026');
    expect(items[0]!.textContent).not.toContain('vista de');
  });

  test('entidade inexistente mostra "não encontrada"', async () => {
    mockFetch({
      '/api/v1/entities/12': json({ error: 'not_found' }, 404),
      '/api/v1/entities/12/relationships': json({ error: 'not_found' }, 404),
    });
    renderAt(<EntityPage />, at);
    expect(await screen.findByRole('heading', { name: 'Entidade não encontrada' })).toBeTruthy();
  });

  test('erro que não é 404: título e foco na página, com tentar de novo', async () => {
    mockFetch({
      '/api/v1/entities/12': json({ error: 'internal' }, 500),
      '/api/v1/entities/12/relationships': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, at);
    const h1 = await screen.findByRole('heading', { level: 1, name: 'Entidade nº 12' });
    await vi.waitFor(() => expect(document.activeElement).toBe(h1));
    expect(document.title).toBe('Entidade nº 12 — Bancada');
    expect(screen.getByRole('button', { name: 'Tentar novamente' })).toBeTruthy();
  });

  test('atalhos: ver no mapa, caminhos a partir daqui e, para empresa, grupo de controle', async () => {
    mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/11': json(personEntity()),
      '/api/v1/entities/12/relationships': json({ items: [], next_cursor: '' }),
      '/api/v1/entities/11/relationships': json({ items: [], next_cursor: '' }),
    });
    const { unmount } = renderAt(<EntityPage />, at);
    const nav = await screen.findByRole('navigation', { name: 'Explorar' });
    expect(within(nav).getByRole('link', { name: 'Ver no mapa' }).getAttribute('href')).toBe('/mapa/12');
    expect(within(nav).getByRole('link', { name: 'Caminhos a partir daqui' }).getAttribute('href')).toBe('/caminhos?a=12');
    expect(within(nav).getByRole('link', { name: 'Grupo de controle' }).getAttribute('href')).toBe('/grupo/12');
    unmount();
    renderAt(<EntityPage />, { route: '/entidade/11', path: '/entidade/:id' });
    const personNav = await screen.findByRole('navigation', { name: 'Explorar' });
    expect(within(personNav).getByRole('link', { name: 'Ver no mapa' }).getAttribute('href')).toBe('/mapa/11');
    expect(within(personNav).queryByRole('link', { name: 'Grupo de controle' })).toBeNull();
  });

  test('atributos novos aparecem com rótulos em português', async () => {
    mockFetch({
      '/api/v1/entities/11': json(personEntity()),
      '/api/v1/entities/11/relationships?state=open': json({
        items: [
          edge({
            relationship_id: 9,
            type: 'donated_to',
            attrs: { year: 2022, amount: '1500.5', receipt_id: 'R9', desconhecido: 'x' },
          }),
        ],
        next_cursor: '',
      }),
      '/api/v1/entities/11/relationships?state=closed': json({ items: [], next_cursor: '' }),
    });
    renderAt(<EntityPage />, { route: '/entidade/11', path: '/entidade/:id' });
    const current = await screen.findByRole('region', { name: 'Vínculos atuais' });
    const row = (await within(current).findByRole('link', { name: 'EMPRESA FICTICIA A LTDA' })).closest('tr')!;
    expect(within(row).getByText('eleição 2022 · R$ 1.500,50 · recibo R9')).toBeTruthy();
    expect(row.textContent).not.toContain('desconhecido');
  });

  test('id que não é número não chama a API', async () => {
    const { fn } = mockFetch({});
    renderAt(<EntityPage />, { route: '/entidade/abc', path: '/entidade/:id' });
    expect(await screen.findByRole('heading', { name: 'Entidade não encontrada' })).toBeTruthy();
    expect(fn).not.toHaveBeenCalled();
  });
});

function closed() {
  return screen.getByRole('region', { name: 'Vínculos encerrados' });
}

function rec(id: number, entity_id: number, overrides: Partial<PersonRecord> = {}): PersonRecord {
  return {
    id, entity_id, source_key: 'receita', context_key: `6000000${id % 10}`, name: 'PESSOA FICTICIA UM',
    cpf_masked: '***777777**', identity_confidence: 1, locked: false, ...overrides,
  };
}

const NO_RELS = { items: [], next_cursor: '' };
const person11 = { route: '/entidade/11', path: '/entidade/:id' };

describe('EntityPage — ações de juntar e separar', () => {
  /** Waits until a decision dialog has its fresh data (actions are enabled only then). */
  async function ready(dialog: HTMLElement) {
    await vi.waitFor(() => expect(dialog.textContent).toMatch(/Dados carregados às \d\d:\d\d:\d\d\./));
  }
  const posts = <T extends { init?: RequestInit }>(calls: T[]) => calls.filter((c) => c.init?.method === 'POST');

  test('separar ficha na página da entidade exige motivo antes de enviar', async () => {
    let split = false;
    let entityLoads = 0;
    const { calls } = mockFetch({
      '/api/v1/entities/11': () => {
        entityLoads++;
        return json(personEntity({ records: split ? [rec(31, 11)] : [rec(31, 11), rec(35, 11, { source_key: 'tse', context_key: 'cand:2022:1' })] }));
      },
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/identity/split': () => {
        split = true;
        return json({ entity_id: 77 });
      },
      '/api/v1/sources': json([]),
    });
    renderAt(<EntityPage />, person11);
    const table = await screen.findByRole('table', { name: /2 registros/ });
    const row = within(table).getByText('cand:2022:1').closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: 'Separar ficha' }));
    const dialog = screen.getByRole('dialog', { name: 'Separar ficha' });
    expect(dialog.textContent).toContain('A ficha passa a ser uma pessoa separada e não será reunida automaticamente de novo.');
    expect(dialog.textContent).toContain('Você pode juntar de novo depois');
    expect(dialog.textContent).toContain(
      'podem ir para a fila de identidades — exceto se essa combinação já tiver sido confirmada ou rejeitada antes: nesse caso ficam separadas e não voltam para a fila',
    );
    expect(dialog.textContent?.toLowerCase()).not.toMatch(/excluir|apagar|deletar/);
    await ready(dialog);
    expect(entityLoads).toBe(2); // the dialog read the entity afresh
    expect(within(dialog).getByText('candidatura 2022')).toBeTruthy();
    const submit = within(dialog).getByRole('button', { name: 'Separar ficha' }) as HTMLButtonElement;
    const reason = within(dialog).getByLabelText(/Motivo/);
    // empty, whitespace, 501 characters: not sendable
    expect(submit.disabled).toBe(true);
    await userEvent.type(reason, '   ');
    expect(submit.disabled).toBe(true);
    await userEvent.clear(reason);
    await userEvent.click(reason);
    await userEvent.paste('x'.repeat(501));
    expect(submit.disabled).toBe(true);
    expect(dialog.textContent).toContain('501/500');
    await userEvent.click(submit);
    expect(posts(calls)).toHaveLength(0);
    await userEvent.clear(reason);
    await userEvent.type(reason, '  homônimo: outra cidade  ');
    expect(submit.disabled).toBe(false);
    await userEvent.click(submit);
    const notice = await screen.findByText(/Ficha separada/);
    const post = posts(calls)[0]!;
    expect(post.url.pathname).toBe('/api/v1/identity/split');
    expect(JSON.parse(String(post.init!.body))).toEqual({ record_ids: [35], reason: 'homônimo: outra cidade', entity_id: 11 });
    expect(within(notice.closest('div')!).getByRole('link', { name: /entidade nº 77/ }).getAttribute('href')).toBe('/entidade/77');
    // the page is reloaded: the record is gone from this entity
    expect(await screen.findByRole('table', { name: /1 registro/ })).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  test('separar: dados frescos mostram que a ficha já saiu; nada pode ser enviado', async () => {
    let loads = 0;
    const { calls } = mockFetch({
      '/api/v1/entities/11': () =>
        json(personEntity({ records: ++loads === 1 ? [rec(31, 11), rec(35, 11), rec(36, 11)] : [rec(31, 11), rec(36, 11)] })),
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/sources': json([]),
    });
    renderAt(<EntityPage />, person11);
    const table = await screen.findByRole('table', { name: /3 registros/ });
    const row = within(table).getByText('60000005').closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: 'Separar ficha' }));
    const dialog = screen.getByRole('dialog');
    expect(await within(dialog).findByText('Esta ficha já não está nesta entidade. Nada foi alterado.')).toBeTruthy();
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'motivo');
    expect((within(dialog).getByRole('button', { name: 'Separar ficha' }) as HTMLButtonElement).disabled).toBe(true);
    expect(posts(calls)).toHaveLength(0);
  });

  test('separar: 409 diz que nada foi alterado, recarrega e mantém o diálogo', async () => {
    let loads = 0;
    mockFetch({
      '/api/v1/entities/11': () => {
        loads++;
        return json(personEntity({ records: [rec(31, 11), rec(35, 11)] }));
      },
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/identity/split': json({ error: 'conflict' }, 409),
      '/api/v1/sources': json([]),
    });
    renderAt(<EntityPage />, person11);
    const table = await screen.findByRole('table', { name: /2 registros/ });
    await userEvent.click(within(table).getAllByRole('button', { name: 'Separar ficha' })[1]!);
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'motivo');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Separar ficha' }));
    expect(
      await within(dialog).findByText('Os dados mudaram desde que esta tela foi carregada. Nada foi alterado. Confira de novo.'),
    ).toBeTruthy();
    await vi.waitFor(() => expect(loads).toBe(3));
    expect(screen.getByRole('dialog')).toBe(dialog);
  });

  test('separar: 409, recarga falha (lista escondida, envio desativado), tentar de novo reabilita com dados novos', async () => {
    let loads = 0;
    mockFetch({
      '/api/v1/entities/11': () => {
        loads++;
        if (loads === 3) return json({ error: 'internal' }, 500);
        return json(personEntity({ records: [rec(31, 11), rec(35, 11)] }));
      },
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/identity/split': json({ error: 'conflict' }, 409),
      '/api/v1/sources': json([]),
    });
    renderAt(<EntityPage />, person11);
    const table = await screen.findByRole('table', { name: /2 registros/ });
    await userEvent.click(within(table).getAllByRole('button', { name: 'Separar ficha' })[1]!);
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'motivo');
    const submit = within(dialog).getByRole('button', { name: 'Separar ficha' }) as HTMLButtonElement;
    await userEvent.click(submit);
    expect(await within(dialog).findByText(/Erro interno do servidor/)).toBeTruthy();
    expect(within(dialog).queryByRole('table')).toBeNull();
    expect(dialog.textContent).not.toMatch(/Dados carregados/);
    expect(submit.disabled).toBe(true);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Tentar novamente' }));
    await ready(dialog);
    expect(within(dialog).getByRole('table')).toBeTruthy();
    expect(submit.disabled).toBe(false);
  });

  test('juntar: 409, recarga falha (listas escondidas, envio desativado), tentar de novo reabilita com dados novos', async () => {
    let loads40 = 0;
    mockFetch({
      '/api/v1/entities/11': json(personEntity({ records: [rec(31, 11)], possible_same: [candidate40] })),
      '/api/v1/entities/40': () => {
        loads40++;
        if (loads40 === 2) return json({ error: 'internal' }, 500);
        return json(entity40(loads40 === 1 ? [rec(12, 40)] : [rec(12, 40), rec(14, 40)]));
      },
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/sources': json([]),
      '/api/v1/identity/merge': json({ error: 'conflict' }, 409),
    });
    renderAt(<EntityPage />, person11);
    const section = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    await userEvent.click(within(section).getByRole('button', { name: 'Juntar a esta pessoa' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'motivo');
    const submit = within(dialog).getByRole('button', { name: 'Juntar as fichas' }) as HTMLButtonElement;
    await userEvent.click(submit);
    expect(await within(dialog).findByText(/Erro interno do servidor/)).toBeTruthy();
    expect(within(dialog).queryByRole('table', { name: /Entidade candidata/ })).toBeNull();
    expect(dialog.textContent).not.toMatch(/Dados carregados/);
    expect(submit.disabled).toBe(true);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Tentar novamente' }));
    await ready(dialog);
    expect(within(dialog).getByRole('table', { name: /Entidade candidata/ })).toBeTruthy();
    expect(dialog.textContent).toContain('As 3 fichas listadas acima');
    expect(submit.disabled).toBe(false);
  });

  test('separar não aparece com uma ficha só', async () => {
    mockFetch({
      '/api/v1/entities/11': json(personEntity({ records: [rec(31, 11)] })),
      '/api/v1/entities/11/relationships': json(NO_RELS),
    });
    renderAt(<EntityPage />, person11);
    await screen.findByRole('table', { name: /1 registro/ });
    expect(screen.queryByRole('button', { name: 'Separar ficha' })).toBeNull();
  });

  const candidate40 = { entity_id: 40, name: 'PESSOA FICTICIA UM', records: 2, shared_fragment: true };
  const entity40 = (records: PersonRecord[], overrides: Partial<EntityResponse> = {}) =>
    personEntity({ entity: { id: 40, kind: 'person', name: 'PESSOA FICTICIA UM', natural_key: '' }, records, ...overrides });

  test('juntar: diálogo lista as fichas frescas dos dois lados, exige motivo e abre a entidade resultante', async () => {
    let loads11 = 0;
    const { calls } = mockFetch({
      // the page saw one record; by the time the dialog opens there are two
      '/api/v1/entities/11': () =>
        json(personEntity({ records: ++loads11 === 1 ? [rec(31, 11)] : [rec(31, 11), rec(32, 11)], possible_same: [candidate40] })),
      '/api/v1/entities/40': json(
        entity40([
          rec(12, 40, { source_key: 'camara', context_key: 'dep:204554' }),
          rec(13, 40, { source_key: 'tse', context_key: 'donor:2022:9', cpf_masked: undefined }),
        ]),
      ),
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/entities/40/relationships': json(NO_RELS),
      '/api/v1/sources': json([{ key: 'receita', name: 'Receita Federal — CNPJ' }]),
      '/api/v1/identity/merge': json({ entity_id: 40 }),
    });
    renderAt(<EntityPage />, person11);
    const section = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    const trigger = within(section).getByRole('button', { name: 'Juntar a esta pessoa' });
    await userEvent.click(trigger);
    const dialog = screen.getByRole('dialog', { name: 'Juntar duas pessoas' });
    expect((within(dialog).getByRole('button', { name: 'Juntar as fichas' }) as HTMLButtonElement).disabled).toBe(true);
    await ready(dialog);
    const here = within(dialog).getByRole('table', { name: /Esta entidade/ });
    const there = within(dialog).getByRole('table', { name: /Entidade candidata/ });
    expect(await within(here).findAllByText('Receita Federal — CNPJ')).toHaveLength(2);
    expect(within(here).getByText('empresa 60.000.001')).toBeTruthy();
    expect(within(there).getByText('deputado')).toBeTruthy();
    expect(within(there).getByText('doação 2022')).toBeTruthy();
    expect(within(there).getByText('CPF não publicado')).toBeTruthy();
    expect(dialog.textContent).toContain('coincidência de nome e do trecho publicado do CPF não é prova de identidade');
    expect(dialog.textContent).toContain(
      'As 4 fichas listadas acima passam a ser tratadas como uma só pessoa em todos os mapas e relatórios. Você pode separar depois.',
    );
    expect(dialog.textContent).toContain('a entidade da ficha de menor número (hoje, nº 40)');
    expect(dialog.textContent).toContain('não são juntadas automaticamente a esta pessoa');
    expect(dialog.textContent).toContain('mesmo trecho do CPF');
    const submit = within(dialog).getByRole('button', { name: 'Juntar as fichas' }) as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'mesmo endereço e sócios em comum');
    await userEvent.click(submit);
    expect(await screen.findByText(/As fichas foram juntadas nesta entidade/)).toBeTruthy();
    const post = posts(calls)[0]!;
    expect(post.url.pathname).toBe('/api/v1/identity/merge');
    expect(JSON.parse(String(post.init!.body))).toEqual({
      record_ids: [31, 32, 12, 13],
      reason: 'mesmo endereço e sócios em comum',
      entity_ids: [11, 40],
    });
    // the resulting entity is shown
    await vi.waitFor(() => expect(calls.some((c) => c.url.pathname === '/api/v1/entities/40/relationships')).toBe(true));
    expect(await screen.findByRole('heading', { level: 1, name: 'PESSOA FICTICIA UM' })).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  test('juntar: quando a entidade resultante é a atual, a página recarrega e avisa', async () => {
    let merged = false;
    mockFetch({
      '/api/v1/entities/11': () =>
        json(personEntity({ records: merged ? [rec(11, 11), rec(12, 11)] : [rec(11, 11)], possible_same: merged ? [] : [candidate40] })),
      '/api/v1/entities/40': json(entity40([rec(12, 40)])),
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/sources': json([]),
      '/api/v1/identity/merge': () => {
        merged = true;
        return json({ entity_id: 11 });
      },
    });
    renderAt(<EntityPage />, person11);
    const section = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    await userEvent.click(within(section).getByRole('button', { name: 'Juntar a esta pessoa' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    expect(dialog.textContent).toContain('(hoje, nº 11)');
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'motivo');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Juntar as fichas' }));
    expect(await screen.findByText(/As fichas foram juntadas nesta entidade/)).toBeTruthy();
    expect(await screen.findByRole('table', { name: /2 registros/ })).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  test('juntar: 409 diz que nada foi alterado, recarrega os dois lados e mantém o diálogo', async () => {
    let loads40 = 0;
    mockFetch({
      '/api/v1/entities/11': json(personEntity({ records: [rec(31, 11)], possible_same: [candidate40] })),
      '/api/v1/entities/40': () => {
        loads40++;
        return json(entity40(loads40 === 1 ? [rec(12, 40)] : [rec(12, 40), rec(14, 40)]));
      },
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/sources': json([]),
      '/api/v1/identity/merge': json({ error: 'conflict' }, 409),
    });
    renderAt(<EntityPage />, person11);
    const section = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    await userEvent.click(within(section).getByRole('button', { name: 'Juntar a esta pessoa' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    expect(dialog.textContent).toContain('As 2 fichas listadas acima');
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'motivo');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Juntar as fichas' }));
    expect(
      await within(dialog).findByText('Os dados mudaram desde que esta tela foi carregada. Nada foi alterado. Confira de novo.'),
    ).toBeTruthy();
    await vi.waitFor(() => expect(dialog.textContent).toContain('As 3 fichas listadas acima'));
    expect(screen.getByRole('dialog')).toBe(dialog);
  });

  test('juntar: mais de 100 fichas no total desativa com explicação', async () => {
    const many = Array.from({ length: 60 }, (_, i) => rec(1000 + i, 11));
    const theirs = Array.from({ length: 41 }, (_, i) => rec(2000 + i, 40));
    const { calls } = mockFetch({
      '/api/v1/entities/11': json(
        personEntity({ records: many, possible_same: [{ ...candidate40, records: 41, shared_fragment: false }] }),
      ),
      '/api/v1/entities/40': json(entity40(theirs)),
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/sources': json([]),
    });
    renderAt(<EntityPage />, person11);
    const section = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    await userEvent.click(within(section).getByRole('button', { name: 'Juntar a esta pessoa' }));
    const dialog = screen.getByRole('dialog', { name: 'Juntar duas pessoas' });
    expect(await within(dialog).findByText(/Juntas, as duas entidades têm 101 fichas/)).toBeTruthy();
    expect(dialog.textContent).toContain('Não é possível juntar pela Bancada pessoas com mais de 100 fichas no total.');
    expect(dialog.textContent).not.toContain('partes menores');
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'motivo');
    expect((within(dialog).getByRole('button', { name: 'Juntar as fichas' }) as HTMLButtonElement).disabled).toBe(true);
    expect(posts(calls)).toHaveLength(0);
  });

  test('juntar: lista cortada (records_truncated) desativa com explicação', async () => {
    const { calls } = mockFetch({
      '/api/v1/entities/11': json(personEntity({ records: [rec(31, 11)], possible_same: [candidate40] })),
      '/api/v1/entities/40': json(entity40([rec(12, 40)], { records_truncated: true })),
      '/api/v1/entities/11/relationships': json(NO_RELS),
      '/api/v1/sources': json([]),
    });
    renderAt(<EntityPage />, person11);
    const section = await screen.findByRole('region', { name: 'Possível mesma pessoa' });
    await userEvent.click(within(section).getByRole('button', { name: 'Juntar a esta pessoa' }));
    const dialog = screen.getByRole('dialog');
    expect(await within(dialog).findByText(/Uma das entidades tem mais fichas do que a página mostra/)).toBeTruthy();
    await userEvent.type(within(dialog).getByLabelText(/Motivo/), 'motivo');
    expect((within(dialog).getByRole('button', { name: 'Juntar as fichas' }) as HTMLButtonElement).disabled).toBe(true);
    expect(posts(calls)).toHaveLength(0);
  });

  test('empresa não tem ações de juntar nem separar', async () => {
    mockFetch({
      '/api/v1/entities/12': json(
        companyEntity({
          records: [rec(31, 12), rec(32, 12)],
          possible_same: [{ entity_id: 40, name: 'X', records: 1, shared_fragment: false }],
        }),
      ),
      '/api/v1/entities/12/relationships': json(NO_RELS),
    });
    renderAt(<EntityPage />, at);
    await screen.findByRole('heading', { level: 1, name: 'EMPRESA FICTICIA A LTDA' });
    expect(screen.queryByRole('button', { name: 'Separar ficha' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Juntar a esta pessoa' })).toBeNull();
  });
});

describe('EntityPage — mapa de conexões', () => {
  const partner = (id: number, name: string) => ({ id, kind: 'person', name, natural_key: '' });
  const openEdges = [
    edge({ relationship_id: 1, src: partner(21, 'SOCIA FICTICIA'), dst: companyEntity().entity }),
    edge({
      relationship_id: 2,
      src: partner(22, 'SOCIO INFERIDO'),
      dst: companyEntity().entity,
      kind: 'inference',
      confidence: 0.7,
      identity_basis: 'auto',
      identity_confidence: 0.9,
    }),
  ];

  function routes(open: { items: unknown[]; next_cursor: string }) {
    return mockFetch({
      '/api/v1/entities/12': json(companyEntity()),
      '/api/v1/entities/12/relationships?state=open': json(open),
      '/api/v1/entities/12/relationships?state=closed': json({ items: [], next_cursor: '' }),
      '/api/v1/entities/21': json(personEntity({ entity: partner(21, 'SOCIA FICTICIA') })),
      '/api/v1/entities/21/relationships': json({ items: [], next_cursor: '' }),
    });
  }

  test('desenha a entidade e os vizinhos da primeira página, com legenda e link para o mapa completo', async () => {
    const { calls } = routes({ items: openEdges, next_cursor: '' });
    renderAt(<EntityPage />, at);
    const section = await screen.findByRole('region', { name: 'Mapa de conexões' });
    // the header and the tables do not wait for the drawing
    expect(screen.getByRole('heading', { level: 1, name: 'EMPRESA FICTICIA A LTDA' })).toBeTruthy();
    const app = await within(section).findByRole('application', { name: /^Mapa de conexões de EMPRESA FICTICIA A LTDA: 3 entidades e 2 vínculos$/ });
    expect(app.tabIndex).toBe(0);
    const g = await lastGraph();
    expect(g.ids()).toEqual(['e1', 'e2', 'n12', 'n21', 'n22']);
    // the inferred link and the merged identity are drawn as such, never as plain fact
    const drawn = g.handle.setElements.mock.calls.at(-1)![0];
    const e2 = drawn.find((el) => el.data.id === 'e2')!;
    expect(String(e2.classes)).toContain('inference');
    expect(String(e2.classes)).toContain('identity');
    expect(String(drawn.find((el) => el.data.id === 'e1')!.classes)).toContain('fact');
    // the legend of the drawing
    expect(within(section).getByRole('list', { name: 'Linhas do gráfico' }).textContent).toMatch(/tracejada: inferência/);
    expect(within(section).getByRole('list', { name: 'Tipos de entidade' })).toBeTruthy();
    expect(within(section).getByText(/2 entidades ligadas por 2 vínculos atuais/)).toBeTruthy();
    expect(within(section).getByRole('link', { name: 'Abrir mapa completo' }).getAttribute('href')).toBe('/mapa/12');
    // the section comes before the tables
    const current = screen.getByRole('region', { name: 'Vínculos atuais' });
    expect(section.compareDocumentPosition(current) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    await within(current).findByRole('link', { name: 'SOCIA FICTICIA' });
    // one request for the open relationships: the drawing reuses the table's
    const open = calls.filter((c) => c.url.pathname === '/api/v1/entities/12/relationships' && c.url.searchParams.get('state') === 'open');
    expect(open).toHaveLength(1);
  });

  test('sem vínculos atuais: aviso curto e o código do gráfico nem é carregado', async () => {
    routes({ items: [], next_cursor: '' });
    renderAt(<EntityPage />, at);
    const section = await screen.findByRole('region', { name: 'Mapa de conexões' });
    expect(await within(section).findByText(/Nenhum vínculo atual: não há conexões para desenhar/)).toBeTruthy();
    expect(within(section).queryByRole('application')).toBeNull();
    expect(within(section).queryByRole('button', { name: 'Ocultar o gráfico' })).toBeNull();
    await new Promise((r) => setTimeout(r, 50));
    expect(graphs).toHaveLength(0);
    expect(within(section).getByRole('link', { name: 'Abrir mapa completo' })).toBeTruthy();
  });

  test('primeira página cortada: diz que há mais e não busca as outras páginas', async () => {
    const { calls } = routes({ items: openEdges, next_cursor: 'MjAw' });
    renderAt(<EntityPage />, at);
    const section = await screen.findByRole('region', { name: 'Mapa de conexões' });
    const summary = await within(section).findByText(/mostrando os 2 primeiros; há mais/);
    expect(within(summary.closest('p')!).getByRole('link', { name: 'Abra o mapa completo' }).getAttribute('href')).toBe('/mapa/12');
    await lastGraph();
    await new Promise((r) => setTimeout(r, 50));
    expect(calls.some((c) => c.url.searchParams.has('cursor'))).toBe(false);
  });

  test('clicar destaca e oferece o link; duplo clique num nó abre a página da entidade', async () => {
    routes({ items: openEdges, next_cursor: '' });
    renderAt(<EntityPage />, at);
    const section = await screen.findByRole('region', { name: 'Mapa de conexões' });
    const g = await lastGraph();
    act(() => g.callbacks.onSelect('n21'));
    const panel = within(section).getByRole('complementary', { name: 'Detalhes do que está selecionado' });
    expect(await within(panel).findByRole('heading', { level: 3, name: 'SOCIA FICTICIA' })).toBeTruthy();
    expect(within(panel).getByRole('link', { name: 'Abrir a página de SOCIA FICTICIA' }).getAttribute('href')).toBe('/entidade/21');
    expect(within(panel).getByRole('link', { name: 'mapa a partir de SOCIA FICTICIA' }).getAttribute('href')).toBe('/mapa/21');
    act(() => g.callbacks.onSelect('e2'));
    expect(await within(panel).findByText('inferência · 70%')).toBeTruthy();
    expect(within(panel).getByText('1 vínculo entre')).toBeTruthy();
    // a click on the empty background drops the selection
    act(() => g.callbacks.onClear!());
    expect(await within(panel).findByRole('heading', { name: 'Como ler o gráfico' })).toBeTruthy();
    // the starting entity offers the full map instead of its own page
    act(() => g.callbacks.onSelect('n12'));
    expect(within(panel).getByRole('link', { name: 'Explorar no mapa completo' }).getAttribute('href')).toBe('/mapa/12');
    // the starting entity itself does not navigate
    act(() => g.callbacks.onActivate('n12'));
    expect(screen.getByRole('heading', { level: 1, name: 'EMPRESA FICTICIA A LTDA' })).toBeTruthy();
    act(() => g.callbacks.onActivate('n21'));
    expect(await screen.findByRole('heading', { level: 1, name: 'SOCIA FICTICIA' })).toBeTruthy();
  });

  test('"Pular o mapa" leva o foco para depois do gráfico', async () => {
    routes({ items: openEdges, next_cursor: '' });
    renderAt(<EntityPage />, at);
    const section = await screen.findByRole('region', { name: 'Mapa de conexões' });
    await userEvent.click(await within(section).findByRole('button', { name: 'Pular o mapa' }));
    expect(document.activeElement).toBe(within(section).getByRole('link', { name: 'Abrir mapa completo' }).closest('p'));
  });

  test('pelo teclado: a lista "Entidades no mapa" destaca no gráfico, oferece "Abrir", e Enter no gráfico abre', async () => {
    routes({ items: openEdges, next_cursor: '' });
    renderAt(<EntityPage />, at);
    const section = await screen.findByRole('region', { name: 'Mapa de conexões' });
    const g = await lastGraph();
    const app = within(section).getByRole('application');
    const hint = document.getElementById(app.getAttribute('aria-describedby')!)!;
    expect(hint.textContent).toMatch(/Clique numa entidade ou numa linha para ver os detalhes ao lado; duplo clique numa entidade abre a página dela/);
    const summary = within(section).getByText('Entidades no mapa (2)');
    summary.focus();
    await userEvent.keyboard('{Enter}');
    const destacar = within(section).getByRole('button', { name: 'Destacar no gráfico SOCIA FICTICIA' });
    destacar.focus();
    await userEvent.keyboard('{Enter}');
    expect(destacar.getAttribute('aria-pressed')).toBe('true');
    expect(g.handle.highlight).toHaveBeenLastCalledWith('n21');
    const panel = within(section).getByRole('complementary', { name: 'Detalhes do que está selecionado' });
    expect(await within(panel).findByRole('heading', { level: 3, name: 'SOCIA FICTICIA' })).toBeTruthy();
    expect(within(panel).getByRole('link', { name: 'Abrir a página de SOCIA FICTICIA' }).getAttribute('href')).toBe('/entidade/21');
    // the list has an "Abrir" per entity too
    expect(within(section).getAllByRole('link', { name: 'Abrir SOCIO INFERIDO' })).toHaveLength(1);
    // Enter on the drawing opens the highlighted node's page
    app.focus();
    await userEvent.keyboard('{Enter}');
    expect(await screen.findByRole('heading', { level: 1, name: 'SOCIA FICTICIA' })).toBeTruthy();
  });
});
