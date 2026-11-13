// API calls: attach the in-memory access token, renew once on 401, and map
// every error to a fixed code (server text is never shown).

export interface ApiAuth {
  /** Current access token, or null when there is none. */
  accessToken(): Promise<string | null>;
  /**
   * Renews the session (refresh-token grant); the new token, or null. On null
   * the auth side has already recorded why (expired or unavailable).
   */
  renew(): Promise<string | null>;
  /**
   * The API refused a freshly renewed token: start a new login that comes
   * back to returnTo (true), or decline (false) when that would loop.
   */
  login(returnTo: string): Promise<boolean>;
  /** A request succeeded with the current token. */
  confirmed?(): void;
}

interface ApiConfig {
  baseUrl: string;
  auth: ApiAuth;
}

let config: ApiConfig | null = null;
let renewing: Promise<string | null> | null = null;
let redirecting = false;

export function configureApi(c: ApiConfig): void {
  config = { baseUrl: c.baseUrl.replace(/\/+$/, ''), auth: c.auth };
  renewing = null;
  redirecting = false;
}

/** Codes the API documents (docs/api.md), plus the client's own. */
const MESSAGES: Record<string, string> = {
  bad_request: 'O pedido não pôde ser entendido. Revise os dados e tente novamente.',
  query_too_short:
    'Digite pelo menos 3 caracteres úteis. Palavras muito comuns, como "LTDA", não contam; nomes curtos só são achados pelo CNPJ.',
  invalid_cpf: 'CPF inválido: confira os dígitos.',
  multiple_cpfs: 'A busca tem mais de um CPF. Busque um CPF de cada vez.',
  unauthorized: 'Sua sessão terminou. Abrindo o login de novo…',
  forbidden: 'Você não tem permissão para esta ação',
  not_found: 'Não encontrado.',
  method_not_allowed: 'Ação não disponível.',
  misdirected_request: 'Endereço de acesso não reconhecido pela API. Abra a Bancada pelo endereço configurado.',
  conflict: 'Esta ação conflita com o estado atual dos dados. Recarregue e confira.',
  body_too_large: 'O pedido é grande demais.',
  internal: 'Erro interno do servidor. Tente novamente em instantes.',
  auth_unavailable: 'Serviço de autenticação indisponível — tente novamente em instantes',
  timeout: 'A consulta passou do tempo limite (10 s). Tente novamente ou restrinja a busca.',
  network: 'Não foi possível falar com o servidor. Verifique a conexão e tente novamente.',
};

const GENERIC = 'Algo deu errado. Tente novamente.';

/** Codes worth a "try again" button. */
const RETRYABLE = new Set(['internal', 'auth_unavailable', 'timeout', 'network', 'unknown']);

export class ApiError extends Error {
  readonly status: number;
  /** A documented code, "network", or "unknown". */
  readonly code: string;
  constructor(status: number, code: string) {
    super(`api error ${status} ${code}`);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

export function errorMessage(err: unknown): string {
  if (err instanceof ApiError && Object.hasOwn(MESSAGES, err.code)) return MESSAGES[err.code]!;
  return GENERIC;
}

export function isRetryable(err: unknown): boolean {
  return !(err instanceof ApiError) || RETRYABLE.has(err.code);
}

export function apiGet<T>(path: string): Promise<T> {
  return request<T>('GET', path);
}

export function apiPost<T>(path: string, body: unknown): Promise<T> {
  return request<T>('POST', path, JSON.stringify(body));
}

function currentRoute(): string {
  return window.location.pathname + window.location.search + window.location.hash;
}

async function request<T>(method: 'GET' | 'POST', path: string, body?: string): Promise<T> {
  if (!config) throw new Error('configureApi was not called');
  const { baseUrl, auth } = config;
  if (redirecting) throw new ApiError(401, 'unauthorized');

  const send = async (token: string | null) => {
    const headers = new Headers({ Accept: 'application/json' });
    if (token) headers.set('Authorization', `Bearer ${token}`);
    if (body !== undefined) headers.set('Content-Type', 'application/json');
    try {
      return await fetch(`${baseUrl}/api/v1${path}`, {
        method,
        headers,
        body,
        credentials: 'omit',
        cache: 'no-store',
        referrerPolicy: 'no-referrer',
      });
    } catch {
      throw new ApiError(0, 'network');
    }
  };

  let res = await send(await auth.accessToken());
  if (res.status === 401) {
    // One renewal (shared by every request that hit 401 at the same time),
    // then one retry. A failed renewal stops here: the auth side shows why.
    // A renewed token that is refused again means a fresh login, started at
    // most once per page (and declined by the auth side when it would loop).
    renewing ??= auth.renew().catch(() => null).finally(() => {
      renewing = null;
    });
    const fresh = await renewing;
    if (!fresh || redirecting) throw new ApiError(401, 'unauthorized');
    res = await send(fresh);
    if (res.status === 401) {
      if (!redirecting) {
        redirecting = true;
        const started = await auth.login(currentRoute()).catch(() => false);
        if (!started) redirecting = false;
      }
      throw new ApiError(401, 'unauthorized');
    }
  }

  if (!res.ok) throw new ApiError(res.status, await errorCode(res));
  auth.confirmed?.();
  if (res.status === 204) return undefined as T;
  try {
    return (await res.json()) as T;
  } catch {
    throw new ApiError(res.status, 'unknown');
  }
}

async function errorCode(res: Response): Promise<string> {
  try {
    const data: unknown = await res.json();
    if (data && typeof data === 'object' && 'error' in data) {
      const code = (data as { error: unknown }).error;
      if (typeof code === 'string' && Object.hasOwn(MESSAGES, code)) return code;
    }
  } catch {
    // not JSON
  }
  return 'unknown';
}
