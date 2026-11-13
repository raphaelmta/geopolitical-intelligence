/** Marks data whose source is past its update deadline. */
export function StaleTag() {
  return (
    <span className="tag tag--stale" title="A fonte deste dado está desatualizada: a última coleta completa passou do prazo">
      vencido
    </span>
  );
}
