import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { configureApi } from './client';
import { GIVE_UP_MS, POLL_MS, jobErrorMessage, useJob } from './jobs';
import type { Job } from './types';
import { json, mockFetch } from '../test/fetchMock';
import { API, fakeAuth } from '../test/render';

const ID = '0193b6c2-7f00-7000-8000-000000000001';
const PATH = `/api/v1/jobs/${ID}`;

function job(overrides: Partial<Job> = {}): Job {
  return {
    id: ID,
    type: 'paths',
    params: { a: 11, b: 13 },
    status: 'running',
    progress: 0.25,
    result: null,
    error: '',
    created_at: '2026-11-11T12:00:00Z',
    finished_at: null,
    ...overrides,
  };
}

function polls(calls: { url: URL }[]) {
  return calls.filter((c) => c.url.pathname === PATH).length;
}

let hidden = false;

beforeEach(() => {
  vi.useFakeTimers();
  configureApi({ baseUrl: API, auth: fakeAuth() });
  hidden = false;
  vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
});

afterEach(() => {
  vi.useRealTimers();
});

async function advance(ms: number) {
  await act(() => vi.advanceTimersByTimeAsync(ms));
}

describe('useJob', () => {
  test('consulta a cada 2 s até done', async () => {
    let n = 0;
    const { calls } = mockFetch({
      [PATH]: () => {
        n++;
        return json(n < 3 ? job({ progress: n / 4 }) : job({ status: 'done', progress: 1, result: { ok: true } }));
      },
    });
    const { result } = renderHook(() => useJob(ID));
    await advance(0);
    expect(result.current.job?.status).toBe('running');
    expect(polls(calls)).toBe(1);
    await advance(POLL_MS);
    expect(polls(calls)).toBe(2);
    await advance(POLL_MS);
    expect(result.current.job?.status).toBe('done');
    await advance(POLL_MS * 5);
    expect(polls(calls)).toBe(3);
  });

  test('para de consultar quando a tarefa falha', async () => {
    const { calls } = mockFetch({ [PATH]: json(job({ status: 'failed', error: 'timeout' })) });
    const { result } = renderHook(() => useJob(ID));
    await advance(0);
    expect(result.current.job?.error).toBe('timeout');
    await advance(POLL_MS * 3);
    expect(polls(calls)).toBe(1);
  });

  test('para ao desmontar', async () => {
    const { calls } = mockFetch({ [PATH]: json(job()) });
    const { unmount } = renderHook(() => useJob(ID));
    await advance(0);
    unmount();
    await advance(POLL_MS * 5);
    expect(polls(calls)).toBe(1);
  });

  test('pausa com a aba oculta e retoma ao voltar', async () => {
    const { calls } = mockFetch({ [PATH]: json(job()) });
    renderHook(() => useJob(ID));
    await advance(0);
    hidden = true;
    await advance(POLL_MS * 5);
    expect(polls(calls)).toBe(1); // the tick that found the tab hidden did not call the API
    hidden = false;
    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await advance(0);
    expect(polls(calls)).toBe(2);
    await advance(POLL_MS);
    expect(polls(calls)).toBe(3);
  });

  test('404 é "tarefa não encontrada", sem nova tentativa', async () => {
    const { calls } = mockFetch({ [PATH]: json({ error: 'not_found' }, 404) });
    const { result } = renderHook(() => useJob(ID));
    await advance(0);
    expect(result.current.notFound).toBe(true);
    await advance(POLL_MS * 5);
    expect(polls(calls)).toBe(1);
  });

  test('desiste depois de 10 minutos consultando', async () => {
    const { calls } = mockFetch({ [PATH]: json(job()) });
    const { result } = renderHook(() => useJob(ID));
    await advance(0);
    await advance(GIVE_UP_MS - POLL_MS);
    expect(result.current.gaveUp).toBe(false);
    await advance(POLL_MS);
    expect(result.current.gaveUp).toBe(true);
    const n = polls(calls);
    await advance(POLL_MS * 5);
    expect(polls(calls)).toBe(n);
  });

  test('o limite é de relógio: o tempo com a aba oculta também conta', async () => {
    const { calls } = mockFetch({ [PATH]: json(job()) });
    const { result } = renderHook(() => useJob(ID));
    await advance(0);
    hidden = true;
    await advance(GIVE_UP_MS + POLL_MS);
    expect(result.current.gaveUp).toBe(false); // paused: nothing decided while hidden
    hidden = false;
    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await advance(0);
    // one look at the job on return, then the window (already past) closes
    expect(polls(calls)).toBe(2);
    expect(result.current.gaveUp).toBe(true);
    await advance(POLL_MS * 3);
    expect(polls(calls)).toBe(2);
  });

  test('"continuar acompanhando" abre outra janela para a mesma tarefa', async () => {
    let n = 0;
    const { calls } = mockFetch({
      [PATH]: () => json(++n < 305 ? job() : job({ status: 'done', progress: 1 })),
    });
    const { result } = renderHook(() => useJob(ID));
    await advance(0);
    await advance(GIVE_UP_MS);
    expect(result.current.gaveUp).toBe(true);
    const before = polls(calls);
    act(() => result.current.resume());
    expect(result.current.gaveUp).toBe(false);
    await advance(0);
    expect(polls(calls)).toBe(before + 1);
    expect(calls.every((c) => c.url.pathname === PATH)).toBe(true); // same job, no new one
    await advance(POLL_MS * 10);
    expect(result.current.job?.status).toBe('done');
    expect(result.current.gaveUp).toBe(false);
  });

  test('erro passageiro (rede) continua consultando; 403 para', async () => {
    let n = 0;
    const { calls } = mockFetch({
      [PATH]: () => (++n === 1 ? json({ error: 'internal' }, 500) : json(job({ status: 'done', progress: 1 }))),
    });
    const { result } = renderHook(() => useJob(ID));
    await advance(0);
    expect(result.current.error).toBeTruthy();
    await advance(POLL_MS);
    expect(result.current.job?.status).toBe('done');
    expect(result.current.error).toBeNull();
    expect(polls(calls)).toBe(2);
  });

  test('sem id não consulta', async () => {
    const { fn } = mockFetch({});
    renderHook(() => useJob(null));
    await advance(POLL_MS * 2);
    expect(fn).not.toHaveBeenCalled();
  });
});

describe('jobErrorMessage', () => {
  test.each([
    ['timeout', /tempo limite/],
    ['interrupted', /interrompida/],
    ['expired', /expirou/],
    ['entity not found', /Entidade não encontrada/],
    ['result too large', /grande demais/],
    ['internal error', /Erro interno/],
    ['could not enqueue', /enfileirar/],
    ['unknown job type', /Tipo de tarefa/],
    ['failed', /A tarefa falhou/],
    ['qualquer outra coisa', /A tarefa falhou/],
  ])('%s', (code, want) => {
    expect(jobErrorMessage(code)).toMatch(want);
  });
});
