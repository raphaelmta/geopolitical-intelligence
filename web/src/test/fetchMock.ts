import { vi } from 'vitest';

export interface Call {
  url: URL;
  init: RequestInit | undefined;
}

export type Reply = Response | (() => Response) | ((url: URL, init?: RequestInit) => Response | Promise<Response>);

export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json; charset=utf-8' },
  });
}

/**
 * Replaces global fetch. Each route is matched against "pathname?search"
 * (prefix match on the pathname, then every query parameter listed in the
 * key must be present with that value). Unmatched requests fail the test.
 */
export function mockFetch(routes: Record<string, Reply>) {
  const calls: Call[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url);
    calls.push({ url, init });
    const match = Object.keys(routes)
      .filter((key) => matches(key, url))
      .sort((a, b) => b.length - a.length)[0];
    if (match === undefined) {
      throw new Error(`unexpected fetch ${url.pathname}${url.search}`);
    }
    const reply = routes[match]!;
    return typeof reply === 'function' ? reply(url, init) : reply.clone();
  });
  vi.stubGlobal('fetch', fn);
  return { fn, calls };
}

function matches(key: string, url: URL): boolean {
  const [path, query] = key.split('?', 2) as [string, string | undefined];
  if (url.pathname !== path) return false;
  if (!query) return true;
  const want = new URLSearchParams(query);
  for (const [k, v] of want) {
    if (url.searchParams.get(k) !== v) return false;
  }
  return true;
}
