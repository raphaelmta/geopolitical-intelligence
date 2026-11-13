import { beforeEach, describe, expect, test, vi } from 'vitest';
import { apiGet, apiPost, ApiError, configureApi, errorMessage, isRetryable } from './client';
import { json, mockFetch } from '../test/fetchMock';
import { API, fakeAuth } from '../test/render';

describe('apiGet/apiPost', () => {
  beforeEach(() => {
    window.history.replaceState(null, '', '/entidade/12?x=1');
  });

  test('anexa o token de acesso e monta a URL da API', async () => {
    const { calls } = mockFetch({ '/api/v1/search': json({ items: [] }) });
    configureApi({ baseUrl: API, auth: fakeAuth() });
    await expect(apiGet('/search?q=abc')).resolves.toEqual({ items: [] });
    expect(calls[0]!.url.href).toBe(`${API}/api/v1/search?q=abc`);
    expect(new Headers(calls[0]!.init?.headers).get('Authorization')).toBe('Bearer token-1');
  });

  test('apiPost envia JSON', async () => {
    const { calls } = mockFetch({ '/api/v1/jobs': json({ id: 'j' }, 202) });
    configureApi({ baseUrl: API, auth: fakeAuth() });
    await expect(apiPost('/jobs', { type: 'paths' })).resolves.toEqual({ id: 'j' });
    expect(calls[0]!.init?.method).toBe('POST');
    expect(calls[0]!.init?.body).toBe('{"type":"paths"}');
    expect(new Headers(calls[0]!.init?.headers).get('Content-Type')).toBe('application/json');
  });

  test('401 → uma renovação → repete e dá certo', async () => {
    let n = 0;
    const { calls } = mockFetch({
      '/api/v1/entities/12': () => (n++ === 0 ? json({ error: 'unauthorized' }, 401) : json({ ok: true })),
    });
    const auth = fakeAuth({ renew: vi.fn(async () => 'token-2') });
    configureApi({ baseUrl: API, auth });
    await expect(apiGet('/entities/12')).resolves.toEqual({ ok: true });
    expect(auth.renew).toHaveBeenCalledTimes(1);
    expect(auth.login).not.toHaveBeenCalled();
    expect(calls).toHaveLength(2);
    expect(new Headers(calls[1]!.init?.headers).get('Authorization')).toBe('Bearer token-2');
  });

  test('401 duas vezes → um único login, sem laço', async () => {
    const { calls } = mockFetch({ '/api/v1/entities/12': () => json({ error: 'unauthorized' }, 401) });
    const auth = fakeAuth({ renew: vi.fn(async () => 'token-2') });
    configureApi({ baseUrl: API, auth });
    const results = await Promise.allSettled([apiGet('/entities/12'), apiGet('/entities/12')]);
    expect(results.every((r) => r.status === 'rejected')).toBe(true);
    await expect(apiGet('/entities/12')).rejects.toBeInstanceOf(ApiError);
    expect(auth.login).toHaveBeenCalledTimes(1);
    expect(auth.login).toHaveBeenCalledWith('/entidade/12?x=1');
    // each request: the original try plus at most one retry
    expect(calls.length).toBeLessThanOrEqual(6);
    expect(vi.mocked(auth.renew).mock.calls.length).toBeLessThanOrEqual(3);
  });

  test('401 com renovação recusada → para, sem repetir nem abrir login', async () => {
    const { calls } = mockFetch({ '/api/v1/entities/12': () => json({ error: 'unauthorized' }, 401) });
    const auth = fakeAuth({ renew: vi.fn(async () => null) });
    configureApi({ baseUrl: API, auth });
    await expect(apiGet('/entities/12')).rejects.toMatchObject({ status: 401, code: 'unauthorized' });
    expect(calls).toHaveLength(1);
    expect(auth.login).not.toHaveBeenCalled();
  });

  test('login recusado pelo lado da autenticação (evita laço) não bloqueia chamadas futuras', async () => {
    const { calls } = mockFetch({ '/api/v1/entities/12': () => json({ error: 'unauthorized' }, 401) });
    const auth = fakeAuth({ renew: vi.fn(async () => 'token-2'), login: vi.fn(async () => false) });
    configureApi({ baseUrl: API, auth });
    await expect(apiGet('/entities/12')).rejects.toMatchObject({ status: 401 });
    await expect(apiGet('/entities/12')).rejects.toMatchObject({ status: 401 });
    expect(auth.login).toHaveBeenCalledTimes(2);
    expect(calls).toHaveLength(4);
  });

  test('sucesso confirma a sessão', async () => {
    mockFetch({ '/api/v1/a': json({}) });
    const auth = fakeAuth({ confirmed: vi.fn() });
    configureApi({ baseUrl: API, auth });
    await apiGet('/a');
    expect(auth.confirmed).toHaveBeenCalledTimes(1);
  });

  test('403 não repete e não renova', async () => {
    const { calls } = mockFetch({ '/api/v1/entities/12': json({ error: 'forbidden' }, 403) });
    const auth = fakeAuth();
    configureApi({ baseUrl: API, auth });
    const err = await apiGet('/entities/12').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(errorMessage(err)).toBe('Você não tem permissão para esta ação');
    expect(calls).toHaveLength(1);
    expect(auth.renew).not.toHaveBeenCalled();
  });

  test('503 auth_unavailable (p.ex. chave recém-rotacionada): mantém a sessão, sem renovar nem login, e pode tentar de novo', async () => {
    let n = 0;
    const { calls } = mockFetch({
      '/api/v1/entities/12': () =>
        n++ === 0
          ? new Response('{"error":"auth_unavailable"}', { status: 503, headers: { 'Retry-After': '30' } })
          : json({ ok: true }),
    });
    const auth = fakeAuth({ confirmed: vi.fn() });
    configureApi({ baseUrl: API, auth });
    const err = await apiGet('/entities/12').catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 503, code: 'auth_unavailable' });
    expect(isRetryable(err)).toBe(true);
    expect(errorMessage(err)).toBe('Serviço de autenticação indisponível — tente novamente em instantes');
    expect(auth.renew).not.toHaveBeenCalled();
    expect(auth.login).not.toHaveBeenCalled();
    expect(auth.confirmed).not.toHaveBeenCalled();
    expect(calls).toHaveLength(1);
    // the retry goes out with the same token, and succeeds
    await expect(apiGet('/entities/12')).resolves.toEqual({ ok: true });
    expect(new Headers(calls[1]!.init?.headers).get('Authorization')).toBe('Bearer token-1');
    expect(auth.login).not.toHaveBeenCalled();
  });

  test('503 auth_unavailable depois de uma renovação também não abre login', async () => {
    let n = 0;
    mockFetch({
      '/api/v1/entities/12': () => (n++ === 0 ? json({ error: 'unauthorized' }, 401) : json({ error: 'auth_unavailable' }, 503)),
    });
    const auth = fakeAuth({ renew: vi.fn(async () => 'token-2') });
    configureApi({ baseUrl: API, auth });
    await expect(apiGet('/entities/12')).rejects.toMatchObject({ code: 'auth_unavailable' });
    expect(auth.renew).toHaveBeenCalledTimes(1);
    expect(auth.login).not.toHaveBeenCalled();
  });

  test('421 misdirected_request: mensagem sobre o endereço de acesso, sem tentar de novo', async () => {
    mockFetch({ '/api/v1/a': json({ error: 'misdirected_request' }, 421) });
    const auth = fakeAuth();
    configureApi({ baseUrl: API, auth });
    const err = await apiGet('/a').catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 421, code: 'misdirected_request' });
    expect(errorMessage(err)).toBe(
      'Endereço de acesso não reconhecido pela API. Abra a Bancada pelo endereço configurado.',
    );
    expect(isRetryable(err)).toBe(false);
    expect(auth.renew).not.toHaveBeenCalled();
    expect(auth.login).not.toHaveBeenCalled();
  });

  test('falha de rede vira erro "network"', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))));
    configureApi({ baseUrl: API, auth: fakeAuth() });
    await expect(apiGet('/search?q=abc')).rejects.toMatchObject({ code: 'network', status: 0 });
  });

  test('corpo de erro desconhecido ou não JSON vira código "unknown"', async () => {
    mockFetch({
      '/api/v1/a': json({ error: 'pq: relation "secret_table" does not exist' }, 500),
      '/api/v1/b': new Response('<html>Bad Gateway</html>', { status: 502 }),
    });
    configureApi({ baseUrl: API, auth: fakeAuth() });
    await expect(apiGet('/a')).rejects.toMatchObject({ code: 'unknown', status: 500 });
    await expect(apiGet('/b')).rejects.toMatchObject({ code: 'unknown', status: 502 });
  });
});

