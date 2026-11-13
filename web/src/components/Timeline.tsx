import type { ReactNode } from 'react';

export interface TimelineItem {
  key: string | number;
  title: ReactNode;
  current?: boolean;
  children?: ReactNode;
}

/** A plain chronological list (in the order given, newest first by convention). */
export function Timeline({ label, items }: { label: string; items: TimelineItem[] }) {
  return (
    <ol className="timeline" aria-label={label}>
      {items.map((item) => (
        <li key={item.key} className={item.current ? 'timeline__item timeline__item--current' : 'timeline__item'}>
          <p className="timeline__title">
            {item.title}
            {item.current && <span className="tag tag--current">atual</span>}
          </p>
          {item.children}
        </li>
      ))}
    </ol>
  );
}
