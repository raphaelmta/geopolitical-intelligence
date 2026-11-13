import { errorMessage, isRetryable } from '../api/client';

/** An API error in Portuguese, with "try again" when trying again can help. */
export function ErrorNotice({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  return (
    <div className="notice notice--error" role="alert">
      <p>{errorMessage(error)}</p>
      {onRetry && isRetryable(error) && (
        <button type="button" className="button" onClick={onRetry}>
          Tentar novamente
        </button>
      )}
    </div>
  );
}
