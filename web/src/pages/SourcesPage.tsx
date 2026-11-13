import { useEffect, useId, useRef, useState, useSyncExternalStore, type FormEvent } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError, apiGet, apiPost } from '../api/client';
import { formatDate, formatTimestamp, localToday } from '../api/format';
import type { SourceStatus } from '../api/types';
import { useSession } from '../auth';
import { Dialog, DialogActions } from '../components/Dialog';
import { ErrorNotice } from '../components/ErrorNotice';
import { usePageHeading } from '../usePageHeading';

/** How often the list reloads while the tab is visible. */
export const REFRESH_MS = 60_000;

const STATUS_LABELS: Record<string, string> = {
  fresh: 'em dia',
  stale: 'vencida',
  never: 'nunca coletada',
  failed: 'falhou',
};

/** Portuguese labels of the counters connectors report; others show their key. */
const STAT_LABELS: [string, string][] = [
  ['rows_read', 'Linhas lidas'],
  ['rows_skipped', 'Linhas descartadas'],
  ['rows_copied', 'Linhas copiadas'],
  ['events_read', 'Eventos lidos'],
  ['relationships_inserted', 'Vínculos novos'],
  ['relationships_seen', 'Vínculos revistos'],
  ['relationships_replaced', 'Vínculos substituídos'],
  ['person_records_inserted', 'Fichas de pessoa novas'],
  ['entities_person_inserted', 'Pessoas novas'],
  ['entities_company_inserted', 'Empresas novas'],
  ['entities_party_inserted', 'Partidos novos'],
  ['entities_foreign_inserted', 'Entidades estrangeiras novas'],
  ['company_versions_inserted', 'Versões cadastrais novas'],
  ['company_versions_seen', 'Versões cadastrais revistas'],
  ['company_versions_closed', 'Versões cadastrais encerradas'],
];
const STAT_LABEL = new Map(STAT_LABELS);
const STAT_ORDER = new Map(STAT_LABELS.map(([k], i) => [k, i]));

export const MSG_FIRST_COLLECTION =
  'A primeira coleta de cada base é sempre pedida por alguém (aqui ou pela linha de comando). Depois disso a plataforma mantém a base em dia sozinha.';

const MSG_COLLECT_CONFLICT = 'Já existe uma coleta em andamento ou um pedido aguardando início para esta base.';
const MSG_COLLECT_OK =
  'Coleta pedida. Acompanhe "Última coleta" e "Última verificação"; esta tela se atualiza a cada minuto.';

const number = new Intl.NumberFormat('pt-BR');

/** The UTC day of an instant (next_expected_at is a reference date + period, at 00:00 UTC). */
function utcDay(ts: string | null): string {
  if (!ts) return '';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return formatDate(d.toISOString().slice(0, 10));
}

function subscribeVisibility(fn: () => void) {
  document.addEventListener('visibilitychange', fn);
  return () => document.removeEventListener('visibilitychange', fn);
}

/** Whether the tab is visible (auto-refresh pauses while it is hidden). */
function usePageVisible(): boolean {
  return useSyncExternalStore(subscribeVisibility, () => document.visibilityState !== 'hidden');
}

/**
 * Collections requested from this screen, by source key → when (ms). Kept
 * with the query cache (no web storage) and only until the list is next
 * refetched: from then on the server's pending_request_at is the truth.
 */
const REQUESTED_KEY = ['collect-requested'];
type Requested = Record<string, number>;

/** "HH:MM" today, "DD/MM/AAAA às HH:MM" another day. */
function requestTime(ts: string): string {
  const full = formatTimestamp(ts);
  const [day, time] = full.split(' ');
  if (!time) return full;
  return formatDate(localToday()) === day ? `às ${time}` : `em ${day} às ${time}`;
}

