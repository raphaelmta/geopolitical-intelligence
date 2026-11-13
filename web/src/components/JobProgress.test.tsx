import { describe, expect, test, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { JobProgress } from './JobProgress';
import type { Job } from '../api/types';
import type { JobState } from '../api/jobs';

function state(job: Partial<Job> | null, extra: Partial<JobState> = {}): JobState {
  return {
    job: job && {
      id: 'x',
      type: 'paths',
      params: {},
      status: 'queued',
      progress: 0,
      result: null,
      error: '',
      created_at: '2026-11-11T12:00:00Z',
      finished_at: null,
      ...job,
    },
    error: null,
    notFound: false,
    gaveUp: false,
    ...extra,
  };
}

describe('JobProgress', () => {
  test('na fila: barra sem valor', () => {
    render(<JobProgress state={state({ status: 'queued' })} onRetry={vi.fn()} />);
    expect(screen.getByRole('status').textContent).toContain('na fila');
    expect(screen.getByRole('progressbar').hasAttribute('value')).toBe(false);
  });

  test('em andamento com percentual, nunca 100 antes de concluir', () => {
    const { rerender } = render(<JobProgress state={state({ status: 'running', progress: 0.4 })} onRetry={vi.fn()} />);
    expect(screen.getByRole('status').textContent).toContain('em andamento 40%');
    expect(screen.getByRole('progressbar').getAttribute('value')).toBe('40');
    rerender(<JobProgress state={state({ status: 'running', progress: 0.999 })} onRetry={vi.fn()} />);
    expect(screen.getByRole('status').textContent).toContain('em andamento 99%');
  });

  test('concluída', () => {
    render(<JobProgress state={state({ status: 'done', progress: 1 })} onRetry={vi.fn()} />);
    expect(screen.getByRole('status').textContent).toContain('concluída');
  });

  test('falhou: erro em português e "Tentar de novo"', async () => {
    const onRetry = vi.fn();
    render(<JobProgress state={state({ status: 'failed', error: 'expired' })} onRetry={onRetry} />);
    expect(screen.getByRole('status').textContent).toContain('falhou');
    expect(screen.getByRole('alert').textContent).toMatch(/expirou/);
    await userEvent.click(screen.getByRole('button', { name: 'Tentar de novo' }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  test('tarefa não encontrada e desistência depois do limite', () => {
    render(<JobProgress state={state(null, { notFound: true })} onRetry={vi.fn()} />);
    expect(screen.getByRole('alert').textContent).toContain('Tarefa não encontrada');
    expect(screen.getByRole('button', { name: 'Tentar de novo' })).toBeTruthy();
  });

  test('depois de desistir: "Continuar acompanhando" (mesma tarefa) e "Enviar de novo" (nova tarefa)', async () => {
    const onRetry = vi.fn();
    const onResume = vi.fn();
    render(
      <JobProgress state={state({ status: 'running', progress: 0.1 }, { gaveUp: true })} onRetry={onRetry} onResume={onResume} />,
    );
    expect(screen.getByRole('alert').textContent).toMatch(/10 minutos/);
    expect(screen.queryByRole('button', { name: 'Tentar de novo' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Continuar acompanhando' }));
    expect(onResume).toHaveBeenCalledTimes(1);
    expect(onRetry).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole('button', { name: 'Enviar de novo' }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });
});
