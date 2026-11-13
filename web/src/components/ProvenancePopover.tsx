import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { useQuery } from '@tanstack/react-query';
import { apiGet } from '../api/client';
import type { CollectionDates, Edge, Provenance } from '../api/types';
import { formatBytes, formatDate, formatTimestamp } from '../api/format';
import { EdgeBadge } from './EdgeBadge';
import { ErrorNotice } from './ErrorNotice';

const FOCUSABLE = 'a[href], button:not([disabled]), input, select, textarea, [tabindex]:not([tabindex="-1"])';

/** What the dialog repeats of the edge it was opened from (the provenance route does not carry it). */
type EdgeIdentity = Partial<Pick<Edge, 'identity_basis' | 'identity_confidence'>>;

/**
 * A "fonte" button that opens where a relationship came from: base, record,
 * link to the source, the first and the last collection that saw it,
 * staleness and the preserved raw file. `edge` (the row's edge) adds its
 * identity basis to the badge.
 * A native modal <dialog> (top layer: never clipped by a scrolling table;
 * Esc and the backdrop close it). Fetches /relationships/{id}/provenance only
 * when opened. Focus goes into the dialog, Tab cycles inside it, and on close
 * it returns to the button.
 */
export function ProvenancePopover({
  relationshipId,
  label,
  edge,
}: {
  relationshipId: number;
  label?: string;
  edge?: EdgeIdentity;
}) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  useEffect(() => {
    const d = dialogRef.current;
    if (open && d && !d.open) {
      d.showModal();
      d.focus();
    }
  }, [open]);

  // Every way of closing (Esc, "Fechar", backdrop) ends in the "close" event.
  const onClose = () => {
    setOpen(false);
    triggerRef.current?.focus();
  };
  const close = () => dialogRef.current?.close();

  const onKeyDown = (e: KeyboardEvent<HTMLDialogElement>) => {
    if (e.key !== 'Tab' || !dialogRef.current) return;
    const items = Array.from(dialogRef.current.querySelectorAll<HTMLElement>(FOCUSABLE));
    e.preventDefault();
    if (items.length === 0) return;
    const i = items.indexOf(document.activeElement as HTMLElement);
    const next = e.shiftKey ? (i <= 0 ? items.length - 1 : i - 1) : (i + 1) % items.length;
    items[next]!.focus();
  };

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        className="link-button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={label}
        onClick={() => setOpen(true)}
      >
        fonte
      </button>
      {open && (
        <dialog
          ref={dialogRef}
          aria-labelledby={titleId}
          tabIndex={-1}
          className="provenance-dialog"
          onClose={onClose}
          onKeyDown={onKeyDown}
          onClick={(e) => {
            // A click on the dialog box itself (not its content) is the backdrop.
            if (e.target === e.currentTarget) close();
          }}
        >
          <div className="provenance-dialog__body">
            <div className="popover__head">
              <h3 id={titleId}>Fonte do vínculo nº {relationshipId}</h3>
              <button type="button" className="link-button" onClick={close}>
                Fechar
              </button>
            </div>
            <ProvenanceBody id={relationshipId} identity={edge} />
          </div>
        </dialog>
      )}
    </>
  );
}

