import { afterEach, describe, expect, test, vi } from 'vitest';
import { act, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SourcesPage } from './SourcesPage';
import { renderAt } from '../test/render';
import { json, mockFetch } from '../test/fetchMock';
import type { SourceStatus } from '../api/types';

const at = { route: '/bases', path: '/bases' };
const ADMIN = ['analyst', 'admin'];

function source(overrides: Partial<SourceStatus> = {}): SourceStatus {
  return {
    key: 'receita',
    name: 'Receita Federal — CNPJ',
    collectable: true,
    // as the server says: only the Receita is published as editions
    editions: (overrides.key ?? 'receita') === 'receita',
    status: 'fresh',
    last_collected_at: '2026-11-01T10:30:00Z',
    next_expected_at: '2099-10-31T00:00:00Z',
    last_reference_date: '2026-10-01',
    last_observed_date: '2026-10-13',
    last_checked_at: '2026-11-05T06:00:12Z',
    last_stats: { rows_read: 1234567, rows_skipped: 0 },
    ...overrides,
  };
}

const FOUR = [
  source(),
  source({
    key: 'tse', name: 'TSE — eleições', status: 'stale', next_expected_at: '2026-03-01T00:00:00Z',
    last_reference_date: '2026-02-01', last_observed_date: '2026-02-01',
  }),
  source({
    key: 'camara', name: 'Câmara dos Deputados', status: 'never', last_collected_at: null, next_expected_at: null,
    last_reference_date: null, last_observed_date: null, last_checked_at: null, last_stats: {},
  }),
  source({ key: 'senado', name: 'Senado Federal', status: 'failed', last_error: 'download: HTTP 503 em ***123456**' }),
];

function card(name: string) {
  return screen.getByRole('region', { name });
}

