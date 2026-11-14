import { kindLabel } from '../api/format';
import { KIND_ICONS } from './graphStyle';

/**
 * The round colored mark of an entity kind, the same glyph and color as its
 * node in the drawing. Decorative by default (the kind is written beside
 * it); `labelled` gives it the kind as its accessible name.
 */
export function KindIcon({ kind, size, labelled = false }: { kind: string; size?: 'small' | 'large'; labelled?: boolean }) {
  const known = kind in KIND_ICONS;
  return (
    <span
      className={`kind-icon kind-icon--${known ? kind : 'other'}${size ? ` kind-icon--${size}` : ''}`}
      {...(labelled ? { role: 'img', 'aria-label': kindLabel(kind) } : { 'aria-hidden': true })}
    >
      {known && (
        // Static markup of this module's own table (graphStyle.KIND_ICONS).
        <svg viewBox="0 0 24 24" focusable="false" dangerouslySetInnerHTML={{ __html: KIND_ICONS[kind]! }} />
      )}
    </span>
  );
}
