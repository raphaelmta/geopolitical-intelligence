// Background jobs (paths, control_group): enqueue, then poll until done.
import { useCallback, useEffect, useRef, useState } from 'react';
import { ApiError, apiGet, apiPost, isRetryable } from './client';
import type { Job, JobRequest } from './types';

export const POLL_MS = 2_000;
/** Polling stops (with a message) after this much time spent polling. */
export const GIVE_UP_MS = 10 * 60_000;

export function enqueueJob(req: JobRequest): Promise<string> {
  return apiPost<{ id: string }>('/jobs', req).then((r) => r.id);
}

export interface JobState {
  /** Last answer from GET /jobs/{id}. */
  job: Job | null;
  /** Last polling error, while polling goes on (passing) or after it stopped. */
  error: unknown;
  /** 404: the job does not exist or belongs to someone else. */
  notFound: boolean;
  /** GIVE_UP_MS of wall-clock time passed since polling started, without an end. */
  gaveUp: boolean;
}

export interface JobTracker extends JobState {
  /** After giving up: follow the same job for another GIVE_UP_MS. */
  resume(): void;
}

const EMPTY: JobState = { job: null, error: null, notFound: false, gaveUp: false };

/**
 * Polls GET /jobs/{id} every POLL_MS until done or failed. Stops on unmount
 * or when id changes; pauses while the tab is hidden and polls at once when
 * it is shown again; gives up GIVE_UP_MS (wall-clock, hidden time
 * included) after polling started, until resume() starts a new window. A 404 or an
 * error that trying again cannot fix (401, 403) stops polling; passing
 * errors (network, 5xx) are shown and polling goes on.
 */
export function useJob(id: string | null): JobTracker {
  // Tagged with the id it belongs to, so a new id never shows the old job.
  const [tagged, setTagged] = useState<{ id: string | null; state: JobState }>({ id, state: EMPTY });
  const state = tagged.id === id ? tagged.state : EMPTY;
  // Each round is one GIVE_UP_MS window of polling the same job.
  const [round, setRound] = useState(0);
  const resume = useCallback(() => setRound((r) => r + 1), []);

  useEffect(() => {
    if (!id) return;
    const setState = (f: (s: JobState) => JobState) =>
      setTagged((t) => ({ id, state: f(t.id === id ? t.state : EMPTY) }));
    let stopped = false;
    let paused = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const started = Date.now();
    if (round > 0) setState((s) => ({ ...s, gaveUp: false }));

    const schedule = () => {
      if (Date.now() + POLL_MS - started > GIVE_UP_MS) {
        setState((s) => ({ ...s, gaveUp: true }));
        return;
      }
      timer = setTimeout(() => void tick(), POLL_MS);
    };

    const tick = async () => {
      if (stopped) return;
      if (document.hidden) {
        paused = true;
        return;
      }
      try {
        const job = await apiGet<Job>(`/jobs/${encodeURIComponent(id)}`);
        if (stopped) return;
        setState((s) => ({ ...s, job, error: null }));
        if (job.status === 'done' || job.status === 'failed') return;
      } catch (err) {
        if (stopped) return;
        if (err instanceof ApiError && err.code === 'not_found') {
          setState((s) => ({ ...s, notFound: true, error: null }));
          return;
        }
        setState((s) => ({ ...s, error: err }));
        if (!isRetryable(err)) return;
      }
      schedule();
    };

    const onVisibility = () => {
      if (!document.hidden && paused && !stopped) {
        paused = false;
        void tick();
      }
    };
    document.addEventListener('visibilitychange', onVisibility);
    void tick();
    return () => {
      stopped = true;
      clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [id, round]);

  return { ...state, resume };
}

export interface JobRunner {
  /** The request last sent (what "Tentar de novo" sends again). */
  request: JobRequest | null;
  /** Enqueueing in progress. */
  sending: boolean;
  /** The POST itself failed. */
  sendError: unknown;
  state: JobState;
  run(req: JobRequest): void;
  /** Send the same request again as a new job. */
  retry(): void;
  /** Keep following the current job after giving up. */
  resume(): void;
}

/** Enqueue a job and follow it; a new run() supersedes the previous one. */
export function useJobRunner(): JobRunner {
  const [request, setRequest] = useState<JobRequest | null>(null);
  const [jobId, setJobId] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState<unknown>(null);
  const seq = useRef(0);
  const last = useRef<JobRequest | null>(null);
  const { resume, ...state } = useJob(jobId);

  const run = useCallback((req: JobRequest) => {
    const n = ++seq.current;
    last.current = req;
    setRequest(req);
    setJobId(null);
    setSendError(null);
    setSending(true);
    enqueueJob(req).then(
      (id) => {
        if (seq.current !== n) return;
        setSending(false);
        setJobId(id);
      },
      (err: unknown) => {
        if (seq.current !== n) return;
        setSending(false);
        setSendError(err);
      },
    );
  }, []);

  const retry = useCallback(() => {
    if (last.current) run(last.current);
  }, [run]);

  return { request, sending, sendError, state, run, retry, resume };
}

/** Failure messages the API passes through (docs/api.md), in Portuguese. */
const JOB_ERRORS: Record<string, string> = {
  timeout:
    'A tarefa passou do tempo limite. Tente restringir a consulta (menos profundidade, menos tipos de vínculo ou uma data).',
  interrupted: 'A tarefa foi interrompida no meio do processamento. Tente de novo.',
  expired: 'A tarefa esperou demais na fila e expirou. Tente de novo.',
  'entity not found': 'Entidade não encontrada (ou não é do tipo que esta consulta aceita).',
  'result too large': 'O resultado ficou grande demais para ser guardado. Restrinja a consulta.',
  'internal error': 'Erro interno ao processar a tarefa. Tente de novo em instantes.',
  'could not enqueue': 'Não foi possível enfileirar a tarefa. Tente de novo.',
  'unknown job type': 'Tipo de tarefa desconhecido pelo servidor.',
};

export function jobErrorMessage(error: string): string {
  return Object.hasOwn(JOB_ERRORS, error) ? JOB_ERRORS[error]! : 'A tarefa falhou.';
}
