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
            <svg className="brand__mark" viewBox="0 0 24 24" aria-hidden="true" focusable="false">
              <path d="M6 7 18 5M6 7l6 11M18 5l-6 13" fill="none" stroke="currentColor" strokeWidth="1.6" />
              <circle cx="6" cy="7" r="3.2" fill="currentColor" />
              <circle cx="18" cy="5" r="2.6" fill="currentColor" />
              <circle cx="12" cy="18" r="3.2" fill="currentColor" />
            </svg>
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
