import { Link } from 'react-router';
import { usePageHeading } from '../usePageHeading';

export function NotFoundPage() {
  const h1 = usePageHeading('Página não encontrada');
  return (
    <>
      <h1 ref={h1} tabIndex={-1}>
        Página não encontrada
      </h1>
      <p>
        Este endereço não existe na Bancada. <Link to="/">Voltar para a busca</Link>.
      </p>
    </>
  );
}
