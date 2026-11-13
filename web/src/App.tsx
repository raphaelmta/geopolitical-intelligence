import { Link, NavLink, Outlet, Route, Routes } from 'react-router';
import { ROLE_LABELS, useSession } from './auth';
import { SearchPage } from './pages/SearchPage';
import { EntityPage } from './pages/EntityPage';
import { NotFoundPage } from './pages/NotFoundPage';
import { MapPage } from './pages/MapPage';
import { PathsPage } from './pages/PathsPage';
import { ControlGroupPage } from './pages/ControlGroupPage';
import { IdentityQueuePage } from './pages/IdentityQueuePage';
import { SourcesPage } from './pages/SourcesPage';

export function AppRoutes() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route index element={<SearchPage />} />
        <Route path="entidade/:id" element={<EntityPage />} />
        <Route path="mapa/:id" element={<MapPage />} />
        <Route path="caminhos" element={<PathsPage />} />
        <Route path="grupo/:id" element={<ControlGroupPage />} />
        <Route path="identidades" element={<IdentityQueuePage />} />
        <Route path="bases" element={<SourcesPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}

function Layout() {
  const session = useSession();
  const roles = session.roles.filter((r) => r in ROLE_LABELS).map((r) => ROLE_LABELS[r]);
  return (
    <>
      <a className="skip-link" href="#conteudo">
        Ir para o conteúdo
      </a>
      <header className="site-header">
        <div className="site-header__inner">
          <Link to="/" className="brand">
            Bancada
          </Link>
          <nav aria-label="Principal">
            <ul className="nav">
              <li>
                <NavLink to="/" end>
                  Busca
                </NavLink>
              </li>
              <li>
                <NavLink to="/caminhos">Caminhos</NavLink>
              </li>
              <li>
                <NavLink to="/identidades">Identidades</NavLink>
              </li>
              <li>
                <NavLink to="/bases">Bases</NavLink>
              </li>
            </ul>
          </nav>
          <div className="user">
            <span className="user__name">{session.name}</span>
            {roles.length > 0 && <span className="user__roles">{roles.join(' · ')}</span>}
            <button type="button" className="link-button" onClick={session.signOut}>
              Sair
            </button>
          </div>
        </div>
      </header>
      <main id="conteudo" className="page" tabIndex={-1}>
        <Outlet />
      </main>
    </>
  );
}
