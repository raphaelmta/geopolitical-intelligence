import { useEffect, useId, useRef, useState, type FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router';
import { useInfiniteQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError, apiGet, apiPost } from '../api/client';
import { formatClock } from '../api/format';
import { contextLabel, cpfLabel, MSG_CHANGED, NOT_PROOF, proposalReason, useSourceNames } from '../api/identity';
import type { PersonRecord, Proposal, ProposalStatus, ProposalsPage } from '../api/types';
import { Dialog, DialogActions } from '../components/Dialog';
import { ErrorNotice } from '../components/ErrorNotice';
import { usePageHeading } from '../usePageHeading';

const STATUSES: [ProposalStatus, string, string][] = [
  ['pending', 'Pendentes', 'pendente'],
  ['confirmed', 'Confirmadas', 'confirmada'],
  ['rejected', 'Rejeitadas', 'rejeitada'],
  ['obsolete', 'Obsoletas', 'obsoleta'],
];

function parseStatus(v: string | null): ProposalStatus {
  return STATUSES.some(([s]) => s === v) ? (v as ProposalStatus) : 'pending';
}

type Decision = { proposal: Proposal; confirm: boolean };

/**
 * What Decide acts on: records not locked by a decision and linked to an
 * entity (decisions.go DecideExpecting). The API's `free` flag when sent;
 * otherwise the same rule from locked/entity_id.
 */
export function isFree(r: PersonRecord): boolean {
  return r.free ?? (!r.locked && r.entity_id !== 0);
}

export function freeRecords(p: Proposal): PersonRecord[] {
  return p.records.filter(isFree);
}

/** Why a record of a proposal will not be moved. */
function notFreeLabel(r: PersonRecord): string {
  return r.entity_id === 0 && !r.locked
    ? 'sem entidade no momento (não será movida)'
    : 'fixada por outra decisão (não será movida)';
}

/**
 * Confirming needs two free records and a target (DecideExpecting: fewer than
 * two free → ErrProposalStale; the target is the entity of the lowest free
 * record). Otherwise the server can only answer 409.
 */
export function canConfirm(p: Proposal): boolean {
  return freeRecords(p).length >= 2 && p.target_entity.entity != null;
}

export const MSG_NOTHING_TO_CONFIRM =
  'Não há o que confirmar agora: restam menos de duas fichas livres nesta proposta (as demais já foram fixadas por outra decisão ou não estão ligadas a uma entidade). Ela será atualizada na próxima resolução de identidades.';

function sameDecisionView(a: Proposal, b: Proposal): boolean {
  const ids = (xs: { id: number }[]) => xs.map((r) => r.id).sort((x, y) => x - y).join(',');
  return (
    ids(freeRecords(a)) === ids(freeRecords(b)) &&
    (a.target_entity.entity?.id ?? null) === (b.target_entity.entity?.id ?? null) &&
    ids(a.target_entity.other_records) === ids(b.target_entity.other_records) &&
    a.target_entity.other_records_truncated === b.target_entity.other_records_truncated
  );
}


/**
 * What happens to future records with this key after a confirmation
 * (resolver.go "actions": confirmed → skip when target IS NULL OR impure OR
 * ambiguous; identity_ambiguous_key is never deleted).
 */
function futureAfterConfirm(code: string | undefined): string {
  if (code === 'ambiguous_key') {
    return 'Esta combinação de nome e trecho do CPF já foi separada por um analista antes; por isso, mesmo depois desta confirmação, fichas novas com ela ficam separadas e não voltam para a fila.';
  }
  if (code === 'common_name' || code === 'impure_target') {
    return 'Fichas novas com este mesmo nome e trecho do CPF passam a ir para essa entidade, enquanto ela só reunir fichas desta combinação. Se a entidade de destino reunir fichas de outra combinação de nome e trecho do CPF, fichas novas com esta combinação ficam separadas e não voltam para a fila.';
  }
  return 'Fichas novas com esta combinação de nome e trecho do CPF podem ficar separadas e não voltar para a fila.';
}

export function IdentityQueuePage() {
  const [params, setParams] = useSearchParams();
  const status = parseStatus(params.get('status'));
  const statusId = useId();
  const h1 = usePageHeading('Fila de identidades');
  const sourceName = useSourceNames();
  const queryClient = useQueryClient();
  // Decided here, per list: a proposal confirmed under "Pendentes" still shows under "Confirmadas".
  const [removed, setRemoved] = useState<ReadonlySet<string>>(new Set());
  const [notice, setNotice] = useState<string | null>(null);
  const [deciding, setDeciding] = useState<Decision | null>(null);
  const noticeRef = useRef<HTMLParagraphElement>(null);
  const listHeadRef = useRef<HTMLParagraphElement>(null);

  const q = useInfiniteQuery({
    queryKey: ['proposals', status],
    queryFn: ({ pageParam }) => {
      const p = new URLSearchParams({ status });
      if (pageParam) p.set('cursor', pageParam);
      return apiGet<ProposalsPage>(`/identity/proposals?${p}`);
    },
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor || undefined,
  });

  // Pages are appended; a proposal seen on an earlier page is not repeated.
  const seen = new Set<number>();
  const items = (q.data?.pages.flatMap((p) => p.items) ?? []).filter(
    (p) => !removed.has(`${status}:${p.id}`) && !seen.has(p.id) && Boolean(seen.add(p.id)),
  );

  const changeStatus = (s: string) => {
    setNotice(null);
    setParams(s === 'pending' ? {} : { status: s }, { replace: true });
  };

  /** Reloads the loaded pages and returns the proposal as it is now (null: no longer pending here). */
  const reload = async (id: number): Promise<{ proposal: Proposal | null; at: number }> => {
    const r = await q.refetch({ throwOnError: true });
    const fresh = (r.data?.pages.flatMap((p) => p.items) ?? []).find((x) => x.id === id && x.status === 'pending');
    return { proposal: fresh ?? null, at: r.dataUpdatedAt };
  };

  const decided = (d: Decision) => {
    setRemoved((prev) => new Set(prev).add(`pending:${d.proposal.id}`));
    setNotice(
      d.confirm
        ? `Proposta confirmada: as fichas de ${d.proposal.name} foram unidas.`
        : `Proposta rejeitada: as fichas de ${d.proposal.name} ficam como pessoas diferentes.`,
    );
    setDeciding(null);
    // Records moved between entities: other proposals may have another target
    // now, and entity pages must reload.
    void queryClient.invalidateQueries({ queryKey: ['proposals'] });
    void queryClient.invalidateQueries({ queryKey: ['entity'] });
    void queryClient.invalidateQueries({ queryKey: ['relationships'] });
  };

  const statusLabel = STATUSES.find(([s]) => s === status)![2];

  return (
    <>
      <h1 ref={h1} tabIndex={-1}>
        Fila de identidades
      </h1>
      <div className="lede">
        <p>
          A plataforma nunca junta pessoas só pelo nome. Quando fichas de bases diferentes têm o mesmo nome e o mesmo
          trecho do CPF, mas ela não tem segurança para uni-las sozinha, a decisão vem para cá. Uma junção errada
          contamina todos os mapas e relatórios; na dúvida, rejeite.
        </p>
        <p className="muted small">
          A fila mostra primeiro as propostas com mais vínculos. Cada decisão fica registrada com o seu usuário e pode
          ser desfeita depois na página da entidade (separar o que foi juntado, juntar o que foi separado).
        </p>
      </div>

      <div className="filter">
        <label htmlFor={statusId}>Situação</label>
        <select id={statusId} value={status} onChange={(e) => changeStatus(e.target.value)}>
          {STATUSES.map(([s, label]) => (
            <option key={s} value={s}>
              {label}
            </option>
          ))}
        </select>
      </div>

      {notice && (
        <p ref={noticeRef} role="status" tabIndex={-1} className="notice">
          {notice}
        </p>
      )}

      {q.isPending && <p role="status">Carregando a fila…</p>}
      {q.isError && items.length === 0 && <ErrorNotice error={q.error} onRetry={() => void q.refetch()} />}
      {q.isSuccess && items.length === 0 && (
        <p className="empty">{status === 'pending' ? 'Nenhuma proposta pendente.' : `Nenhuma proposta ${statusLabel}.`}</p>
      )}

      {items.length > 0 && (
        <>
          <p ref={listHeadRef} tabIndex={-1} className="muted small">
            {items.length} {items.length === 1 ? 'carregada' : 'carregadas'}
            {q.hasNextPage ? ' · há mais' : ''}
          </p>
          <ol className="proposals">
            {items.map((p) => (
              <li key={p.id}>
                <ProposalItem proposal={p} sourceName={sourceName} onDecide={(confirm) => setDeciding({ proposal: p, confirm })} />
              </li>
            ))}
          </ol>
        </>
      )}
      {q.isError && items.length > 0 && <ErrorNotice error={q.error} onRetry={() => void q.fetchNextPage()} />}
      {q.hasNextPage && (
        <button type="button" className="button" disabled={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()}>
          {q.isFetchingNextPage ? 'Carregando…' : 'Carregar mais'}
        </button>
      )}

      {deciding && (
        <DecisionDialog
          decision={deciding}
          reload={reload}
          onClose={() => setDeciding(null)}
          onDone={decided}
          fallbackFocus={() => noticeRef.current ?? listHeadRef.current ?? h1.current}
        />
      )}
    </>
  );
}

function ProposalItem({
  proposal: p,
  sourceName,
  onDecide,
}: {
  proposal: Proposal;
  sourceName: (key: string) => string;
  onDecide: (confirm: boolean) => void;
}) {
  const h = useId();
  const targetId = useId();
  const target = p.target_entity;
  const fragments = new Set(p.records.map((r) => r.cpf_masked ?? ''));
  const sameFragment = p.records.length > 1 && !fragments.has('') && fragments.size === 1;
  const statusLabel = STATUSES.find(([s]) => s === p.status)?.[2] ?? p.status;

  return (
    <article aria-labelledby={h} className="proposal">
      <header className="proposal__head">
        <h2 id={h}>{p.name}</h2>
        <p className="proposal__meta">
          <span className="proposal__count">
            {p.connections} {p.connections === 1 ? 'vínculo' : 'vínculos'}
          </span>{' '}
          <span className="muted small">citam estas fichas</span>
          {p.status !== 'pending' && <span className="tag">{statusLabel}</span>}
        </p>
      </header>

      <p>
        <strong>Por que está na fila: </strong>
        {proposalReason(p.reason_code)}
      </p>
      <p>
        <strong>O que as fichas têm em comum: </strong>
        mesmo nome normalizado
        {sameFragment ? '; mesmo trecho do CPF (os 6 dígitos visíveis são iguais)' : ''}.
      </p>
      <p className="muted small">{NOT_PROOF}</p>

      <div className="table-wrap">
        <table className="table">
          <caption>Fichas propostas ({p.records.length})</caption>
          <thead>
            <tr>
              <th scope="col">Base</th>
              <th scope="col">Contexto</th>
              <th scope="col">Nome na ficha</th>
              <th scope="col">CPF (mascarado)</th>
              <th scope="col">Entidade atual</th>
            </tr>
          </thead>
          <tbody>
            {p.records.map((r) => (
              <tr key={r.id}>
                <td>{sourceName(r.source_key)}</td>
                <td>{contextLabel(r.context_key)}</td>
                <td>{r.name}</td>
                <td className="mono">{cpfLabel(r.cpf_masked)}</td>
                <td>
                  {r.entity_id ? (
                    <Link to={`/entidade/${encodeURIComponent(r.entity_id)}`}>entidade nº {r.entity_id}</Link>
                  ) : (
                    <span className="muted">sem entidade</span>
                  )}
                  {target.entity && r.entity_id === target.entity.id && (
                    <span className="muted small"> · já está no destino</span>
                  )}
                  {p.status === 'pending' && !isFree(r) && <span className="muted small"> · {notFreeLabel(r)}</span>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {p.status === 'pending' ? (
        <TargetBlock proposal={p} sourceName={sourceName} headingId={targetId} />
      ) : (
        <p className="proposal__decided">
          <strong>Decisão: </strong>
          {DECIDED[p.status] ?? p.status}
        </p>
      )}

      {p.status === 'pending' && !canConfirm(p) && <p className="hint hint--warning">{MSG_NOTHING_TO_CONFIRM}</p>}
      {p.status === 'pending' && (
        <div className="actions proposal__actions">
          {canConfirm(p) && (
            <button type="button" className="button" aria-describedby={h} onClick={() => onDecide(true)}>
              Confirmar — são a mesma pessoa
            </button>
          )}
          <button type="button" className="button" aria-describedby={h} onClick={() => onDecide(false)}>
            Rejeitar — são pessoas diferentes
          </button>
        </div>
      )}
    </article>
  );
}

/** What a decided proposal's status means (decisions.go Decide; resolver.go "obsolete proposals"). */
const DECIDED: Record<string, string> = {
  confirmed: 'confirmada — as fichas foram unidas por um analista.',
  rejected: 'rejeitada — as fichas ficam como pessoas diferentes.',
  obsolete:
    'obsoleta — não havia mais o que decidir (as fichas já tinham sido unidas, ou restou menos de duas fichas livres).',
};

function TargetBlock({
  proposal: p,
  sourceName,
  headingId,
}: {
  proposal: Proposal;
  sourceName: (key: string) => string;
  headingId: string;
}) {
  const target = p.target_entity;
  return (
    <section aria-labelledby={headingId} className="proposal__target">
      <h3 id={headingId}>Entidade de destino</h3>
      {target.entity ? (
        <>
          <p>
            Confirmar une as fichas a{' '}
            <Link to={`/entidade/${encodeURIComponent(target.entity.id)}`}>
              {target.entity.name} (nº {target.entity.id})
            </Link>
            .
          </p>
          {target.other_records.length === 0 ? (
            <p className="muted small">Ela não tem nenhuma outra ficha além das desta proposta.</p>
          ) : (
            <>
              <p className="hint hint--warning">{othersWarning(target.other_records.length, target.other_records_truncated)}</p>
              <div className="table-wrap">
                <table className="table">
                  <caption>Outras fichas que a entidade de destino já reúne</caption>
                  <thead>
                    <tr>
                      <th scope="col">Base</th>
                      <th scope="col">Contexto</th>
                    </tr>
                  </thead>
                  <tbody>
                    {target.other_records.map((o) => (
                      <tr key={o.id}>
                        <td>{sourceName(o.source_key)}</td>
                        <td>{contextLabel(o.context_key)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {target.other_records_truncated && (
                <p className="muted small">Lista cortada: abra a entidade para ver todas as fichas.</p>
              )}
            </>
          )}
        </>
      ) : (
        <p className="muted">Nenhuma ficha desta proposta está ligada a uma entidade agora.</p>
      )}
    </section>
  );
}

function othersWarning(n: number, truncated: boolean): string {
  if (truncated) return `Confirmar une estas fichas a uma entidade que já reúne mais de ${n} outras fichas.`;
  if (n === 1) return 'Confirmar une estas fichas a uma entidade que já reúne outra ficha.';
  return `Confirmar une estas fichas a uma entidade que já reúne outras ${n} fichas.`;
}

function DecisionDialog({
  decision,
  reload,
  onClose,
  onDone,
  fallbackFocus,
}: {
  decision: Decision;
  reload: (id: number) => Promise<{ proposal: Proposal | null; at: number }>;
  onClose: () => void;
  onDone: (d: Decision) => void;
  fallbackFocus: () => HTMLElement | null;
}) {
  const { confirm } = decision;
  const sourceName = useSourceNames();
  // What the dialog shows, and therefore what the request asserts.
  const [view, setView] = useState<Proposal>(decision.proposal);
  const [loadedAt, setLoadedAt] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [gone, setGone] = useState(false);
  const [changed, setChanged] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const inFlight = useRef(false);
  const viewRef = useRef(view);
  viewRef.current = view;

  // Never decide on a view of unknown age: reload before the action is enabled.
  const load = async () => {
    setLoading(true);
    setLoadError(null);
    setLoadedAt(null);
    try {
      const { proposal, at } = await reload(decision.proposal.id);
      // Reset on every reload: a proposal that reappears is actionable again.
      setGone(!proposal);
      if (proposal) {
        if (!sameDecisionView(viewRef.current, proposal)) setChanged(true);
        setView(proposal);
      }
      setLoadedAt(at);
    } catch (err) {
      setLoadError(err);
    } finally {
      setLoading(false);
    }
  };
  const started = useRef(false);
  useEffect(() => {
    if (started.current) return;
    started.current = true;
    void load();
  }, []);

  const free = freeRecords(view);
  const notFree = view.records.filter((r) => !isFree(r));
  const target = view.target_entity;
  const others = target.other_records.length;
  // Content and action come only from a reload that succeeded after the dialog
  // opened (or after the last 409/retry).
  const fresh = !loading && loadError == null && loadedAt != null && !gone;
  const impossible = confirm && !canConfirm(view);

  const send = async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setError(null);
    setConflict(false);
    setChanged(false);
    try {
      await apiPost<void>(`/identity/proposals/${encodeURIComponent(view.id)}/decision`, {
        confirm,
        expected_target_entity_id: target.entity?.id ?? null,
        expected_record_ids: free.map((r) => r.id),
      });
      onDone({ proposal: view, confirm });
    } catch (err) {
      inFlight.current = false;
      setBusy(false);
      if (err instanceof ApiError && err.code === 'conflict') {
        setConflict(true);
        await load();
      } else setError(err);
    }
  };
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (fresh && !impossible) void send();
  };

  const title = confirm ? 'Confirmar que são a mesma pessoa' : 'Rejeitar: são pessoas diferentes';
  return (
    <Dialog title={title} onClose={onClose} busy={busy} fallbackFocus={fallbackFocus}>
      <form onSubmit={onSubmit}>
        <p className="muted small">
          {view.name} ·{' '}
          {loading ? 'conferindo se a proposta mudou…' : loadedAt ? `Dados carregados às ${formatClock(loadedAt)}.` : ''}
        </p>
        {conflict && (
          <div className="notice notice--error" role="alert">
            <p>{MSG_CHANGED}</p>
          </div>
        )}
        {changed && !conflict && (
          <p className="hint hint--warning" role="alert">
            Esta proposta mudou; confira de novo.
          </p>
        )}
        {gone && (
          <div className="hint hint--warning" role="alert">
            <p>Esta proposta não está mais entre as pendentes carregadas (foi decidida ou mudou). Nada foi alterado.</p>
            <button type="button" className="button" disabled={loading} onClick={() => void load()}>
              Conferir de novo
            </button>
          </div>
        )}
        {loadError != null && <ErrorNotice error={loadError} onRetry={() => void load()} />}

        {fresh && impossible && <p className="hint hint--warning">{MSG_NOTHING_TO_CONFIRM}</p>}
        {fresh && (
        <>
        <div className="table-wrap">
          <table className="table">
            <caption>
              {confirm ? 'Fichas que serão unidas' : 'Fichas desta decisão'} ({free.length})
            </caption>
            <thead>
              <tr>
                <th scope="col">Base</th>
                <th scope="col">Contexto</th>
                <th scope="col">CPF (mascarado)</th>
              </tr>
            </thead>
            <tbody>
              {free.map((r) => (
                <tr key={r.id}>
                  <td>{sourceName(r.source_key)}</td>
                  <td>{contextLabel(r.context_key)}</td>
                  <td className="mono">{cpfLabel(r.cpf_masked)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {notFree.length > 0 && (
          <ul className="plain-list muted small">
            {notFree.map((r) => (
              <li key={r.id}>
                {sourceName(r.source_key)} · {contextLabel(r.context_key)}: {notFreeLabel(r)}
              </li>
            ))}
          </ul>
        )}

        {confirm ? (
          <>
            <p>
              As fichas listadas acima serão unidas na entidade de destino
              {target.entity ? ` ${target.entity.name} (nº ${target.entity.id})` : ''} e passam a ser tratadas como uma
              só pessoa em todos os mapas e relatórios. Você pode separar depois, na página da entidade.
            </p>
            {others > 0 && (
              <p>
                <strong>
                  Essa entidade já reúne {target.other_records_truncated ? `mais de ${others}` : others}{' '}
                  {others === 1 ? 'outra ficha' : 'outras fichas'}; elas continuam lá, junto com as novas.
                </strong>
              </p>
            )}
            <p className="small">
              As fichas ficam fixadas por esta decisão: a união automática não as move mais. {futureAfterConfirm(view.reason_code)}
            </p>
          </>
        ) : (
          <>
            <p>
              A plataforma passa a tratar estas fichas como pessoas diferentes. Esta combinação de nome e trecho do CPF
              nunca mais será unida automaticamente nem proposta de novo — nem com fichas que chegarem depois.
            </p>
            {free.length > 0 ? (
              <p className="small">
                Se a união automática já tinha posto algumas destas fichas na mesma entidade, elas são separadas: cada
                uma vai para uma entidade própria. As fichas ficam fixadas por esta decisão. Você pode juntar depois, na
                página da entidade, se mudar de ideia.
              </p>
            ) : (
              <p className="small">
                Nenhuma ficha é movida; a combinação de nome e trecho do CPF fica rejeitada e não será proposta de novo.
              </p>
            )}
          </>
        )}
        </>
        )}
        <p className="muted small">{NOT_PROOF}</p>
        {error != null && <ErrorNotice error={error} onRetry={() => void send()} />}
        <DialogActions
          onCancel={onClose}
          busy={busy}
          disabled={!fresh || impossible}
          submitLabel={confirm ? 'Confirmar: são a mesma pessoa' : 'Rejeitar: são pessoas diferentes'}
        />
      </form>
    </Dialog>
  );
}
