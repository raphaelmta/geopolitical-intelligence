import { errorMessage } from '../api/client';
import { jobErrorMessage, type JobState } from '../api/jobs';

/** Whole percent of a job's progress, never 100 before it is done. */
function percent(progress: number, done: boolean): number {
  if (done) return 100;
  if (!Number.isFinite(progress) || progress <= 0) return 0;
  return Math.min(99, Math.floor(progress * 100));
}

/**
 * Status of a background job in Portuguese (na fila, em andamento NN%,
 * concluída, falhou) with a progress bar; on failure the reason and
 * "Tentar de novo", which enqueues the same request again.
 */
export function JobProgress({
  state,
  onRetry,
  onResume,
}: {
  state: JobState;
  /** Send the same request as a new job. */
  onRetry: () => void;
  /** Keep following the same job (after giving up). */
  onResume?: () => void;
}) {
  const { job, error, notFound, gaveUp } = state;
  const status = job?.status;
  const done = status === 'done';
  const pct = percent(job?.progress ?? 0, done);
  const label = !job
    ? 'enviando…'
    : status === 'queued'
      ? 'na fila'
      : status === 'running'
        ? `em andamento ${pct}%`
        : done
          ? 'concluída'
          : status === 'failed'
            ? 'falhou'
            : String(status);

  let problem: string | null = null;
  if (notFound) problem = 'Tarefa não encontrada (ela pode ter sido apagada ou ser de outra pessoa).';
  else if (status === 'failed') problem = jobErrorMessage(job?.error ?? '');
  else if (gaveUp)
    problem =
      'A tarefa não terminou depois de 10 minutos de acompanhamento. Ela pode ainda estar na fila; tente de novo mais tarde ou restrinja a consulta.';
  else if (error) problem = `Não foi possível consultar a tarefa: ${errorMessage(error)}`;
  const canRetry = notFound || status === 'failed';
  const stalled = gaveUp && !notFound && status !== 'failed';

  return (
    <div className="job">
      <p className="job__status" role="status">
        Tarefa: {label}
      </p>
      {!notFound && status !== 'failed' && (
        <progress
          className="job__bar"
          aria-label="Progresso da tarefa"
          max={100}
          {...(job && status !== 'queued' ? { value: pct } : {})}
        />
      )}
      {problem && (
        <div className="notice notice--error" role="alert">
          <p>{problem}</p>
          {canRetry && (
            <button type="button" className="button" onClick={onRetry}>
              Tentar de novo
            </button>
          )}
          {stalled && (
            <div className="actions">
              {onResume && (
                <button type="button" className="button" onClick={onResume}>
                  Continuar acompanhando
                </button>
              )}
              <button type="button" className="button" onClick={onRetry}>
                Enviar de novo
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
