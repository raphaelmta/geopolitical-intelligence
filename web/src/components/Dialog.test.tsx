import { describe, expect, test, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { Dialog, DialogActions } from './Dialog';

function Harness({ busy = false, onClose = vi.fn() }: { busy?: boolean; onClose?: () => void }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" onClick={() => setOpen(true)}>
        Abrir
      </button>
      {open && (
        <Dialog
          title="Decidir"
          busy={busy}
          onClose={() => {
            onClose();
            setOpen(false);
          }}
        >
          <form>
            <textarea aria-label="Motivo" data-autofocus />
            <DialogActions onCancel={() => setOpen(false)} busy={busy} submitLabel="Enviar" />
          </form>
        </Dialog>
      )}
    </>
  );
}

describe('Dialog', () => {
  test('foco entra no campo marcado, Tab circula dentro e volta ao botão que abriu', async () => {
    render(<Harness />);
    const opener = screen.getByRole('button', { name: 'Abrir' });
    await userEvent.click(opener);
    const dialog = screen.getByRole('dialog', { name: 'Decidir' });
    const field = screen.getByLabelText('Motivo');
    expect(document.activeElement).toBe(field);
    await userEvent.tab();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Cancelar' }));
    await userEvent.tab();
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Enviar' }));
    await userEvent.tab();
    expect(document.activeElement).toBe(field);
    await userEvent.tab({ shift: true });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Enviar' }));
    expect(dialog.contains(document.activeElement)).toBe(true);
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  test('enquanto envia, Esc não fecha e os botões ficam desativados', async () => {
    const onClose = vi.fn();
    render(<Harness busy onClose={onClose} />);
    await userEvent.click(screen.getByRole('button', { name: 'Abrir' }));
    await userEvent.keyboard('{Escape}');
    expect(screen.getByRole('dialog')).toBeTruthy();
    expect(onClose).not.toHaveBeenCalled();
    expect((screen.getByRole('button', { name: 'Cancelar' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Enviar' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/Enviando/)).toBeTruthy();
  });
});
