import { describe, expect, test } from 'vitest';
import { render, screen } from '@testing-library/react';
import { EdgeBadge } from './EdgeBadge';
import { edge } from '../test/fixtures';

describe('EdgeBadge', () => {
  test('EdgeBadge mostra "fato" para fato', () => {
    const { container } = render(<EdgeBadge edge={edge({ kind: 'fact', confidence: null })} />);
    expect(screen.getByText('fato')).toBeTruthy();
    expect(container.textContent).not.toContain('%');
    expect(container.querySelector('.edge-line--solid')).not.toBeNull();
    expect(container.querySelector('.edge-line--dashed')).toBeNull();
  });

  test('EdgeBadge mostra "inferência · 80%" para inferência 0.8', () => {
    const { container } = render(<EdgeBadge edge={edge({ kind: 'inference', confidence: 0.8 })} />);
    expect(screen.getByText('inferência · 80%')).toBeTruthy();
    expect(screen.queryByText('fato')).toBeNull();
    expect(container.querySelector('.edge-line--dashed')).not.toBeNull();
  });

  test('EdgeBadge arredonda a confiança da inferência', () => {
    render(<EdgeBadge edge={edge({ kind: 'inference', confidence: 0.666 })} />);
    expect(screen.getByText('inferência · 67%')).toBeTruthy();
  });

  test('base "auto": "identidade 99%", com a explicação da união automática', () => {
    render(<EdgeBadge edge={edge({ identity_basis: 'auto', identity_confidence: 0.99999 })} />);
    expect(screen.getByText('fato')).toBeTruthy();
    const tag = screen.getByText('identidade 99%');
    expect(tag.getAttribute('title')).toContain('pessoas unidas automaticamente por nome e trecho do CPF');
  });

  test('base "auto" com confiança 1 nunca mostra 100%', () => {
    const { container } = render(<EdgeBadge edge={edge({ identity_basis: 'auto', identity_confidence: 1 })} />);
    expect(screen.getByText('identidade · confiança não informada')).toBeTruthy();
    expect(container.textContent).not.toContain('100%');
  });

  test('base "analyst": "identidade: decisão de analista", sem porcentagem', () => {
    const { container } = render(<EdgeBadge edge={edge({ identity_basis: 'analyst', identity_confidence: 1 })} />);
    expect(screen.getByText('fato')).toBeTruthy();
    expect(screen.getByText('identidade: decisão de analista').getAttribute('title')).toMatch(/analista/);
    expect(container.textContent).not.toContain('%');
  });

  test('base "unknown": "identidade: base não informada"', () => {
    render(<EdgeBadge edge={edge({ identity_basis: 'unknown', identity_confidence: 0.9 })} />);
    expect(screen.getByText('identidade: base não informada')).toBeTruthy();
  });

  test('base "none": nada sobre identidade', () => {
    const { container } = render(<EdgeBadge edge={edge({ identity_basis: 'none', identity_confidence: 1 })} />);
    expect(container.textContent).not.toContain('identidade');
  });

  test('EdgeBadge mostra "vencido" quando stale', () => {
    const { rerender } = render(<EdgeBadge edge={edge({ stale: false })} />);
    expect(screen.queryByText('vencido')).toBeNull();
    rerender(<EdgeBadge edge={edge({ stale: true })} />);
    expect(screen.getByText('vencido')).toBeTruthy();
  });

  test.each([
    [0.995, '99%'],
    [0.999, '99%'],
    [0.9999, '99%'],
    [1, '100%'],
    [0.004, '< 1%'],
    [0, '0%'],
    [0.5, '50%'],
  ])('inferência %s → "inferência · %s" (nunca arredonda para 100%%)', (value, text) => {
    render(<EdgeBadge edge={edge({ kind: 'inference', confidence: value })} />);
    expect(screen.getByText(`inferência · ${text}`)).toBeTruthy();
  });

  test.each([null, NaN, 1.2, -0.1])('inferência com confiança inválida (%s) não mostra número', (value) => {
    const { container } = render(<EdgeBadge edge={edge({ kind: 'inference', confidence: value })} />);
    expect(screen.getByText('inferência · confiança não informada')).toBeTruthy();
    expect(container.textContent).not.toMatch(/\d%/);
  });

  test.each([
    [0.995, 'identidade 99%'],
    [0.999, 'identidade 99%'],
    [0.9999, 'identidade 99%'],
    [0.004, 'identidade < 1%'],
    [0, 'identidade 0%'],
  ])('identidade automática %s → "%s"', (value, text) => {
    render(<EdgeBadge edge={edge({ identity_basis: 'auto', identity_confidence: value })} />);
    expect(screen.getByText(text)).toBeTruthy();
  });

  test.each([null, NaN, 1.2, -0.1])('identidade automática inválida (%s) → "confiança não informada"', (value) => {
    const { container } = render(<EdgeBadge edge={edge({ identity_basis: 'auto', identity_confidence: value })} />);
    expect(screen.getByText('identidade · confiança não informada')).toBeTruthy();
    expect(container.textContent).not.toMatch(/\d%/);
  });

  test('sem o campo identity_basis (dado antigo): sem registro de pessoa → nada; com registro → base não informada', () => {
    const { container, rerender } = render(
      <EdgeBadge edge={edge({ identity_basis: undefined, identity_confidence: undefined })} />,
    );
    expect(container.textContent).not.toContain('identidade');
    rerender(<EdgeBadge edge={edge({ identity_basis: undefined, identity_confidence: 1 })} />);
    expect(screen.getByText('identidade: base não informada')).toBeTruthy();
  });
});
