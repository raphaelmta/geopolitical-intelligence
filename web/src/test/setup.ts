import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

// jsdom has HTMLDialogElement but not showModal/close. A minimal stand-in
// for tests only (never in the bundle): open/close toggle the attribute and
// fire "close"; Esc on an open modal fires "cancel" and then closes it, as
// browsers do.
const proto = HTMLDialogElement.prototype as HTMLDialogElement & { showModal?: () => void };
if (typeof proto.showModal !== 'function') {
  const modals: HTMLDialogElement[] = [];
  proto.showModal = function showModal(this: HTMLDialogElement) {
    if (this.hasAttribute('open')) throw new DOMException('already open', 'InvalidStateError');
    this.setAttribute('open', '');
    modals.push(this);
  };
  proto.show = function show(this: HTMLDialogElement) {
    this.setAttribute('open', '');
  };
  proto.close = function close(this: HTMLDialogElement) {
    if (!this.hasAttribute('open')) return;
    this.removeAttribute('open');
    const i = modals.indexOf(this);
    if (i >= 0) modals.splice(i, 1);
    this.dispatchEvent(new Event('close'));
  };
  Object.defineProperty(proto, 'open', {
    configurable: true,
    get(this: HTMLDialogElement) {
      return this.hasAttribute('open');
    },
  });
  document.addEventListener('keydown', (e) => {
    const top = modals.filter((m) => m.isConnected).at(-1);
    if (e.key !== 'Escape' || !top) return;
    if (top.dispatchEvent(new Event('cancel', { cancelable: true }))) top.close();
  });
}

afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
  window.localStorage.clear();
});
