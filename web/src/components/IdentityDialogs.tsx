// Analyst merge and split from the entity page (POST /identity/merge and
// /identity/split; internal/identity/decisions.go Merge and Split).
import { useId, useRef, useState, type FormEvent } from 'react';
import { useQuery } from '@tanstack/react-query';
import { ApiError, apiGet, apiPost } from '../api/client';
import { formatClock } from '../api/format';
import {
  contextLabel,
  cpfLabel,
  MAX_RECORD_IDS,
  MSG_CHANGED,
  NOT_PROOF,
  REASON_MAX,
  reasonLength,
  useSourceNames,
  validReason,
} from '../api/identity';
import type { EntityResponse, PersonRecord, PossibleSame } from '../api/types';
import { Dialog, DialogActions } from './Dialog';
import { ErrorNotice } from './ErrorNotice';

function ReasonField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const id = useId();
  const hintId = useId();
  const n = reasonLength(value);
  return (
    <div className="reason-field">
      <label htmlFor={id}>Motivo (obrigatório)</label>
      <textarea
        id={id}
        data-autofocus
        rows={3}
        value={value}
        aria-describedby={hintId}
        aria-invalid={n > REASON_MAX}
        onChange={(e) => onChange(e.target.value)}
      />
      <p id={hintId} className="field__hint">
        Por que você decidiu assim (fica registrado na decisão). {n}/{REASON_MAX}
        {n > REASON_MAX ? ' — passou do limite' : ''}
      </p>
    </div>
  );
}

/**
 * Sends a decision once at a time; errors stay in the dialog. A 409 means the
 * records changed under the dialog (the server checks the expected state):
 * onConflict reloads them and the dialog stays open.
 */
function useSubmit(send: () => Promise<void>, onConflict: () => void) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [conflict, setConflict] = useState(false);
  const inFlight = useRef(false);
  const run = async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setError(null);
    setConflict(false);
    try {
      await send();
    } catch (err) {
      inFlight.current = false;
      setBusy(false);
      if (err instanceof ApiError && err.code === 'conflict') {
        setConflict(true);
        onConflict();
      } else setError(err);
    }
  };
  return { busy, error, conflict, run };
}

/**
 * An entity read afresh for a decision dialog: never from the page's cache,
 * dropped when the dialog closes. `data` is set only from a fetch that
 * succeeded after the dialog opened or after the last reload(): a failed
 * refetch (TanStack keeps the old data) never brings stale lists back.
 */
function useFreshEntity(id: number) {
  const [requestedAt, setRequestedAt] = useState(() => Date.now());
  const q = useQuery({
    queryKey: ['identity-fresh', 'entity', id],
    queryFn: () => apiGet<EntityResponse>(`/entities/${encodeURIComponent(id)}`),
    staleTime: 0,
    gcTime: 0,
    refetchOnMount: 'always',
  });
  const usable = q.isSuccess && !q.isFetching && q.dataUpdatedAt >= requestedAt;
  return {
    data: usable ? q.data : undefined,
    at: usable ? q.dataUpdatedAt : 0,
    loading: q.isFetching,
    error: q.isError && !q.isFetching ? q.error : null,
    reload: () => {
      setRequestedAt(Date.now());
      void q.refetch();
    },
  };
}

function ChangedNotice() {
  return (
    <div className="notice notice--error" role="alert">
      <p>{MSG_CHANGED}</p>
    </div>
  );
}

function LoadedAt({ at, loading }: { at: number; loading: boolean }) {
  return (
    <p className="muted small">
      {loading ? 'Carregando os dados atuais…' : at > 0 ? `Dados carregados às ${formatClock(at)}.` : ''}
    </p>
  );
}

