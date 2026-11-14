import { describe, expect, test } from 'vitest';
import { render, screen } from '@testing-library/react';
import { KindIcon } from './KindIcon';
import { NODE_KINDS } from './graphStyle';

describe('KindIcon', () => {
  test('por padrão é decorativo: escondido de leitores de tela, sem nome', () => {
    const { container } = render(<KindIcon kind="company" />);
    const icon = container.querySelector('.kind-icon')!;
    expect(icon.getAttribute('aria-hidden')).toBe('true');
    expect(icon.getAttribute('role')).toBeNull();
    expect(icon.getAttribute('aria-label')).toBeNull();
    expect(screen.queryByRole('img')).toBeNull();
    expect(icon.className).toContain('kind-icon--company');
    expect(icon.querySelector('svg')).not.toBeNull();
  });

  test('com labelled vira imagem com o tipo como nome', () => {
    render(<KindIcon kind="public_body" labelled />);
    const icon = screen.getByRole('img', { name: 'Órgão público' });
    expect(icon.getAttribute('aria-hidden')).toBeNull();
  });

  test('cada tipo conhecido tem seu glifo e sua classe; tamanho vira classe', () => {
    for (const kind of NODE_KINDS) {
      const { container, unmount } = render(<KindIcon kind={kind} size="large" />);
      const icon = container.querySelector('.kind-icon')!;
      expect(icon.className).toContain(`kind-icon--${kind}`);
      expect(icon.className).toContain('kind-icon--large');
      expect(icon.querySelector('svg')!.innerHTML).not.toBe('');
      unmount();
    }
  });

  test('tipo desconhecido usa a classe "other", sem glifo, e o nome é o próprio tipo', () => {
    const { container } = render(<KindIcon kind="outer_space" labelled />);
    const icon = container.querySelector('.kind-icon')!;
    expect(icon.className).toContain('kind-icon--other');
    expect(icon.querySelector('svg')).toBeNull();
    expect(screen.getByRole('img', { name: 'outer_space' })).toBe(icon);
  });
});
