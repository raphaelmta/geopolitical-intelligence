/** What the edge tags mean; shown wherever edges are listed. */
export function EdgeLegend() {
  return (
    <details className="fold">
      <summary>
        <span className="fold__title">Como ler as etiquetas</span>{' '}
        <span className="muted small">fato, inferência, identidade e vencido</span>
      </summary>
      <aside className="legend" aria-label="Legenda">
      <p>
        <span className="tag tag--fact">
          <span className="edge-line edge-line--solid" aria-hidden="true" />
          <span>fato</span>
        </span>{' '}
        lido de uma fonte ·{' '}
        <span className="tag tag--inference">
          <span className="edge-line edge-line--dashed" aria-hidden="true" />
          <span>inferência · NN%</span>
        </span>{' '}
        derivado, com a confiança · <span className="tag tag--identity">identidade NN%</span> o vínculo depende de
        pessoas unidas automaticamente por nome e trecho do CPF (inferência, com a confiança) ·{' '}
        <span className="tag tag--identity">identidade: decisão de analista</span> depende de registros unidos por um
        analista: um julgamento humano, não um fato documentado ·{' '}
        <span className="tag tag--stale">vencido</span> a fonte passou do prazo de atualização.
        Clique em “fonte” para ver de onde veio cada vínculo.
      </p>
      </aside>
    </details>
  );
}