export function SourcesPage() {
  const h1 = usePageHeading('Situação das bases');
  const { roles } = useSession();
  const admin = roles.includes('admin');
  const visible = usePageVisible();
  const queryClient = useQueryClient();
  const q = useQuery({
    queryKey: ['sources'],
    queryFn: () => apiGet<SourceStatus[]>('/sources'),
    refetchInterval: visible ? REFRESH_MS : false,
    refetchIntervalInBackground: false,
  });
  const requested = useQuery({
    queryKey: REQUESTED_KEY,
    queryFn: () => queryClient.getQueryData<Requested>(REQUESTED_KEY) ?? {},
    initialData: {} as Requested,
    staleTime: Infinity,
    gcTime: Infinity,
  }).data;
  const markRequested = (key: string) =>
    queryClient.setQueryData<Requested>(REQUESTED_KEY, (prev = {}) => ({ ...prev, [key]: Date.now() }));

  // Back to a visible tab: the list may be minutes old; reload at once.
  const wasVisible = useRef(visible);
  useEffect(() => {
    if (visible && !wasVisible.current) void q.refetch();
    wasVisible.current = visible;
  }, [visible, q]);

  return (
    <>
      <h1 ref={h1} tabIndex={-1}>
        Situação das bases
      </h1>
      <div className="lede">
        <p>
          Cada base vem de uma fonte oficial e é coletada de novo quando a fonte publica dados novos. Uma base fica{' '}
          <strong>vencida</strong> quando a última coleta completa reflete uma publicação mais antiga que o prazo da
          fonte: vínculos vindos de uma base vencida aparecem com a etiqueta "vencido" em todas as telas. Para a
          Receita, publicada em edições mensais, o prazo conta da data de publicação da edição; para as demais, da data
          de referência dos dados.
        </p>
        <p>{MSG_FIRST_COLLECTION}</p>
        <p className="muted small">
          Uma base vencida, nunca coletada ou com falha deixa as análises que dependem dela desatualizadas ou
          incompletas. A lista se atualiza sozinha a cada minuto enquanto esta aba está visível.
        </p>
      </div>

      <div className="row-actions">
        <button type="button" className="button" disabled={q.isFetching} onClick={() => void q.refetch()}>
          Atualizar
        </button>
        {q.dataUpdatedAt > 0 && (
          <span className="muted small">atualizada em {formatTimestamp(new Date(q.dataUpdatedAt).toISOString())}</span>
        )}
      </div>

      {q.isPending && <p role="status">Carregando as bases…</p>}
      {q.isError && <ErrorNotice error={q.error} onRetry={() => void q.refetch()} />}
      {q.isSuccess && q.data.length === 0 && <p className="empty">Nenhuma base cadastrada.</p>}
      {q.data && q.data.length > 0 && (
        <div className="sources">
          {q.data.map((s) => (
            <SourceCard
              key={s.key}
              source={s}
              admin={admin}
              requested={(requested[s.key] ?? 0) >= q.dataUpdatedAt}
              onRequested={() => markRequested(s.key)}
              onConflict={() => void q.refetch()}
            />
          ))}
        </div>
      )}
    </>
  );
}

