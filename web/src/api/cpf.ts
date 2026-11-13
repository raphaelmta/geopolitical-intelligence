// CPFs typed in a search. The API answers a query carrying a CPF by the
// 6 middle digits (docs/search.md, "Busca por CPF"); the web app keeps such a
// query in memory only: never in the URL, the history or storage.

const NUMERIC_SEPARATORS = /[.\-/\s]/g;
/** A full CPF spelling: a run of 11 digits, or ddd S ddd S ddd S dd. */
const FULL = /(?<!\d)\d{3}[.\-/ ]?\d{3}[.\-/ ]?\d{3}[.\-/ ]?\d{2}(?!\d)/;
/** The masked form the sources publish: ***NNNNNN**, ***.NNN.NNN-**. */
const MASKED = /\*{3}[.\- ]?\d{3}[.\- ]?\d{3}[.\- ]?\*{2}/;

/**
 * Whether text may carry a CPF. Deliberately broader than the API's rule
 * (it also catches a CPF glued to a word): what it flags stays out of the
 * URL. A bare 6-digit number or a CNPJ (8 or 14 digits) is not flagged.
 */
export function mayCarryCPF(text: string): boolean {
  const digits = text.replace(NUMERIC_SEPARATORS, '');
  if (/^\d{11}$/.test(digits)) return true;
  return FULL.test(text) || MASKED.test(text);
}