describe('errorMessage', () => {
  test('mapeia códigos conhecidos para português', () => {
    expect(errorMessage(new ApiError(503, 'auth_unavailable'))).toBe(
      'Serviço de autenticação indisponível — tente novamente em instantes',
    );
    expect(errorMessage(new ApiError(400, 'query_too_short'))).toMatch(/3 caracteres/);
    expect(errorMessage(new ApiError(404, 'not_found'))).toMatch(/não encontrad/i);
  });

  test.each(['constructor', '__proto__', 'toString', 'hasOwnProperty', 'valueOf'])(
    'código herdado de Object (%s) recebe a mensagem genérica',
    async (code) => {
      expect(errorMessage(new ApiError(500, code))).toBe('Algo deu errado. Tente novamente.');
      mockFetch({ '/api/v1/x': new Response(`{"error":"${code}"}`, { status: 500 }) });
      configureApi({ baseUrl: API, auth: fakeAuth() });
      await expect(apiGet('/x')).rejects.toMatchObject({ code: 'unknown' });
    },
  );

  test('código desconhecido ou erro qualquer recebem mensagem genérica', () => {
    expect(errorMessage(new ApiError(500, 'unknown'))).toBe('Algo deu errado. Tente novamente.');
    expect(errorMessage(new Error('segredo interno'))).toBe('Algo deu errado. Tente novamente.');
  });
});
