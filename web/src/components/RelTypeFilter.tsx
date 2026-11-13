import { REL_TYPES, relationTypeName } from '../api/format';

/** Checkboxes of relationship types; none checked means every type. */
export function RelTypeFilter({ value, onChange }: { value: string[]; onChange: (types: string[]) => void }) {
  const toggle = (t: string, on: boolean) => {
    const next = new Set(value);
    if (on) next.add(t);
    else next.delete(t);
    // canonical order, whatever the click order
    onChange(REL_TYPES.filter((r) => next.has(r)));
  };
  return (
    <fieldset className="checks">
      <legend>Tipos de vínculo</legend>
      <p className="muted small checks__hint">Nenhum marcado: todos os tipos.</p>
      <div className="checks__grid">
        {REL_TYPES.map((t) => (
          <label key={t} className="check">
            <input type="checkbox" checked={value.includes(t)} onChange={(e) => toggle(t, e.target.checked)} />{' '}
            {relationTypeName(t)}
          </label>
        ))}
      </div>
    </fieldset>
  );
}
