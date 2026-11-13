import { useEffect, useRef } from 'react';
import { useLocation } from 'react-router';

/**
 * Page title ("<title> — Bancada") and, on each new route, focus on the
 * page's <h1> (give it tabIndex={-1}) so screen readers announce the page.
 * Changes of the query string alone (a new search) do not move focus.
 * ready: the heading is rendered (false while the page is loading).
 */
export function usePageHeading(title: string, ready = true) {
  const ref = useRef<HTMLHeadingElement>(null);
  const { pathname } = useLocation();
  const focusedFor = useRef<string | null>(null);

  useEffect(() => {
    document.title = `${title} — Bancada`;
  }, [title]);

  useEffect(() => {
    if (ready && ref.current && focusedFor.current !== pathname) {
      focusedFor.current = pathname;
      ref.current.focus();
    }
  });

  return ref;
}
