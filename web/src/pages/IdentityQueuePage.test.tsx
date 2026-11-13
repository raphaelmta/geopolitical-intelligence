import { describe, expect, test, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { IdentityQueuePage, MSG_NOTHING_TO_CONFIRM } from './IdentityQueuePage';
import { renderAt } from '../test/render';
import { json, mockFetch } from '../test/fetchMock';
import type { Proposal, SourceStatus } from '../api/types';
import { REASON_OTHER } from '../api/identity';

const at = { route: '/identidades', path: '/identidades' };

function proposal(overrides: Partial<Proposal> = {}): Proposal {
  return {
    id: 9,
    status: 'pending',
    name: 'PESSOA REPETIDA FICTICIA',
    records: [
      {
        id: 31, entity_id: 40, source_key: 'receita', context_key: '60000001', name: 'PESSOA REPETIDA FICTICIA',
        cpf_masked: '***777777**', identity_confidence: 1, locked: false,
      },
      {
        id: 35, entity_id: 41, source_key: 'tse', context_key: 'donor:2022:250000000001', name: 'PESSOA REPETIDA FICTICIA',
        cpf_masked: '***777777**', identity_confidence: 1, locked: false,
      },
    ],
    connections: 3,
    reason: '1500 distinct CPF fragments share this name (automatic merge up to 1000)',
    reason_code: 'common_name',
    target_entity: {
      entity: { id: 40, kind: 'person', name: 'PESSOA REPETIDA FICTICIA', natural_key: '' },
      other_records: [],
      other_records_truncated: false,
    },
    ...overrides,
  };
}

const SOURCES: Partial<SourceStatus>[] = [
  { key: 'receita', name: 'Receita Federal — CNPJ' },
  { key: 'tse', name: 'TSE — eleições' },
];

function page(items: Proposal[], next_cursor = '') {
  return json({ items, next_cursor });
}

describe('IdentityQueuePage', () => {
  test('fila mostra nome, fichas com base e contexto, e número de vínculos', async () => {
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': page([proposal()]),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    expect(await screen.findByRole('heading', { level: 1, name: 'Fila de identidades' })).toBeTruthy();
    expect(document.title).toBe('Fila de identidades — Bancada');
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    expect(within(item).getByText('3 vínculos')).toBeTruthy();
    // why it was proposed
    expect(item.textContent).toContain('Nome comum: há mais trechos do CPF diferentes com este nome');
    const records = within(item).getByRole('table', { name: /Fichas propostas/ });
    const rows = within(records).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).getByText('Receita Federal — CNPJ')).toBeTruthy();
    expect(within(rows[0]!).getByText('empresa 60.000.001')).toBeTruthy();
    expect(within(rows[0]!).getByText('***777777**')).toBeTruthy();
    expect(within(rows[0]!).getByRole('link', { name: 'entidade nº 40' }).getAttribute('href')).toBe('/entidade/40');
    expect(within(rows[1]!).getByText('TSE — eleições')).toBeTruthy();
    expect(within(rows[1]!).getByText('doação 2022')).toBeTruthy();
    expect(within(rows[1]!).getByRole('link', { name: 'entidade nº 41' }).getAttribute('href')).toBe('/entidade/41');
    // what makes them look alike, without overstating
    expect(item.textContent).toContain('mesmo nome normalizado');
    expect(item.textContent).toContain('mesmo trecho do CPF');
    expect(item.textContent).toContain('coincidência de nome e do trecho publicado do CPF não é prova de identidade');
    expect(item.textContent).not.toMatch(/\d+%/);
    expect(screen.getByText('1 carregada')).toBeTruthy();
    expect(calls.find((c) => c.url.pathname === '/api/v1/identity/proposals')!.url.searchParams.get('status')).toBe('pending');
  });

  test('ficha sem CPF publicado diz isso', async () => {
    const p = proposal();
    delete p.records[1]!.cpf_masked;
    mockFetch({ '/api/v1/identity/proposals': page([p]), '/api/v1/sources': json(SOURCES) });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    expect(within(item).getByText('CPF não publicado')).toBeTruthy();
    expect(item.textContent).not.toContain('mesmo trecho do CPF');
  });

  test('bloco da entidade de destino mostra as outras fichas e o aviso', async () => {
    const p = proposal({
      target_entity: {
        entity: { id: 40, kind: 'person', name: 'PESSOA REPETIDA FICTICIA', natural_key: '' },
        other_records: [
          { id: 28, source_key: 'camara', context_key: 'dep:204554' },
          { id: 30, source_key: 'receita', context_key: '60000003' },
          { id: 31, source_key: 'fonte-nova', context_key: 'x:1' },
        ],
        other_records_truncated: false,
      },
    });
    mockFetch({ '/api/v1/identity/proposals': page([p]), '/api/v1/sources': json(SOURCES) });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    const target = within(item).getByRole('region', { name: 'Entidade de destino' });
    expect(within(target).getByRole('link', { name: /PESSOA REPETIDA FICTICIA/ }).getAttribute('href')).toBe('/entidade/40');
    expect(within(target).getByText('deputado')).toBeTruthy();
    expect(within(target).getByText('empresa 60.000.003')).toBeTruthy();
    // not in /sources (SOURCES): the static name; a key no table knows: as text
    expect(within(target).getByText('Câmara dos Deputados')).toBeTruthy();
    expect(within(target).getByText('fonte-nova')).toBeTruthy();
    expect(item.textContent).toContain('Confirmar une estas fichas a uma entidade que já reúne outras 3 fichas.');
  });

  test('sem outras fichas no destino não há aviso', async () => {
    mockFetch({ '/api/v1/identity/proposals': page([proposal()]), '/api/v1/sources': json(SOURCES) });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    expect(item.textContent).not.toContain('já reúne');
    expect(within(item).getByRole('region', { name: 'Entidade de destino' }).textContent).toContain(
      'nenhuma outra ficha',
    );
  });

  test('confirmar envia {confirm:true} e remove o item da lista', async () => {
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': page([proposal(), proposal({ id: 10, name: 'OUTRA PESSOA FICTICIA' })]),
      '/api/v1/identity/proposals/9/decision': () => new Response(null, { status: 204 }),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    const trigger = within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' });
    await userEvent.click(trigger);
    const dialog = screen.getByRole('dialog', { name: 'Confirmar que são a mesma pessoa' });
    await ready(dialog);
    // the consequence is restated; nothing is sent before the second click
    expect(dialog.textContent).toContain('passam a ser tratadas como uma só pessoa em todos os mapas e relatórios');
    expect(dialog.textContent).toContain('Você pode separar depois');
    expect(dialog.textContent).toContain('fichas novas com esta combinação ficam separadas e não voltam para a fila');
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(0);
    await ready(dialog);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirmar: são a mesma pessoa' }));
    await waitGone('PESSOA REPETIDA FICTICIA');
    const post = calls.find((c) => c.init?.method === 'POST')!;
    expect(post.url.pathname).toBe('/api/v1/identity/proposals/9/decision');
    expect(JSON.parse(String(post.init!.body))).toEqual({
      confirm: true,
      expected_target_entity_id: 40,
      expected_record_ids: [31, 35],
    });
    expect(screen.getByRole('article', { name: 'OUTRA PESSOA FICTICIA' })).toBeTruthy();
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('status').textContent).toContain('Proposta confirmada');
  });

  test('rejeitar envia {confirm:false} e remove o item da lista', async () => {
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': page([proposal()]),
      '/api/v1/identity/proposals/9/decision': () => new Response(null, { status: 204 }),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Rejeitar — são pessoas diferentes' }));
    const dialog = screen.getByRole('dialog', { name: 'Rejeitar: são pessoas diferentes' });
    await ready(dialog);
    expect(dialog.textContent).toContain('nunca mais');
    expect(dialog.textContent).toContain('Você pode juntar depois');
    expect(dialog.textContent).toContain('As fichas ficam fixadas por esta decisão');
    expect(dialog.textContent).not.toContain('Nenhuma ficha é movida');
    await ready(dialog);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }));
    await waitGone('PESSOA REPETIDA FICTICIA');
    const post = calls.find((c) => c.init?.method === 'POST')!;
    expect(JSON.parse(String(post.init!.body))).toEqual({
      confirm: false,
      expected_target_entity_id: 40,
      expected_record_ids: [31, 35],
    });
    expect(await screen.findByText('Nenhuma proposta pendente.')).toBeTruthy();
  });

  test('cancelar o diálogo não envia nada e devolve o foco', async () => {
    const { calls } = mockFetch({ '/api/v1/identity/proposals': page([proposal()]), '/api/v1/sources': json(SOURCES) });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    const trigger = within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' });
    await userEvent.click(trigger);
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(trigger);
    await userEvent.click(trigger);
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancelar' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(0);
  });

  test('409: nada foi alterado, recarrega e o diálogo fica aberto com os dados novos', async () => {
    let lists = 0;
    const changed = proposal({
      target_entity: {
        entity: { id: 41, kind: 'person', name: 'PESSOA REPETIDA FICTICIA', natural_key: '' },
        other_records: [],
        other_records_truncated: false,
      },
    });
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': () => {
        lists++;
        return lists <= 2 ? page([proposal()]) : page([changed]);
      },
      '/api/v1/identity/proposals/9/decision': json({ error: 'conflict' }, 409),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirmar: são a mesma pessoa' }));
    expect(
      await within(dialog).findByText('Os dados mudaram desde que esta tela foi carregada. Nada foi alterado. Confira de novo.'),
    ).toBeTruthy();
    await vi.waitFor(() => expect(lists).toBe(3));
    // fresh content in the still-open dialog; a new decision needs a new click
    await vi.waitFor(() => expect(dialog.textContent).toContain('(nº 41)'));
    expect(screen.getByRole('dialog')).toBe(dialog);
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(1);
    expect((within(dialog).getByRole('button', { name: 'Confirmar: são a mesma pessoa' }) as HTMLButtonElement).disabled).toBe(false);
  });

  test('ao abrir, o diálogo recarrega a proposta antes de habilitar e avisa se ela mudou', async () => {
    let lists = 0;
    let release: (() => void) | undefined;
    const now = proposal();
    now.records[1] = { ...now.records[1]!, locked: true };
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': () => {
        lists++;
        if (lists === 1) return page([proposal()]);
        return new Promise<Response>((resolve) => {
          release = () => resolve(page([now]));
        }) as unknown as Response;
      },
      '/api/v1/identity/proposals/9/decision': () => new Response(null, { status: 204 }),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Rejeitar — são pessoas diferentes' }));
    const dialog = screen.getByRole('dialog');
    const submit = within(dialog).getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }) as HTMLButtonElement;
    await vi.waitFor(() => expect(lists).toBe(2));
    expect(submit.disabled).toBe(true);
    // nothing from the old view is shown while the reload is in flight
    expect(within(dialog).queryByRole('table')).toBeNull();
    expect(dialog.textContent).not.toContain('pessoas diferentes. Esta combinação');
    await userEvent.click(submit);
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(0);
    release!();
    expect(await within(dialog).findByText('Esta proposta mudou; confira de novo.')).toBeTruthy();
    expect(within(dialog).getByRole('table', { name: /Fichas desta decisão \(1\)/ })).toBeTruthy();
    expect(dialog.textContent).toMatch(/Dados carregados às \d\d:\d\d:\d\d\./);
    expect(submit.disabled).toBe(false);
    await userEvent.click(submit);
    await vi.waitFor(() => expect(calls.some((c) => c.init?.method === 'POST')).toBe(true));
    const post = calls.find((c) => c.init?.method === 'POST')!;
    expect(JSON.parse(String(post.init!.body))).toEqual({
      confirm: false,
      expected_target_entity_id: 40,
      expected_record_ids: [31],
    });
  });

  test('proposta que sumiu ao recarregar não pode ser decidida', async () => {
    let lists = 0;
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': () => (++lists === 1 ? page([proposal()]) : page([])),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' }));
    const dialog = screen.getByRole('dialog');
    expect(await within(dialog).findByText(/não está mais entre as pendentes carregadas/)).toBeTruthy();
    expect((within(dialog).getByRole('button', { name: 'Confirmar: são a mesma pessoa' }) as HTMLButtonElement).disabled).toBe(true);
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(0);
  });

  test('proposta que sumiu e reaparece ao conferir de novo volta a poder ser decidida', async () => {
    let lists = 0;
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': () => (++lists === 2 ? page([]) : page([proposal()])),
      '/api/v1/identity/proposals/9/decision': () => new Response(null, { status: 204 }),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Rejeitar — são pessoas diferentes' }));
    const dialog = screen.getByRole('dialog');
    expect(await within(dialog).findByText(/não está mais entre as pendentes carregadas/)).toBeTruthy();
    const submit = () => within(dialog).getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }) as HTMLButtonElement;
    expect(submit().disabled).toBe(true);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Conferir de novo' }));
    await ready(dialog);
    expect(within(dialog).queryByText(/não está mais entre as pendentes carregadas/)).toBeNull();
    expect(submit().disabled).toBe(false);
    await userEvent.click(submit());
    await waitGone('PESSOA REPETIDA FICTICIA');
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(1);
  });

  test('destino nulo (nenhuma ficha ligada): não oferece Confirmar; Rejeitar envia null e lista vazia', async () => {
    const p = proposal({ target_entity: { entity: null, other_records: [], other_records_truncated: false } });
    p.records = p.records.map((r) => ({ ...r, entity_id: 0, free: false }));
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': page([p]),
      '/api/v1/identity/proposals/9/decision': () => new Response(null, { status: 204 }),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    expect(item.textContent).toContain('Nenhuma ficha desta proposta está ligada a uma entidade agora.');
    expect(item.textContent).toContain(MSG_NOTHING_TO_CONFIRM);
    expect(within(item).queryByRole('button', { name: 'Confirmar — são a mesma pessoa' })).toBeNull();
    expect(within(item).getAllByText(/sem entidade no momento \(não será movida\)/)).toHaveLength(2);
    expect(item.textContent).not.toContain('fixada por outra decisão');
    await userEvent.click(within(item).getByRole('button', { name: 'Rejeitar — são pessoas diferentes' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    // nothing is moved or locked: say so, not "fixadas"
    expect(dialog.textContent).toContain(
      'Nenhuma ficha é movida; a combinação de nome e trecho do CPF fica rejeitada e não será proposta de novo.',
    );
    expect(dialog.textContent).not.toContain('ficam fixadas');
    expect(dialog.textContent).not.toContain('elas são separadas');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }));
    await waitGone('PESSOA REPETIDA FICTICIA');
    const post = calls.find((c) => c.init?.method === 'POST')!;
    expect(JSON.parse(String(post.init!.body))).toEqual({
      confirm: false,
      expected_target_entity_id: null,
      expected_record_ids: [],
    });
  });

  test('uma ficha livre só: sem Confirmar, com o motivo; rótulos de fichas fixadas', async () => {
    const p = proposal();
    p.records[1] = { ...p.records[1]!, locked: true, free: false };
    p.records[0] = { ...p.records[0]!, free: true };
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': page([p]),
      '/api/v1/identity/proposals/9/decision': () => new Response(null, { status: 204 }),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    expect(item.textContent).toContain(MSG_NOTHING_TO_CONFIRM);
    expect(within(item).queryByRole('button', { name: 'Confirmar — são a mesma pessoa' })).toBeNull();
    expect(within(item).getByText(/fixada por outra decisão \(não será movida\)/)).toBeTruthy();
    await userEvent.click(within(item).getByRole('button', { name: 'Rejeitar — são pessoas diferentes' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    expect(dialog.textContent).toContain('fixada por outra decisão (não será movida)');
    // one free record: it is locked by the rejection
    expect(dialog.textContent).toContain('As fichas ficam fixadas por esta decisão');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }));
    await waitGone('PESSOA REPETIDA FICTICIA');
    expect(JSON.parse(String(calls.find((c) => c.init?.method === 'POST')!.init!.body)).expected_record_ids).toEqual([31]);
  });

  test('confirmar: se ao recarregar restar menos de duas fichas livres, o diálogo explica e não deixa enviar', async () => {
    let lists = 0;
    const later = proposal();
    later.records[1] = { ...later.records[1]!, locked: true };
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': () => (++lists === 1 ? page([proposal()]) : page([later])),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    expect(dialog.textContent).toContain(MSG_NOTHING_TO_CONFIRM);
    expect((within(dialog).getByRole('button', { name: 'Confirmar: são a mesma pessoa' }) as HTMLButtonElement).disabled).toBe(true);
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(0);
  });

  test.each([
    [
      'ambiguous_key',
      'já foi separada por um analista antes; por isso, mesmo depois desta confirmação, fichas novas com ela ficam separadas e não voltam para a fila.',
      'passam a ir para essa entidade',
    ],
    ['impure_target', 'passam a ir para essa entidade', 'podem ficar separadas'],
    ['common_name', 'passam a ir para essa entidade', 'podem ficar separadas'],
    ['other', 'fichas novas com esta combinação de nome e trecho do CPF podem ficar separadas', 'passam a ir para essa entidade'],
    [undefined, 'fichas novas com esta combinação de nome e trecho do CPF podem ficar separadas', 'passam a ir para essa entidade'],
  ])('confirmar: frase sobre fichas futuras conforme o reason_code (%s)', async (reason_code, want, notWant) => {
    // The English text is never read: it says "ambiguous" here whatever the code.
    const p = proposal({ reason: 'an analyst split records with this name and CPF fragment apart before', reason_code });
    mockFetch({ '/api/v1/identity/proposals': page([p]), '/api/v1/sources': json(SOURCES) });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    expect(dialog.textContent?.toLowerCase()).toContain(want.toLowerCase());
    expect(dialog.textContent).not.toContain(notWant);
  });

  test('motivo "other" ou ausente: frase cautelosa, sem o texto em inglês', async () => {
    mockFetch({
      '/api/v1/identity/proposals': page([proposal({ reason: 'something new', reason_code: 'other' })]),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    expect(item.textContent).toContain(REASON_OTHER);
    expect(item.textContent).not.toContain('something new');
  });

  test('destino que ganhou fichas depois de aberto o diálogo conta como mudança', async () => {
    let lists = 0;
    const grown = proposal({
      target_entity: {
        entity: { id: 40, kind: 'person', name: 'PESSOA REPETIDA FICTICIA', natural_key: '' },
        other_records: [{ id: 28, source_key: 'camara', context_key: 'dep:1' }],
        other_records_truncated: false,
      },
    });
    mockFetch({
      '/api/v1/identity/proposals': () => (++lists === 1 ? page([proposal()]) : page([grown])),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' }));
    const dialog = screen.getByRole('dialog');
    expect(await within(dialog).findByText('Esta proposta mudou; confira de novo.')).toBeTruthy();
    expect(dialog.textContent).toContain('Essa entidade já reúne 1 outra ficha');
  });

  test('recarga que falha: nada é mostrado nem enviável; tentar de novo volta com dados novos', async () => {
    let lists = 0;
    const { calls } = mockFetch({
      '/api/v1/identity/proposals': () => {
        lists++;
        if (lists === 2) return json({ error: 'internal' }, 500);
        return page([proposal()]);
      },
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Rejeitar — são pessoas diferentes' }));
    const dialog = screen.getByRole('dialog');
    expect(await within(dialog).findByText(/Erro interno do servidor/)).toBeTruthy();
    expect(within(dialog).queryByRole('table')).toBeNull();
    expect((within(dialog).getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }) as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Tentar novamente' }));
    await ready(dialog);
    expect(within(dialog).getByRole('table')).toBeTruthy();
    expect((within(dialog).getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }) as HTMLButtonElement).disabled).toBe(false);
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(0);
  });

  test('decidida some só das pendentes: aparece em Confirmadas com o estado da decisão', async () => {
    mockFetch({
      '/api/v1/identity/proposals?status=confirmed': page([proposal({ status: 'confirmed' })]),
      '/api/v1/identity/proposals': page([proposal()]),
      '/api/v1/identity/proposals/9/decision': () => new Response(null, { status: 204 }),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    const confirmBtn = within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' });
    // each action names its proposal
    const heading = within(item).getByRole('heading', { level: 2 });
    expect(confirmBtn.getAttribute('aria-describedby')).toBe(heading.id);
    await userEvent.click(confirmBtn);
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirmar: são a mesma pessoa' }));
    await waitGone('PESSOA REPETIDA FICTICIA');
    await userEvent.selectOptions(screen.getByLabelText('Situação'), 'confirmed');
    const decided = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    expect(decided.textContent).toContain('Decisão: confirmada');
    expect(within(decided).queryByRole('region', { name: 'Entidade de destino' })).toBeNull();
    expect(decided.textContent).not.toContain('Confirmar une');
    expect(within(decided).queryByRole('button')).toBeNull();
  });

  test('403 mostra a falta de permissão e mantém o item', async () => {
    mockFetch({
      '/api/v1/identity/proposals': page([proposal()]),
      '/api/v1/identity/proposals/9/decision': json({ error: 'forbidden' }, 403),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Rejeitar — são pessoas diferentes' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Rejeitar: são pessoas diferentes' }));
    expect(await within(dialog).findByText('Você não tem permissão para esta ação')).toBeTruthy();
    expect(screen.getByRole('article', { name: 'PESSOA REPETIDA FICTICIA' })).toBeTruthy();
  });

  test('erro de rede mantém o item e permite tentar de novo; sem envio duplo', async () => {
    let posts = 0;
    let release: (() => void) | undefined;
    mockFetch({
      '/api/v1/identity/proposals': page([proposal()]),
      '/api/v1/identity/proposals/9/decision': () => {
        posts++;
        if (posts === 1) throw new TypeError('network down');
        return new Promise<Response>((resolve) => {
          release = () => resolve(new Response(null, { status: 204 }));
        }) as unknown as Response;
      },
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    const item = await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.click(within(item).getByRole('button', { name: 'Confirmar — são a mesma pessoa' }));
    const dialog = screen.getByRole('dialog');
    await ready(dialog);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirmar: são a mesma pessoa' }));
    expect(await within(dialog).findByText(/Não foi possível falar com o servidor/)).toBeTruthy();
    expect(screen.getByRole('article', { name: 'PESSOA REPETIDA FICTICIA' })).toBeTruthy();
    // retry; while pending the button is disabled, so a second click sends nothing
    const submit = within(dialog).getByRole('button', { name: 'Confirmar: são a mesma pessoa' });
    await userEvent.click(submit);
    await vi.waitFor(() => expect(posts).toBe(2));
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    await userEvent.click(submit);
    expect(posts).toBe(2);
    release!();
    await waitGone('PESSOA REPETIDA FICTICIA');
  });

  test('carregar mais acrescenta a página seguinte', async () => {
    const { calls } = mockFetch({
      '/api/v1/identity/proposals?cursor=abc': page([proposal({ id: 10, name: 'SEGUNDA PESSOA FICTICIA' })]),
      '/api/v1/identity/proposals': page([proposal()], 'abc'),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    expect(screen.getByText('1 carregada · há mais')).toBeTruthy();
    await userEvent.click(screen.getByRole('button', { name: 'Carregar mais' }));
    expect(await screen.findByRole('article', { name: 'SEGUNDA PESSOA FICTICIA' })).toBeTruthy();
    expect(screen.getByRole('article', { name: 'PESSOA REPETIDA FICTICIA' })).toBeTruthy();
    expect(screen.getByText('2 carregadas')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Carregar mais' })).toBeNull();
    const last = calls.filter((c) => c.url.pathname === '/api/v1/identity/proposals').at(-1)!;
    expect(last.url.searchParams.get('cursor')).toBe('abc');
    expect(last.url.searchParams.get('status')).toBe('pending');
  });

  test('filtro de situação consulta de novo; decididas não têm ações', async () => {
    const { calls } = mockFetch({
      '/api/v1/identity/proposals?status=rejected': page([proposal({ id: 11, status: 'rejected', name: 'REJEITADA FICTICIA' })]),
      '/api/v1/identity/proposals': page([proposal()]),
      '/api/v1/sources': json(SOURCES),
    });
    renderAt(<IdentityQueuePage />, at);
    await screen.findByRole('article', { name: 'PESSOA REPETIDA FICTICIA' });
    await userEvent.selectOptions(screen.getByLabelText('Situação'), 'rejected');
    const item = await screen.findByRole('article', { name: 'REJEITADA FICTICIA' });
    expect(screen.queryByRole('article', { name: 'PESSOA REPETIDA FICTICIA' })).toBeNull();
    expect(within(item).queryByRole('button')).toBeNull();
    expect(calls.at(-1)!.url.searchParams.get('status')).toBe('rejected');
    // the options are the four statuses
    const options = within(screen.getByLabelText('Situação')).getAllByRole('option').map((o) => o.textContent);
    expect(options).toEqual(['Pendentes', 'Confirmadas', 'Rejeitadas', 'Obsoletas']);
  });

  test('lista vazia, carregando e erro', async () => {
    mockFetch({ '/api/v1/identity/proposals': page([]), '/api/v1/sources': json(SOURCES) });
    renderAt(<IdentityQueuePage />, at);
    expect(screen.getByRole('status').textContent).toContain('Carregando');
    expect(await screen.findByText('Nenhuma proposta pendente.')).toBeTruthy();
  });

  test('erro ao carregar a fila mostra a mensagem com tentar de novo', async () => {
    mockFetch({ '/api/v1/identity/proposals': json({ error: 'internal' }, 500), '/api/v1/sources': json(SOURCES) });
    renderAt(<IdentityQueuePage />, at);
    expect(await screen.findByText(/Erro interno do servidor/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Tentar novamente' })).toBeTruthy();
  });
});

/** Waits until the dialog has reloaded its data (the action is enabled only then). */
async function ready(dialog: HTMLElement) {
  await vi.waitFor(() => expect(dialog.textContent).toMatch(/Dados carregados às/));
}

async function waitGone(name: string) {
  await vi.waitFor(() => expect(screen.queryByRole('article', { name })).toBeNull());
}
