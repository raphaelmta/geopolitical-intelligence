import { useEffect, useId, useRef, type KeyboardEvent, type ReactNode } from 'react';

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/**
 * A native modal <dialog>, shown while mounted. Focus goes to the element
 * marked data-autofocus (or the dialog itself), Tab cycles inside, and when
 * the dialog goes away focus returns to the element that opened it — or, if
 * that element is gone (the item it acted on was removed) or disabled, to
 * fallbackFocus.
 * Esc, the backdrop and "Cancelar" call onClose; none of them work while
 * busy (a request is in flight).
 */
export function Dialog({
  title,
  onClose,
  busy = false,
  fallbackFocus,
  children,
}: {
  title: string;
  onClose: () => void;
  busy?: boolean;
  fallbackFocus?: () => HTMLElement | null | undefined;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  const busyRef = useRef(busy);
  busyRef.current = busy;
  const fallbackRef = useRef(fallbackFocus);
  fallbackRef.current = fallbackFocus;

  useEffect(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const d = ref.current;
    if (d && !d.open) {
      d.showModal();
      const first = d.querySelector<HTMLElement>('[data-autofocus]');
      (first ?? d).focus();
    }
    return () => {
      // Runs after the dialog left the DOM: opener.isConnected is accurate.
      if (opener?.isConnected && !opener.matches(':disabled')) opener.focus();
      else fallbackRef.current?.()?.focus();
    };
  }, []);

  const onKeyDown = (e: KeyboardEvent<HTMLDialogElement>) => {
    if (e.key !== 'Tab' || !ref.current) return;
    const items = Array.from(ref.current.querySelectorAll<HTMLElement>(FOCUSABLE));
    e.preventDefault();
    if (items.length === 0) return;
    const i = items.indexOf(document.activeElement as HTMLElement);
    const next = e.shiftKey ? (i <= 0 ? items.length - 1 : i - 1) : (i + 1) % items.length;
    items[next]!.focus();
  };

  return (
    <dialog
      ref={ref}
      aria-labelledby={titleId}
      tabIndex={-1}
      className="modal"
      onKeyDown={onKeyDown}
      onCancel={(e) => {
        // Esc: never native-close (the parent unmounts us), never while busy.
        e.preventDefault();
        if (!busyRef.current) onClose();
      }}
      onClose={() => {
        // A browser may force-close (repeated Esc); while busy, keep it shown
        // so the answer (or error) is seen.
        if (!busyRef.current) onClose();
        else if (ref.current && !ref.current.open) ref.current.showModal();
      }}
      onClick={(e) => {
        // A click on the dialog box itself (not its content) is the backdrop.
        if (e.target === e.currentTarget && !busyRef.current) onClose();
      }}
    >
      <div className="modal__body">
        <h2 id={titleId} className="modal__title">
          {title}
        </h2>
        {children}
      </div>
    </dialog>
  );
}

/** The two buttons at the foot of a decision dialog: cancel, then act. */
export function DialogActions({
  onCancel,
  busy,
  disabled = false,
  submitLabel,
}: {
  onCancel: () => void;
  busy: boolean;
  disabled?: boolean;
  submitLabel: string;
}) {
  return (
    <>
      <div className="actions modal__actions">
        <button type="button" className="button" onClick={onCancel} disabled={busy}>
          Cancelar
        </button>
        <button type="submit" className="button button--primary" disabled={busy || disabled}>
          {submitLabel}
        </button>
      </div>
      <p className="muted small modal__progress" aria-live="polite">
        {busy ? 'Enviando… aguarde a resposta do servidor.' : ''}
      </p>
    </>
  );
}
