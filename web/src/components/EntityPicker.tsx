import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { useQuery } from '@tanstack/react-query';
import { apiGet, errorMessage } from '../api/client';
import type { EntityRef, SearchResponse } from '../api/types';
import { kindLabel, naturalKeyLabel, usefulLength } from '../api/format';

const DEBOUNCE_MS = 400;
const LIMIT = 10;

/**
 * Chooses one entity by searching as you type (the search API, from 3
 * useful characters, after a pause so not every keystroke is a search). A
 * combobox: arrows move through the results, Enter chooses, Esc closes.
 * Once chosen it shows the entity with "Trocar".
 */
export function EntityPicker({
  label,
  value,
  onChange,
}: {
  label: string;
  value: EntityRef | null;
  onChange: (entity: EntityRef | null) => void;
}) {
  const [text, setText] = useState('');
  const [query, setQuery] = useState('');
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const inputRef = useRef<HTMLInputElement>(null);
  const refocus = useRef(false);
  const inputId = useId();
  const listId = useId();
  const hintId = useId();
  const labelId = useId();

  useEffect(() => {
    const t = setTimeout(() => setQuery(text.trim()), DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [text]);

  useEffect(() => {
    if (!value && refocus.current) {
      refocus.current = false;
      inputRef.current?.focus();
    }
  }, [value]);

  const enabled = usefulLength(query) >= 3;
  const result = useQuery({
    queryKey: ['picker', query],
    queryFn: () => apiGet<SearchResponse>(`/search?${new URLSearchParams({ q: query, limit: String(LIMIT) })}`),
    enabled,
    staleTime: 60_000,
  });
  const items = enabled && result.data ? result.data.items : [];
  const showList = open && items.length > 0;
  const optionId = (i: number) => `${listId}-${i}`;

  const choose = (item: EntityRef) => {
    setOpen(false);
    setActive(-1);
    setText('');
    setQuery('');
    onChange(item);
  };

  if (value) {
    const key = naturalKeyLabel(value.kind, value.natural_key);
    return (
      <div className="picker" role="group" aria-labelledby={labelId}>
        <span id={labelId} className="picker__label">
          {label}
        </span>
        <p className="picker__chosen">
          <span>{value.name}</span>{' '}
          <span className="muted small">
            · {kindLabel(value.kind)}
            {key && ` · ${key}`}
          </span>{' '}
          <button
            type="button"
            className="link-button"
            aria-label={`Trocar a entidade (${label})`}
            onClick={() => {
              refocus.current = true;
              onChange(null);
            }}
          >
            Trocar
          </button>
        </p>
      </div>
    );
  }

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setOpen(true);
      if (items.length) setActive((a) => Math.min(a + 1, items.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (items.length) setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === 'Enter') {
      if (showList && active >= 0 && items[active]) {
        e.preventDefault();
        choose(items[active]);
      }
    } else if (e.key === 'Escape') {
      if (showList) e.preventDefault();
      setOpen(false);
      setActive(-1);
    }
  };

  const typed = text.trim();
  const hint =
    typed && usefulLength(typed) < 3
      ? 'Digite pelo menos 3 caracteres úteis (letras ou números).'
      : !enabled || typed !== query
        ? 'Digite parte do nome ou o CNPJ.'
        : result.isFetching
          ? 'Buscando…'
          : result.isError
            ? errorMessage(result.error)
            : items.length === 0
              ? 'Nenhum resultado.'
              : `${items.length} resultado${items.length === 1 ? '' : 's'}${items.length >= LIMIT ? ' (os primeiros; refine a busca)' : ''}.`;

  return (
    <div className="picker">
      <label htmlFor={inputId} className="picker__label">
        {label}
      </label>
      <input
        ref={inputRef}
        id={inputId}
        type="text"
        role="combobox"
        aria-expanded={showList}
        aria-controls={showList ? listId : undefined}
        aria-autocomplete="list"
        aria-describedby={hintId}
        aria-activedescendant={showList && active >= 0 ? optionId(active) : undefined}
        autoComplete="off"
        spellCheck={false}
        maxLength={200}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          setOpen(true);
          setActive(-1);
        }}
        onKeyDown={onKeyDown}
        onBlur={() => setOpen(false)}
        onFocus={() => setOpen(true)}
      />
      <p id={hintId} className="hint" aria-live="polite">
        {hint}
      </p>
      {showList && (
        <ul id={listId} role="listbox" aria-label={`Resultados para ${label}`} className="picker__list">
          {items.map((item, i) => {
            const key = naturalKeyLabel(item.kind, item.natural_key);
            return (
              <li
                key={item.id}
                id={optionId(i)}
                role="option"
                aria-selected={i === active}
                className={i === active ? 'picker__option is-active' : 'picker__option'}
                // keep focus in the input (no blur) when an option is clicked
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => choose(item)}
              >
                <span>{item.name}</span>{' '}
                <span className="muted small">
                  · {kindLabel(item.kind)}
                  {key && ` · ${key}`}
                </span>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
