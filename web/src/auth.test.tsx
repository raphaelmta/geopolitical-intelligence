import { afterEach, describe, expect, test, vi } from 'vitest';
import { render as rtlRender, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactElement } from 'react';
import { QueryClientProvider, useQuery } from '@tanstack/react-query';
import { AuthProvider } from 'react-oidc-context';
import { OidcClient, User, type UserManager } from 'oidc-client-ts';
import { AuthGate, createUserManager, isSignoutReturn, MSG_OUTAGE, MSG_REJECTED, rolesOf, useSession } from './auth';
import { RENEW_BEFORE_SECONDS, safeRoute, SessionController } from './session';
import { apiGet, configureApi } from './api/client';
import { json, mockFetch, type Reply } from './test/fetchMock';
import { newQueryClient } from './test/render';
import { ErrorNotice } from './components/ErrorNotice';

const AUTHORITY = 'http://kc.test/realms/r';
const TOKEN_PATH = '/realms/r/protocol/openid-connect/token';
const config = { apiBaseUrl: 'http://api.test', oidcAuthority: AUTHORITY, oidcClientId: 'web' };

function render(ui: ReactElement) {
  return rtlRender(<QueryClientProvider client={newQueryClient()}>{ui}</QueryClientProvider>);
}

function b64url(v: unknown): string {
  return btoa(JSON.stringify(v)).replace(/=+$/, '').replace(/\+/g, '-').replace(/\//g, '_');
}
function jwt(claims: Record<string, unknown>): string {
  return `${b64url({ alg: 'none', typ: 'JWT' })}.${b64url(claims)}.sig`;
}
const now = () => Math.floor(Date.now() / 1000);

function discovery() {
  return json({
    issuer: AUTHORITY,
    authorization_endpoint: `${AUTHORITY}/protocol/openid-connect/auth`,
    token_endpoint: `${AUTHORITY}/protocol/openid-connect/token`,
    end_session_endpoint: `${AUTHORITY}/protocol/openid-connect/logout`,
    revocation_endpoint: `${AUTHORITY}/protocol/openid-connect/revoke`,
    jwks_uri: `${AUTHORITY}/protocol/openid-connect/certs`,
  });
}

function tokenResponse(n: string) {
  return json({
    access_token: jwt({ sub: 'u1', roles: ['analyst'], exp: now() + 300, n }),
    refresh_token: `REFRESH-${n}`,
    id_token: jwt({ sub: 'u1', iss: AUTHORITY, aud: 'web', exp: now() + 300, iat: now(), name: 'Ana Analista' }),
    token_type: 'Bearer',
    expires_in: 300,
  });
}

let controllers: SessionController[] = [];
function session() {
  const um = createUserManager(config);
  const controller = new SessionController(um);
  controllers.push(controller);
  return { um, controller };
}
afterEach(() => {
  controllers.forEach((c) => c.stop());
  controllers = [];
  vi.useRealTimers();
  window.history.replaceState(null, '', '/');
});

function Who() {
  const s = useSession();
  return <p>conteúdo para {s.name}</p>;
}

function userWith(roles: string[], extra: Partial<ConstructorParameters<typeof User>[0]> = {}) {
  return new User({
    access_token: jwt({ sub: 'u1', roles, exp: now() + 300 }),
    token_type: 'Bearer',
    profile: { sub: 'u1', iss: AUTHORITY, aud: 'web', exp: now() + 300, iat: now(), name: 'Ana Analista' },
    expires_at: now() + 300,
    ...extra,
  });
}

async function signedInAs(roles: string[], extra = {}) {
  const s = session();
  await s.um.storeUser(userWith(roles, extra));
  return s;
}

function gate(s: ReturnType<typeof session>, children: ReactElement = <Who />, signedOut = false) {
  return render(
    <AuthProvider userManager={s.um} onSigninCallback={s.controller.afterSignin}>
      <AuthGate controller={s.controller} signedOut={signedOut}>
        {children}
      </AuthGate>
    </AuthProvider>,
  );
}

/** The redirect half of signinRedirect, then the return with ?code&state. */
async function simulateLogin(um: UserManager, state: Record<string, unknown>) {
  const request = await new OidcClient(um.settings).createSigninRequest({ request_type: 'si:r', state });
  const s = new URL(request.url).searchParams.get('state')!;
  return { request, callbackUrl: `http://localhost:3000/?code=CODE&state=${s}` };
}

describe('AuthGate', () => {
  test('usuário sem o papel analyst vê a página "sem acesso"', async () => {
    gate(await signedInAs(['admin']));
    expect(await screen.findByRole('heading', { name: 'Sem acesso' })).toBeTruthy();
    expect(screen.queryByText(/conteúdo para/)).toBeNull();
    expect(screen.getByRole('button', { name: 'Sair' })).toBeTruthy();
  });

  test('analista entra e vê o conteúdo', async () => {
    gate(await signedInAs(['analyst']));
    expect(await screen.findByText('conteúdo para Ana Analista')).toBeTruthy();
  });

  test('depois de sair mostra "Você saiu" sem redirecionar sozinho', async () => {
    const s = session();
    const signin = vi.spyOn(s.um, 'signinRedirect').mockResolvedValue(undefined);
    gate(s, <Who />, true);
    expect(await screen.findByRole('heading', { name: 'Você saiu' })).toBeTruthy();
    expect(signin).not.toHaveBeenCalled();
  });

  // Found by the e2e run: without state data the logout URL had no `state`,
  // Keycloak came back to "/" bare and the app started a new login.
  test('Sair manda state no logout, para a volta ser reconhecida como "Você saiu"', async () => {
    const s = await signedInAs(['admin']); // "Sem acesso" page: has Sair
    mockFetch({ '/realms/r/.well-known/openid-configuration': discovery() });
    const signout = vi.spyOn(s.um, 'signoutRedirect').mockResolvedValue(undefined);
    gate(s);
    await userEvent.click(await screen.findByRole('button', { name: 'Sair' }));
    expect(signout).toHaveBeenCalledTimes(1);
    const args = signout.mock.calls[0]![0];
    expect(args?.state).toBeTruthy();
    // With that state the logout URL carries one, which Keycloak sends back.
    const request = await new OidcClient(s.um.settings).createSignoutRequest({
      state: args?.state,
      id_token_hint: 'ID',
      post_logout_redirect_uri: 'http://localhost:3000/',
    });
    const state = new URL(request.url).searchParams.get('state');
    expect(state).toBeTruthy();
    const back = new URL(`http://localhost:3000/?state=${state}`);
    expect(isSignoutReturn(back as unknown as Location)).toBe(true);
  });

  test('sem sessão, redireciona para o login uma vez, guardando a rota', async () => {
    window.history.replaceState(null, '', '/entidade/9?a=1');
    const s = session();
    const signin = vi.spyOn(s.um, 'signinRedirect').mockResolvedValue(undefined);
    gate(s);
    await screen.findByText(/Entrando/);
    await vi.waitFor(() => expect(signin).toHaveBeenCalledTimes(1));
    expect(signin).toHaveBeenCalledWith({ state: { returnTo: '/entidade/9?a=1' } });
  });

  test('sem sessão, a rota guardada para a volta nunca leva um CPF da busca', async () => {
    // Synthetic CPF with valid check digits.
    window.history.replaceState(null, '', '/?q=maria+123.456.789-09&tipo=person');
    const s = session();
    const signin = vi.spyOn(s.um, 'signinRedirect').mockResolvedValue(undefined);
    gate(s);
    await vi.waitFor(() => expect(signin).toHaveBeenCalledTimes(1));
    expect(signin).toHaveBeenCalledWith({ state: { returnTo: '/?tipo=person' } });
    expect(JSON.stringify({ ...window.sessionStorage })).not.toMatch(/456/);
  });
});

describe('sessão recusada pela API depois de um novo login', () => {
  test('login causado por 401 + 401 persistente → nenhum novo redirecionamento, página de bloqueio', async () => {
    let grants = 0;
    const { calls } = mockFetch({
      '/realms/r/.well-known/openid-configuration': discovery(),
      [TOKEN_PATH]: () => tokenResponse(`T${++grants}`),
      '/api/v1/entities/1': () => json({ error: 'unauthorized' }, 401),
    });
    const s = session();
    configureApi({ baseUrl: 'http://api.test', auth: s.controller });
    const signin = vi.spyOn(s.um, 'signinRedirect');

    // The app came back from a login that a 401 started.
    const { callbackUrl } = await simulateLogin(s.um, { returnTo: '/entidade/1', reauth: true });
    s.controller.afterSignin(await s.um.signinCallback(callbackUrl));
    expect(window.location.pathname + window.location.search).toBe('/entidade/1');

    function Data() {
      const q = useQuery({ queryKey: ['e'], queryFn: () => apiGet('/entities/1') });
      return <p>{q.isError ? 'falhou' : 'carregando'}</p>;
    }
    gate(s, <Data />);

    const heading = await screen.findByRole('heading', { name: 'Sessão recusada pela API' });
    expect(heading).toBeTruthy();
    expect(screen.getByText(MSG_REJECTED)).toBeTruthy();
    expect(signin).not.toHaveBeenCalled();
    // one try, one renewal (refresh grant), one retry
    expect(calls.filter((c) => c.url.pathname === '/api/v1/entities/1')).toHaveLength(2);
    expect(grants).toBe(2); // the login, then one refresh

    // "Tentar novamente" reloads data, never a login.
    await userEvent.click(screen.getByRole('button', { name: 'Tentar novamente' }));
    await screen.findByRole('heading', { name: 'Sessão recusada pela API' });
    expect(signin).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Sair' })).toBeTruthy();
  });

  test('503 auth_unavailable (chave recém-rotacionada) mantém a sessão: nada de "sessão recusada", logout ou login; tentar de novo carrega', async () => {
    let grants = 0;
    let api = 0;
    const { calls } = mockFetch({
      '/realms/r/.well-known/openid-configuration': discovery(),
      [TOKEN_PATH]: () => tokenResponse(`T${++grants}`),
      '/api/v1/entities/1': () =>
        ++api === 1
          ? new Response('{"error":"auth_unavailable"}', { status: 503, headers: { 'Retry-After': '30' } })
          : json({ ok: true }),
    });
    const s = session();
    configureApi({ baseUrl: 'http://api.test', auth: s.controller });
    const signin = vi.spyOn(s.um, 'signinRedirect');
    const signout = vi.spyOn(s.um, 'signoutRedirect');
    // Even a session that came from a login a 401 started (the strictest case).
    const { callbackUrl } = await simulateLogin(s.um, { returnTo: '/entidade/1', reauth: true });
    s.controller.afterSignin(await s.um.signinCallback(callbackUrl));

    function Data() {
      const q = useQuery({ queryKey: ['e'], queryFn: () => apiGet('/entities/1'), retry: false });
      if (q.isError) return <ErrorNotice error={q.error} onRetry={() => void q.refetch()} />;
      return <p>{q.isSuccess ? 'dados carregados' : 'carregando'}</p>;
    }
    gate(s, <Data />);

    expect(await screen.findByText('Serviço de autenticação indisponível — tente novamente em instantes')).toBeTruthy();
    expect(screen.queryByRole('heading', { name: 'Sessão recusada pela API' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Tentar novamente' }));
    expect(await screen.findByText('dados carregados')).toBeTruthy();
    expect(signin).not.toHaveBeenCalled();
    expect(signout).not.toHaveBeenCalled();
    expect(grants).toBe(1); // the login only: no renewal for a 503
    expect(calls.filter((c) => c.url.pathname === '/api/v1/entities/1')).toHaveLength(2);
  });

  test('sem a marca de reauth, um 401 depois da renovação abre um login marcado', async () => {
    const s = await signedInAs(['analyst'], { refresh_token: 'R1' });
    mockFetch({
      '/realms/r/.well-known/openid-configuration': discovery(),
      [TOKEN_PATH]: () => tokenResponse('T2'),
      '/api/v1/entities/1': () => json({ error: 'unauthorized' }, 401),
    });
    configureApi({ baseUrl: 'http://api.test', auth: s.controller });
    const signin = vi.spyOn(s.um, 'signinRedirect').mockResolvedValue(undefined);
    window.history.replaceState(null, '', '/entidade/1');
    await expect(apiGet('/entities/1')).rejects.toMatchObject({ status: 401 });
    expect(signin).toHaveBeenCalledTimes(1);
    expect(signin).toHaveBeenCalledWith({ state: { returnTo: '/entidade/1', reauth: true } });
  });
});

describe('renovação da sessão', () => {
  test('renova pelo refresh token antes de expirar, sem iframe', async () => {
    let grants = 0;
    mockFetch({
      '/realms/r/.well-known/openid-configuration': discovery(),
      [TOKEN_PATH]: (_url: URL, init?: RequestInit) => {
        expect(String(init?.body)).toContain('grant_type=refresh_token');
        grants++;
        return tokenResponse('T2');
      },
    } as Record<string, Reply>);
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const s = await signedInAs(['analyst'], { refresh_token: 'R1', expires_at: now() + 120 });
    const createElement = vi.spyOn(document, 'createElement');
    const silent = vi.spyOn(s.um, 'signinSilent');
    s.controller.schedule((await s.um.getUser())!);

    await vi.advanceTimersByTimeAsync((120 - RENEW_BEFORE_SECONDS - 5) * 1000);
    expect(silent).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(10_000);
    vi.useRealTimers();
    await vi.waitFor(() => expect(grants).toBe(1));
    expect(silent).toHaveBeenCalledTimes(1);
    expect(silent.mock.calls[0]![0]?.forceIframeAuth).toBeFalsy();
    expect(createElement.mock.calls.some(([tag]) => String(tag).toLowerCase() === 'iframe')).toBe(false);
    expect((await s.um.getUser())?.refresh_token).toBe('REFRESH-T2');
    expect(s.um.settings.automaticSilentRenew).toBe(false);
    expect(s.um.settings.monitorSession).toBe(false);
  });

  test('sem refresh token: sessão expirada, nada de iframe', async () => {
    const s = await signedInAs(['analyst']);
    const silent = vi.spyOn(s.um, 'signinSilent');
    const createElement = vi.spyOn(document, 'createElement');
    await expect(s.controller.renew()).resolves.toBeNull();
    expect(silent).not.toHaveBeenCalled();
    expect(createElement.mock.calls.some(([tag]) => String(tag).toLowerCase() === 'iframe')).toBe(false);
    expect(s.controller.getProblem()).toBe('expired');
  });

  test('refresh recusado (invalid_grant) → "Sua sessão expirou", entrar de novo guarda a rota', async () => {
    mockFetch({
      '/realms/r/.well-known/openid-configuration': discovery(),
      [TOKEN_PATH]: json({ error: 'invalid_grant', error_description: 'Session not active' }, 400),
    });
    const s = await signedInAs(['analyst'], { refresh_token: 'R1' });
    const signin = vi.spyOn(s.um, 'signinRedirect').mockResolvedValue(undefined);
    window.history.replaceState(null, '', '/entidade/3?x=1');
    gate(s);
    await screen.findByText('conteúdo para Ana Analista');
    await s.controller.renew();
    expect(await screen.findByRole('heading', { name: 'Sua sessão expirou' })).toBeTruthy();
    expect(screen.queryByText(MSG_OUTAGE)).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Entrar novamente' }));
    expect(signin).toHaveBeenCalledWith({ state: { returnTo: '/entidade/3?x=1' } });
  });

  test('Keycloak fora do ar (rede ou 5xx) → mensagem de indisponibilidade', async () => {
    for (const reply of [
      () => {
        throw new TypeError('Failed to fetch');
      },
      () => new Response('<html>Bad gateway</html>', { status: 502, headers: { 'Content-Type': 'text/html' } }),
    ]) {
      mockFetch({ '/realms/r/.well-known/openid-configuration': discovery(), [TOKEN_PATH]: reply });
      const s = await signedInAs(['analyst'], { refresh_token: 'R1' });
      const { unmount } = gate(s);
      await screen.findByText('conteúdo para Ana Analista');
      await s.controller.renew();
      expect(await screen.findByText(MSG_OUTAGE)).toBeTruthy();
      expect(screen.queryByRole('heading', { name: 'Sua sessão expirou' })).toBeNull();
      unmount();
    }
  });
});

describe('volta depois da indisponibilidade', () => {
  test('"Tentar novamente" renova e recarrega o que falhou', async () => {
    let idpUp = false;
    let apiCalls = 0;
    mockFetch({
      '/realms/r/.well-known/openid-configuration': discovery(),
      [TOKEN_PATH]: () => {
        if (!idpUp) throw new TypeError('Failed to fetch');
        return tokenResponse('T2');
      },
      // First call: expired token (401). After the renewal: data.
      '/api/v1/entities/1': () => (apiCalls++ === 0 ? json({ error: 'unauthorized' }, 401) : json({ ok: true })),
    });
    const s = await signedInAs(['analyst'], { refresh_token: 'R1' });
    configureApi({ baseUrl: 'http://api.test', auth: s.controller });
    function Data() {
      const q = useQuery({ queryKey: ['e1'], queryFn: () => apiGet('/entities/1') });
      return <p>{q.isSuccess ? 'dados carregados' : q.isError ? 'falhou' : 'carregando'}</p>;
    }
    gate(s, <Data />);
    expect(await screen.findByText(MSG_OUTAGE)).toBeTruthy();
    idpUp = true;
    await userEvent.click(screen.getByRole('button', { name: 'Tentar novamente' }));
    expect(await screen.findByText('dados carregados')).toBeTruthy();
    expect(apiCalls).toBe(2);
  });
});

describe('papéis e rota de volta', () => {
  test('rolesOf lê a claim roles do token de acesso', () => {
    expect(rolesOf(jwt({ roles: ['analyst', 'admin'] }))).toEqual(['analyst', 'admin']);
    expect(rolesOf('não-é-jwt')).toEqual([]);
    expect(rolesOf(jwt({ roles: 'analyst' }))).toEqual([]);
  });

  test.each([
    ['/ok?x=1#y', '/ok?x=1#y'],
    ['/entidade/5', '/entidade/5'],
    ['/\t/evil', '/'],
    ['//evil', '/'],
    ['//evil.example/x', '/'],
    ['\\\\evil', '/'],
    ['/\\evil', '/'],
    ['https://evil', '/'],
    ['javascript:alert(1)', '/'],
    ['/a\u0000b', '/'],
    ['', '/'],
    [42, '/'],
    [undefined, '/'],
  ])('safeRoute(%j) → %s', (input, want) => {
    expect(safeRoute(input)).toBe(want);
  });

  test('depois do login, code e state saem da URL e a rota volta', () => {
    const { controller } = session();
    window.history.replaceState(null, '', '/?code=abc&state=xyz');
    controller.afterSignin({ state: { returnTo: '/entidade/5?x=1' } } as unknown as User);
    expect(window.location.pathname + window.location.search).toBe('/entidade/5?x=1');

    window.history.replaceState(null, '', '/?code=abc&state=xyz');
    controller.afterSignin({ state: { returnTo: '//evil.example/x' } } as unknown as User);
    expect(window.location.pathname + window.location.search).toBe('/');

    window.history.replaceState(null, '', '/?code=abc&state=xyz');
    controller.afterSignin(undefined);
    expect(window.location.search).toBe('');
  });
});

describe('armazenamento dos tokens', () => {
  test('nenhum token é gravado em localStorage ou sessionStorage', async () => {
    const secrets: string[] = [];
    let grant = 0;
    mockFetch({
      '/realms/r/.well-known/openid-configuration': discovery(),
      [TOKEN_PATH]: async () => {
        const res = tokenResponse(`T${++grant}`);
        const body = (await res.clone().json()) as Record<string, string>;
        secrets.push(body.access_token!, body.refresh_token!, body.id_token!);
        return res;
      },
    });
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    const { um, controller } = session();

    const { request, callbackUrl } = await simulateLogin(um, { returnTo: '/' });
    expect(new URL(request.url).searchParams.get('code_challenge_method')).toBe('S256');
    const user = await um.signinCallback(callbackUrl);
    controller.afterSignin(user);
    expect(user?.refresh_token).toBe('REFRESH-T1');
    // Renewal through the refresh-token grant.
    const renewed = await controller.renew();
    expect(renewed).toBe((await um.getUser())?.access_token);
    expect((await um.getUser())?.refresh_token).toBe('REFRESH-T2');

    expect(secrets).toHaveLength(6);
    expect(setItem).toHaveBeenCalled(); // the PKCE/state entry
    for (const [i, call] of setItem.mock.calls.entries()) {
      expect(setItem.mock.contexts[i]).toBe(window.sessionStorage);
      for (const s of secrets) expect(String(call[1])).not.toContain(s);
    }
    expect(window.localStorage.length).toBe(0);
    for (let i = 0; i < window.sessionStorage.length; i++) {
      const v = window.sessionStorage.getItem(window.sessionStorage.key(i)!)!;
      for (const s of secrets) expect(v).not.toContain(s);
    }
  });
});