function RecordsTable({ caption, records }: { caption: string; records: PersonRecord[] }) {
  const sourceName = useSourceNames();
  return (
    <div className="table-wrap">
      <table className="table">
        <caption>{caption}</caption>
        <thead>
          <tr>
            <th scope="col">Base</th>
            <th scope="col">Contexto</th>
            <th scope="col">CPF (mascarado)</th>
          </tr>
        </thead>
        <tbody>
          {records.map((r) => (
            <tr key={r.id}>
              <td>{sourceName(r.source_key)}</td>
              <td>{contextLabel(r.context_key)}</td>
              <td className="mono">{cpfLabel(r.cpf_masked)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function SplitDialog({
  entityId,
  entityName,
  recordId,
  onClose,
  onDone,
  fallbackFocus,
}: {
  entityId: number;
  entityName: string;
  recordId: number;
  onClose: () => void;
  onDone: (entityId: number) => void;
  fallbackFocus: () => HTMLElement | null;
}) {
  const [reason, setReason] = useState('');
  const fresh = useFreshEntity(entityId);
  const loading = fresh.loading;
  const data = fresh.data;
  const record = data?.records.find((r) => r.id === recordId);
  let blocked = '';
  if (data && !record) blocked = 'Esta ficha já não está nesta entidade. Nada foi alterado.';
  else if (data && data.records.length < 2) blocked = 'Esta entidade tem agora uma ficha só; não há o que separar.';

  const { busy, error, conflict, run } = useSubmit(
    async () => {
      const res = await apiPost<{ entity_id: number }>('/identity/split', {
        record_ids: [recordId],
        reason: reason.trim(),
        entity_id: entityId,
      });
      onDone(res.entity_id);
    },
    () => fresh.reload(),
  );
  const ok = !!record && !blocked && validReason(reason);
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (ok) void run();
  };
  return (
    <Dialog title="Separar ficha" onClose={onClose} busy={busy} fallbackFocus={fallbackFocus}>
      <form onSubmit={onSubmit}>
        <LoadedAt at={fresh.at} loading={loading} />
        {conflict && <ChangedNotice />}
        {fresh.error != null && <ErrorNotice error={fresh.error} onRetry={fresh.reload} />}
        {record && <RecordsTable caption={`Ficha que sai de ${data?.entity.name ?? entityName}`} records={[record]} />}
        {blocked && <p className="hint hint--warning">{blocked}</p>}
        <p>
          <strong>A ficha passa a ser uma pessoa separada e não será reunida automaticamente de novo.</strong> Ela
          forma uma entidade nova, e os vínculos que vêm desta ficha vão com ela.
        </p>
        <p className="small">
          Se alguma ficha que fica aqui tiver o mesmo nome e o mesmo trecho do CPF, essa combinação passa a ser
          tratada como ambígua: fichas que chegarem depois com essa combinação nunca são unidas automaticamente; podem
          ir para a fila de identidades — exceto se essa combinação já tiver sido confirmada ou rejeitada antes: nesse
          caso ficam separadas e não voltam para a fila. Você pode juntar de novo depois, em “Possível mesma pessoa”.
        </p>
        <ReasonField value={reason} onChange={setReason} />
        {error != null && <ErrorNotice error={error} />}
        <DialogActions onCancel={onClose} busy={busy} disabled={!ok || !data} submitLabel="Separar ficha" />
      </form>
    </Dialog>
  );
}

export function MergeDialog({
  entityId,
  candidate,
  onClose,
  onDone,
  fallbackFocus,
}: {
  entityId: number;
  candidate: PossibleSame;
  onClose: () => void;
  onDone: (entityId: number) => void;
  fallbackFocus: () => HTMLElement | null;
}) {
  const [reason, setReason] = useState('');
  // Both sides read afresh: the ids sent are exactly the records listed.
  const here = useFreshEntity(entityId);
  const other = useFreshEntity(candidate.entity_id);
  const loading = here.loading || other.loading;
  const a = here.data;
  const b = other.data;
  const mine = a?.records ?? [];
  const theirs = b?.records ?? [];
  const all = [...mine, ...theirs];
  const total = all.length;
  // Merge puts every record in the entity of the lowest-id one (decisions.go analystMerge).
  const lowest = all.reduce<PersonRecord | null>((m, r) => (m && m.id < r.id ? m : r), null);
  const loadedAt = a && b ? Math.max(here.at, other.at) : 0;

  let blocked = '';
  if (a && b) {
    if (a.entity.kind !== 'person' || b.entity.kind !== 'person')
      blocked = 'Uma das entidades não é uma pessoa; só pessoas podem ser juntadas.';
    else if (a.records_truncated || b.records_truncated)
      blocked = `Uma das entidades tem mais fichas do que a página mostra. Não é possível juntar pela Bancada pessoas com mais de ${MAX_RECORD_IDS} fichas no total.`;
    else if (total > MAX_RECORD_IDS)
      blocked = `Juntas, as duas entidades têm ${total} fichas. Não é possível juntar pela Bancada pessoas com mais de ${MAX_RECORD_IDS} fichas no total.`;
    else if (mine.length === 0 || theirs.length === 0) blocked = 'Uma das entidades não tem fichas para juntar.';
  }

  const { busy, error, conflict, run } = useSubmit(
    async () => {
      const res = await apiPost<{ entity_id: number }>('/identity/merge', {
        record_ids: all.map((r) => r.id),
        reason: reason.trim(),
        entity_ids: [entityId, candidate.entity_id],
      });
      onDone(res.entity_id);
    },
    () => {
      here.reload();
      other.reload();
    },
  );
  const ready = !!a && !!b && !blocked;
  const ok = ready && validReason(reason);
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (ok) void run();
  };

  return (
    <Dialog title="Juntar duas pessoas" onClose={onClose} busy={busy} fallbackFocus={fallbackFocus}>
      <form onSubmit={onSubmit}>
        <LoadedAt at={loadedAt} loading={loading} />
        {conflict && <ChangedNotice />}
        <p>
          <strong>O que as duas têm em comum: </strong>
          mesmo nome normalizado
          {candidate.shared_fragment ? '; mesmo trecho do CPF em fichas dos dois lados' : ''}.
        </p>
        <p className="muted small">{NOT_PROOF}</p>
        {here.error != null && <ErrorNotice error={here.error} onRetry={here.reload} />}
        {other.error != null && <ErrorNotice error={other.error} onRetry={other.reload} />}
        {a && <RecordsTable caption={`Esta entidade: ${a.entity.name} (nº ${a.entity.id})`} records={mine} />}
        {b && <RecordsTable caption={`Entidade candidata: ${b.entity.name} (nº ${b.entity.id})`} records={theirs} />}
        <p>
          <strong>
            As {total > 0 ? `${total} ` : ''}fichas listadas acima passam a ser tratadas como uma só pessoa em todos os
            mapas e relatórios. Você pode separar depois.
          </strong>
        </p>
        <p className="small">
          Elas vão para a entidade da ficha de menor número
          {lowest ? ` (hoje, nº ${lowest.entity_id})` : ''} e ficam fixadas por esta decisão: a união automática não
          as move mais. Fichas que chegarem depois com o nome e o trecho do CPF de alguma delas não são juntadas
          automaticamente a esta pessoa, porque a união automática nunca acrescenta fichas a uma entidade montada por
          um analista; elas ficam em entidade própria e podem aparecer em “Possível mesma pessoa”.
        </p>
        {blocked && <p className="hint hint--warning">{blocked}</p>}
        <ReasonField value={reason} onChange={setReason} />
        {error != null && <ErrorNotice error={error} />}
        <DialogActions onCancel={onClose} busy={busy} disabled={!ok || !a || !b} submitLabel="Juntar as fichas" />
      </form>
    </Dialog>
  );
}