describe('SourcesPage', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  test('bases: mostra "em dia", "vencida", "nunca coletada" e "falhou" conforme status', async () => {
    mockFetch({ '/api/v1/sources': json(FOUR) });
    renderAt(<SourcesPage />, at);
    expect(await screen.findByRole('heading', { level: 1, name: 'Situação das bases' })).toBeTruthy();
    expect(document.title).toBe('Situação das bases — Bancada');
    await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    const want: [string, string, string][] = [
      ['Receita Federal — CNPJ', 'em dia', 'fresh'],
      ['TSE — eleições', 'vencida', 'stale'],
      ['Câmara dos Deputados', 'nunca coletada', 'never'],
      ['Senado Federal', 'falhou', 'failed'],
    ];
    for (const [name, label, status] of want) {
      const tag = within(card(name)).getByText(label);
      // text plus a non-color cue (a distinct border pattern per status)
      expect(tag.className).toContain(`status-tag--${status}`);
    }
    // explainer
    expect(document.body.textContent).toContain(
      'vínculos vindos de uma base vencida aparecem com a etiqueta "vencido" em todas as telas',
    );
  });

  test('datas, explicações, "atrasada desde" e contadores', async () => {
    mockFetch({ '/api/v1/sources': json(FOUR) });
    renderAt(<SourcesPage />, at);
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(receita.textContent).toContain('01/10/2026');
    // an edition source: the reference date names the month; the publication date counts
    expect(receita.textContent).toContain('edição publicada em 13/10/2026');
    expect(receita.textContent).toContain('o prazo de validade conta da data de publicação da edição');
    expect(receita.textContent).not.toContain('os dados refletem a base publicada nesta data');
    const tse = card('TSE — eleições');
    expect(tse.textContent).toContain('os dados refletem a base publicada nesta data');
    expect(tse.textContent).not.toContain('edição publicada');
    expect(receita.textContent).toContain('última vez em que a plataforma conferiu se havia publicação nova');
    expect(receita.textContent).toContain('31/10/2099');
    expect(receita.textContent).not.toContain('atrasada');
    expect(within(receita).getByText('Linhas lidas')).toBeTruthy();
    expect(within(receita).getByText('1.234.567')).toBeTruthy();
    expect(within(receita).getByText('Linhas descartadas')).toBeTruthy();
    // a stale source past its expected date
    expect(card('TSE — eleições').textContent).toContain('atrasada desde 01/03/2026');
    // never collected
    const camara = card('Câmara dos Deputados');
    expect(camara.textContent).toContain('nenhuma coleta completa');
    expect(camara.textContent).not.toContain('Linhas lidas');
  });

  test('a regra de edição segue o campo editions da API, não o nome da base', async () => {
    mockFetch({
      '/api/v1/sources': json([
        source({ key: 'receita', name: 'Receita Federal — CNPJ', editions: false }),
        source({ key: 'cvm', name: 'CVM', editions: true }),
      ]),
    });
    renderAt(<SourcesPage />, at);
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(receita.textContent).not.toContain('edição publicada');
    expect(receita.textContent).toContain('os dados refletem a base publicada nesta data');
    expect(card('CVM').textContent).toContain('edição publicada em 13/10/2026');
  });

  test('a primeira coleta é sempre pedida por alguém: explicação na página e junto de "Coletar agora"', async () => {
    const FIRST =
      'A primeira coleta de cada base é sempre pedida por alguém (aqui ou pela linha de comando). Depois disso a plataforma mantém a base em dia sozinha.';
    mockFetch({ '/api/v1/sources': json(FOUR) });
    const { unmount } = renderAt(<SourcesPage />, { ...at, roles: ADMIN });
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(document.querySelector('.lede')!.textContent).toContain(FIRST);
    expect(receita.querySelector('.source-card__actions')!.textContent).toContain(FIRST);
    // admin, never collected: points at the button
    expect(card('Câmara dos Deputados').textContent).toContain('nunca coletada — peça a primeira coleta em “Coletar agora”');
    expect(receita.textContent).not.toContain('nunca coletada —');
    // nothing claims the platform starts the first collection by itself
    expect(document.body.textContent).not.toMatch(/primeira coleta (começa|é feita|é iniciada) (sozinha|automaticamente)/);
    unmount();
    // analyst: ask an administrator
    renderAt(<SourcesPage />, at);
    const camara = await screen.findByRole('region', { name: 'Câmara dos Deputados' });
    expect(camara.textContent).toContain('nunca coletada — peça a um administrador');
    expect(camara.textContent).not.toContain('Coletar agora');
    expect(document.querySelector('.lede')!.textContent).toContain(FIRST);
  });

  test('contador desconhecido aparece pela chave', async () => {
    mockFetch({ '/api/v1/sources': json([source({ last_stats: { contador_novo: 7, relationships_inserted: 2000 } })]) });
    renderAt(<SourcesPage />, at);
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(within(receita).getByText('contador_novo')).toBeTruthy();
    expect(within(receita).getByText('Vínculos novos')).toBeTruthy();
    expect(within(receita).getByText('2.000')).toBeTruthy();
  });

  test('bases: falha mostra a mensagem de erro da última coleta', async () => {
    mockFetch({ '/api/v1/sources': json(FOUR) });
    renderAt(<SourcesPage />, { ...at, roles: ADMIN });
    const senado = await screen.findByRole('region', { name: 'Senado Federal' });
    const pre = senado.querySelector('pre')!;
    expect(pre.textContent).toBe('download: HTTP 503 em ***123456**');
    // only for the failed source
    expect(card('Receita Federal — CNPJ').querySelector('pre')).toBeNull();
  });

  test('para analista, a falha explica que o detalhe é de administradores', async () => {
    const { last_error: _drop, ...noError } = FOUR[3]!;
    mockFetch({ '/api/v1/sources': json([noError]) });
    renderAt(<SourcesPage />, at);
    const senado = await screen.findByRole('region', { name: 'Senado Federal' });
    expect(senado.textContent).toContain(
      'A última coleta falhou. O detalhe do erro é visível para administradores.',
    );
    expect(senado.querySelector('pre')).toBeNull();
  });

  test('bases: botão "Coletar agora" só aparece para admin', async () => {
    mockFetch({
      '/api/v1/sources': json([source(), source({ key: 'cvm', name: 'CVM', collectable: false })]),
    });
    const { unmount } = renderAt(<SourcesPage />, at);
    await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(screen.queryByRole('button', { name: /Coletar agora/ })).toBeNull();
    unmount();
    renderAt(<SourcesPage />, { ...at, roles: ADMIN });
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(within(receita).getByRole('button', { name: 'Coletar agora' })).toBeTruthy();
    // not collectable: no button
    expect(within(card('CVM')).queryByRole('button', { name: /Coletar agora/ })).toBeNull();
  });

  test('coletar agora: confirmação, envia {} e 202 desativa o botão', async () => {
    const { calls } = mockFetch({
      '/api/v1/sources': json([source()]),
      '/api/v1/sources/receita/collect': json({}, 202),
    });
    renderAt(<SourcesPage />, { ...at, roles: ADMIN });
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    const trigger = within(receita).getByRole('button', { name: 'Coletar agora' });
    await userEvent.click(trigger);
    const dialog = screen.getByRole('dialog', { name: 'Coletar Receita Federal — CNPJ agora' });
    expect(dialog.textContent).toContain(
      'Pede uma coleta completa da edição mais recente publicada pela fonte. Se essa edição já tiver sido coletada, nada é baixado de novo: só a "Última verificação" muda. Uma coleta completa pode levar horas e roda em segundo plano.',
    );
    expect(calls.filter((c) => c.init?.method === 'POST')).toHaveLength(0);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Pedir a coleta completa' }));
    expect(
      await within(receita).findByText(
        'Coleta pedida. Acompanhe "Última coleta" e "Última verificação"; esta tela se atualiza a cada minuto.',
      ),
    ).toBeTruthy();
    const post = calls.find((c) => c.init?.method === 'POST')!;
    expect(post.url.pathname).toBe('/api/v1/sources/receita/collect');
    expect(JSON.parse(String(post.init!.body))).toEqual({});
    expect(screen.queryByRole('dialog')).toBeNull();
    expect((within(receita).getByRole('button', { name: /Coletar agora/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  test('coletar agora: 409 e 403 mostram a mensagem; 409 recarrega a lista', async () => {
    let status = 409;
    const { calls } = mockFetch({
      '/api/v1/sources': json([source()]),
      '/api/v1/sources/receita/collect': () => json({ error: status === 409 ? 'conflict' : 'forbidden' }, status),
    });
    renderAt(<SourcesPage />, { ...at, roles: ADMIN });
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    await userEvent.click(within(receita).getByRole('button', { name: 'Coletar agora' }));
    let dialog = screen.getByRole('dialog');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Pedir a coleta completa' }));
    expect(
      await within(dialog).findByText('Já existe uma coleta em andamento ou um pedido aguardando início para esta base.'),
    ).toBeTruthy();
    await vi.waitFor(() => expect(calls.filter((c) => c.url.pathname === '/api/v1/sources')).toHaveLength(2));
    status = 403;
    await userEvent.click(within(dialog).getByRole('button', { name: 'Pedir a coleta completa' }));
    expect(await within(dialog).findByText('Você não tem permissão para esta ação')).toBeTruthy();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancelar' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    dialog = receita;
    expect((within(dialog).getByRole('button', { name: 'Coletar agora' }) as HTMLButtonElement).disabled).toBe(false);
  });

  test('atualiza a cada 60 s com a aba visível e pausa quando escondida; botão Atualizar', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let hidden = false;
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => (hidden ? 'hidden' : 'visible'));
    const { calls } = mockFetch({ '/api/v1/sources': json([source()]) });
    renderAt(<SourcesPage />, at);
    await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    const count = () => calls.filter((c) => c.url.pathname === '/api/v1/sources').length;
    expect(count()).toBe(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    await vi.waitFor(() => expect(count()).toBe(2));
    hidden = true;
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(180_000);
    });
    expect(count()).toBe(2);
    hidden = false;
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    await vi.waitFor(() => expect(count()).toBeGreaterThanOrEqual(3));
    // back to visible: reloaded at once, without waiting for the interval
    hidden = true;
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    const hiddenAt = count();
    hidden = false;
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await vi.waitFor(() => expect(count()).toBe(hiddenAt + 1));
    const before = count();
    await userEvent.click(screen.getByRole('button', { name: 'Atualizar' }));
    await vi.waitFor(() => expect(count()).toBe(before + 1));
  });

  test('analista não vê last_error mesmo se a API o mandar por engano', async () => {
    mockFetch({ '/api/v1/sources': json([FOUR[3]]) });
    renderAt(<SourcesPage />, at);
    const senado = await screen.findByRole('region', { name: 'Senado Federal' });
    expect(senado.querySelector('pre')).toBeNull();
    expect(senado.textContent).not.toContain('HTTP 503');
    expect(senado.textContent).toContain('O detalhe do erro é visível para administradores.');
  });

  test('base em dia com previsão já passada não aparece como atrasada', async () => {
    mockFetch({ '/api/v1/sources': json([source({ next_expected_at: '2020-01-01T00:00:00Z' })]) });
    renderAt(<SourcesPage />, at);
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(receita.textContent).toContain('01/01/2020');
    expect(receita.textContent).not.toContain('atrasada');
  });

  test.each([
    // [TZ, now (UTC), late?]
    ['America/Sao_Paulo', '2026-03-01T23:30:00Z', false], // 01/03 20:30 in Brasília: the expected day itself
    ['America/Sao_Paulo', '2026-03-02T02:59:00Z', false], // 01/03 23:59 in Brasília
    ['America/Sao_Paulo', '2026-03-02T03:00:00Z', true], // 02/03 00:00 in Brasília
    ['UTC', '2026-03-01T23:59:00Z', false],
    ['UTC', '2026-03-02T00:00:00Z', true],
  ])('"atrasada desde" compara datas (%s, agora %s)', async (tz, now, late) => {
    vi.stubEnv('TZ', tz); // restored after the test (unstubEnvs)
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date(now));
    mockFetch({ '/api/v1/sources': json([source({ status: 'stale', next_expected_at: '2026-03-01T00:00:00Z' })]) });
    renderAt(<SourcesPage />, at);
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(receita.textContent).toContain('01/03/2026');
    expect(receita.textContent?.includes('atrasada desde 01/03/2026')).toBe(late);
  });

  test('pedido aguardando início aparece no cartão e desativa "Coletar agora"', async () => {
    const at10 = new Date();
    at10.setHours(10, 5, 0, 0);
    mockFetch({ '/api/v1/sources': json([source({ pending_request_at: at10.toISOString() })]) });
    renderAt(<SourcesPage />, { ...at, roles: ADMIN });
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(within(receita).getByText('Coleta pedida às 10:05, aguardando início.')).toBeTruthy();
    expect((within(receita).getByRole('button', { name: 'Coletar agora' }) as HTMLButtonElement).disabled).toBe(true);
  });

  test('pedido de outro dia mostra a data; analista também vê que há pedido', async () => {
    mockFetch({ '/api/v1/sources': json([source({ pending_request_at: '2026-01-05T13:00:00Z' })]) });
    renderAt(<SourcesPage />, at);
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(receita.textContent).toMatch(/Coleta pedida em 05\/01\/2026 às \d\d:00, aguardando início\./);
    expect(within(receita).queryByRole('button', { name: /Coletar agora/ })).toBeNull();
  });

  test('o bloqueio da sessão vale só até a próxima atualização: depois manda o pending_request_at', async () => {
    let n = 0;
    mockFetch({
      '/api/v1/sources': () => json([source(++n === 1 ? {} : { pending_request_at: null })]),
      '/api/v1/sources/receita/collect': json({}, 202),
    });
    renderAt(<SourcesPage />, { ...at, roles: ADMIN });
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    await userEvent.click(within(receita).getByRole('button', { name: 'Coletar agora' }));
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Pedir a coleta completa' }));
    const btn = () => within(receita).getByRole('button', { name: 'Coletar agora' }) as HTMLButtonElement;
    await vi.waitFor(() => expect(btn().disabled).toBe(true));
    await userEvent.click(screen.getByRole('button', { name: 'Atualizar' }));
    await vi.waitFor(() => expect(btn().disabled).toBe(false));
  });

  test('rótulos: última coleta completa concluída e contadores', async () => {
    mockFetch({ '/api/v1/sources': json([source()]) });
    renderAt(<SourcesPage />, at);
    const receita = await screen.findByRole('region', { name: 'Receita Federal — CNPJ' });
    expect(within(receita).getByText('Última coleta completa concluída')).toBeTruthy();
    expect(within(receita).getByText('Contadores da última coleta concluída (completa ou parcial)')).toBeTruthy();
  });

  test('erro ao carregar as bases', async () => {
    mockFetch({ '/api/v1/sources': json({ error: 'internal' }, 500) });
    renderAt(<SourcesPage />, at);
    expect(await screen.findByText(/Erro interno do servidor/)).toBeTruthy();
  });
});
