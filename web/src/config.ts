// Runtime configuration, fetched from /config.json at start so the same
// bundle runs anywhere. Public values only: this is a public OIDC client.
export interface AppConfig {
  apiBaseUrl: string;
  oidcAuthority: string;
  oidcClientId: string;
}

function httpUrl(v: unknown): v is string {
  if (typeof v !== 'string') return false;
  try {
    const u = new URL(v);
    return u.protocol === 'http:' || u.protocol === 'https:';
  } catch {
    return false;
  }
}

export function parseConfig(data: unknown): AppConfig {
  const c = (data ?? {}) as Record<string, unknown>;
  if (!httpUrl(c.apiBaseUrl) || !httpUrl(c.oidcAuthority) || typeof c.oidcClientId !== 'string' || !c.oidcClientId) {
    throw new Error('invalid config.json');
  }
  return {
    apiBaseUrl: c.apiBaseUrl.replace(/\/+$/, ''),
    oidcAuthority: c.oidcAuthority.replace(/\/+$/, ''),
    oidcClientId: c.oidcClientId,
  };
}

export async function loadConfig(): Promise<AppConfig> {
  const res = await fetch('/config.json', { cache: 'no-store', credentials: 'omit' });
  if (!res.ok) throw new Error(`config.json: ${res.status}`);
  return parseConfig(await res.json());
}