function ProvenanceBody({ id, identity }: { id: number; identity?: EdgeIdentity }) {
  const firstId = useId();
  const lastId = useId();
  const q = useQuery({
    queryKey: ['provenance', id],
    queryFn: () => apiGet<Provenance>(`/relationships/${encodeURIComponent(id)}/provenance`),
    staleTime: 60_000,
  });
  if (q.isPending) return <p role="status">Carregando a fonte…</p>;
  if (q.isError) return <ErrorNotice error={q.error} onRetry={() => void q.refetch()} />;
  const p = q.data;
  const c = p.collection;
  const last = p.last_seen_collection;
  return (
    <>
      <dl className="facts">
        <dt>Natureza</dt>
        <dd>
          <EdgeBadge
            edge={{
              kind: p.kind,
              confidence: p.confidence,
              stale: c.stale,
              identity_basis: identity?.identity_basis ?? 'none',
              identity_confidence: identity?.identity_confidence,
            }}
          />
        </dd>
        <dt>Base</dt>
        <dd>{p.source.name}</dd>
        <dt>Registro</dt>
        <dd>
          <code>{p.record_ref}</code>
        </dd>
        <dt>Na fonte</dt>
        <dd>
          <SourceLink url={p.source_url ?? null} />
          {p.source_note && <p className="small provenance-note">{p.source_note}</p>}
        </dd>
        <dt id={firstId}>Primeira coleta que registrou este vínculo</dt>
        <dd role="group" aria-labelledby={firstId}>
          <CollectionFacts dates={c} extra={c.full_scope ? 'completa' : 'recorte'} />
        </dd>
        <dt id={lastId}>Última coleta que o confirmou</dt>
        <dd role="group" aria-labelledby={lastId}>
          {last ? <CollectionFacts dates={last} /> : <span className="muted">não informado</span>}
        </dd>
        {p.closed_collection_id != null && (
          <>
            <dt>Encerramento</dt>
            <dd>vínculo encerrado na coleta nº {p.closed_collection_id}</dd>
          </>
        )}
        <dt>Arquivo bruto</dt>
        <dd>
          {p.raw_key ? (
            <>
              <code className="break">{p.raw_key}</code>
              {p.raw_size != null && <span className="muted"> · {formatBytes(p.raw_size)}</span>}
            </>
          ) : (
            <span className="muted">arquivo bruto não registrado (coleta anterior ao registro dos arquivos)</span>
          )}
        </dd>
        {p.raw_sha256 && (
          <>
            <dt>SHA-256</dt>
            <dd>
              <Hash value={p.raw_sha256} />
            </dd>
          </>
        )}
      </dl>
      {p.raw_key && (
        <p className="muted small provenance-note">
          O arquivo original fica guardado pela plataforma; a impressão digital (SHA-256) permite conferir um novo
          download da fonte.
        </p>
      )}
    </>
  );
}

/** Only absolute http(s) URLs become links; anything else is shown as text. */
export function linkableUrl(url: string): boolean {
  if (!/^https?:\/\//i.test(url)) return false;
  try {
    const u = new URL(url);
    return u.protocol === 'http:' || u.protocol === 'https:';
  } catch {
    return false;
  }
}

/** "Abrir na fonte" (new tab, no opener or referrer) plus the URL as copyable text. */
function SourceLink({ url }: { url: string | null }) {
  if (!url) return <span className="muted">endereço de origem não registrado para este arquivo</span>;
  if (!linkableUrl(url)) return <code className="break">{url}</code>;
  return (
    <>
      <a href={url} target="_blank" rel="noopener noreferrer">
        Abrir na fonte
      </a>
      <br />
      <code className="break">{url}</code>
    </>
  );
}

/** Reference date, observed/publication date (when different) and the collection's end. */
function CollectionFacts({ dates: d, extra }: { dates: CollectionDates | Provenance['collection']; extra?: string }) {
  const observed = d.observed_date && d.observed_date !== d.reference_date ? d.observed_date : null;
  return (
    <dl className="facts facts--nested">
      <dt>Coleta</dt>
      <dd>
        nº {d.id}
        {extra && ` · ${extra}`}
      </dd>
      <dt>Data de referência</dt>
      <dd>{formatDate(d.reference_date)}</dd>
      {observed && (
        <>
          <dt>Publicada ou observada em</dt>
          <dd>{formatDate(observed)}</dd>
        </>
      )}
      <dt>Coletada em</dt>
      <dd>{formatTimestamp(d.collected_at) || 'não registrada'}</dd>
    </dl>
  );
}

function Hash({ value }: { value: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const short = value.length > 20 ? `${value.slice(0, 12)}…${value.slice(-8)}` : value;
  const copy = () => {
    const clip = navigator.clipboard;
    if (!clip) {
      setState('failed');
      return;
    }
    clip.writeText(value).then(
      () => setState('copied'),
      () => setState('failed'),
    );
  };
  return (
    <>
      <code title={value}>{short}</code>{' '}
      <button type="button" className="link-button" aria-label="Copiar SHA-256 completo" onClick={copy}>
        copiar
      </button>
      <span role="status" className="muted">
        {state === 'copied' ? ' copiado' : ''}
      </span>
      {state === 'failed' && <code className="break hash-full">{value}</code>}
    </>
  );
}