function SourceCard({
  source: s,
  admin,
  requested,
  onRequested,
  onConflict,
}: {
  source: SourceStatus;
  admin: boolean;
  /** Requested from this screen since the list was last loaded. */
  requested: boolean;
  onRequested: () => void;
  onConflict: () => void;
}) {
  const h = useId();
  const [collecting, setCollecting] = useState(false);
  const doneRef = useRef<HTMLParagraphElement>(null);
  const reference = formatDate(s.last_reference_date);
  // Published as dated editions (GET /sources `editions`): the reference
  // date names the edition's month and staleness counts from the
  // publication date (last_observed_date).
  const edition = s.editions === true;
  const published = formatDate(s.last_observed_date);
  const expected = utcDay(s.next_expected_at);
  // Dates, not instants: the expected day itself is not late (no early flag at 21:00 in Brasília).
  const late = s.status !== 'fresh' && s.next_expected_at != null && localToday() > s.next_expected_at.slice(0, 10);
  const pending = s.pending_request_at ?? null;
  const stats = Object.entries(s.last_stats ?? {}).sort(([a], [b]) => {
    const ia = STAT_ORDER.get(a) ?? Infinity;
    const ib = STAT_ORDER.get(b) ?? Infinity;
    return ia !== ib ? ia - ib : a.localeCompare(b);
  });

  return (
    <section aria-labelledby={h} className={`source-card source-card--${s.status}`}>
      <header className="source-card__head">
        <h2 id={h}>{s.name}</h2>
        <p className="source-card__status">
          Situação: <span className={`status-tag status-tag--${s.status}`}>{STATUS_LABELS[s.status] ?? s.status}</span>
        </p>
      </header>

      <dl className="facts">
        <dt>Data de referência dos dados</dt>
        <dd>
          {reference ? (
            edition ? (
              <>
                {reference}
                <span className="muted small"> — mês da edição</span>
                {published && (
                  <>
                    {' '}
                    · edição publicada em {published}
                    <span className="muted small"> — o prazo de validade conta da data de publicação da edição</span>
                  </>
                )}
              </>
            ) : (
              <>
                {reference}
                <span className="muted small">
                  {' '}
                  — os dados refletem a base publicada nesta data; o prazo de validade conta dela
                </span>
              </>
            )
          ) : (
            <span className="muted">nenhuma coleta completa concluída</span>
          )}
        </dd>
        <dt>Última coleta completa concluída</dt>
        <dd>{formatTimestamp(s.last_collected_at) || <span className="muted">nunca</span>}</dd>
        <dt>Última verificação</dt>
        <dd>
          {formatTimestamp(s.last_checked_at) || <span className="muted">nunca</span>}
          <span className="muted small">
            {' '}
            — última vez em que a plataforma conferiu se havia publicação nova e confirmou a base em dia
          </span>
        </dd>
        <dt>Próxima publicação esperada</dt>
        <dd>
          {expected || <span className="muted">sem previsão</span>}
          {late && <strong className="late"> · atrasada desde {expected}</strong>}
        </dd>
      </dl>

      {s.status === 'never' && (
        <p className="source-card__never">
          {!s.collectable
            ? 'nunca coletada — esta base ainda não tem coletor'
            : admin
              ? 'nunca coletada — peça a primeira coleta em “Coletar agora”'
              : 'nunca coletada — peça a um administrador'}
        </p>
      )}

      {pending && <p className="source-card__pending">Coleta pedida {requestTime(pending)}, aguardando início.</p>}

      {s.status === 'failed' &&
        (admin && s.last_error ? (
          <div className="source-card__error">
            <p>Erro da última coleta:</p>
            <pre className="error-text">{s.last_error}</pre>
          </div>
        ) : (
          <p className="source-card__error">
            {admin
              ? 'A última coleta falhou, sem mensagem de erro registrada.'
              : 'A última coleta falhou. O detalhe do erro é visível para administradores.'}
          </p>
        ))}

      {stats.length > 0 && (
        <div className="source-card__stats">
          <h3>Contadores da última coleta concluída (completa ou parcial)</h3>
          <dl className="stats">
            {stats.map(([k, v]) => (
              <div key={k} className="stats__pair">
                <dt>{STAT_LABEL.get(k) ?? k}</dt>
                <dd>{typeof v === 'number' ? number.format(v) : String(v)}</dd>
              </div>
            ))}
          </dl>
        </div>
      )}

      {admin && s.collectable && (
        <div className="source-card__actions">
          <button
            type="button"
            className="button"
            disabled={requested || pending != null}
            onClick={() => setCollecting(true)}
          >
            Coletar agora
          </button>
          <p className="muted small">{MSG_FIRST_COLLECTION}</p>
          {requested && (
            <p ref={doneRef} role="status" tabIndex={-1} className="small">
              {MSG_COLLECT_OK}
            </p>
          )}
        </div>
      )}

      {collecting && (
        <CollectDialog
          source={s}
          onClose={() => setCollecting(false)}
          onDone={() => {
            onRequested();
            setCollecting(false);
          }}
          onConflict={onConflict}
          fallbackFocus={() => doneRef.current}
        />
      )}
    </section>
  );
}

function CollectDialog({
  source: s,
  onClose,
  onDone,
  onConflict,
  fallbackFocus,
}: {
  source: SourceStatus;
  onClose: () => void;
  onDone: () => void;
  onConflict: () => void;
  fallbackFocus: () => HTMLElement | null;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const inFlight = useRef(false);

  const send = async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setError(null);
    try {
      await apiPost<unknown>(`/sources/${encodeURIComponent(s.key)}/collect`, {});
      onDone();
    } catch (err) {
      inFlight.current = false;
      setBusy(false);
      setError(err);
      if (err instanceof ApiError && err.code === 'conflict') onConflict();
    }
  };
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    void send();
  };

  return (
    <Dialog title={`Coletar ${s.name} agora`} onClose={onClose} busy={busy} fallbackFocus={fallbackFocus}>
      <form onSubmit={onSubmit}>
        <p>
          Pede uma coleta completa da edição mais recente publicada pela fonte. Se essa edição já tiver sido coletada,
          nada é baixado de novo: só a "Última verificação" muda. Uma coleta completa pode levar horas e roda em segundo
          plano.
        </p>
        <p className="muted small">
          O pedido entra na fila do coletor e fica registrado na auditoria com o seu usuário, mesmo que a coleta não
          chegue a rodar.
        </p>
        {error != null &&
          (error instanceof ApiError && error.code === 'conflict' ? (
            <div className="notice notice--error" role="alert">
              <p>{MSG_COLLECT_CONFLICT}</p>
            </div>
          ) : (
            <ErrorNotice error={error} />
          ))}
        <DialogActions onCancel={onClose} busy={busy} submitLabel="Pedir a coleta completa" />
      </form>
    </Dialog>
  );
}
