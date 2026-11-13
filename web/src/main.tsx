import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { AuthProvider } from 'react-oidc-context';
import { BrowserRouter } from 'react-router';
import { AppRoutes } from './App';
import { AuthGate, createUserManager, isSignoutReturn } from './auth';
import { SessionController } from './session';
import { configureApi } from './api/client';
import { loadConfig } from './config';
import './styles.css';

const root = createRoot(document.getElementById('root')!);

async function start() {
  let config;
  try {
    config = await loadConfig();
  } catch {
    root.render(
      <main className="standalone">
        <h1>Bancada</h1>
        <p role="alert">Não foi possível carregar a configuração da aplicação (config.json). Recarregue a página.</p>
      </main>,
    );
    return;
  }

  const userManager = createUserManager(config);
  const controller = new SessionController(userManager);
  configureApi({ baseUrl: config.apiBaseUrl, auth: controller });
  const signedOut = isSignoutReturn();
  const queryClient = new QueryClient({
    defaultOptions: {
      // Every read is audited by the API: no automatic retries or refetch on focus.
      queries: { retry: false, refetchOnWindowFocus: false, staleTime: 30_000 },
    },
  });

  root.render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <AuthProvider
          userManager={userManager}
          onSigninCallback={controller.afterSignin}
          matchSignoutCallback={() => isSignoutReturn()}
          onSignoutCallback={() => window.history.replaceState(null, '', '/')}
        >
          <AuthGate controller={controller} signedOut={signedOut}>
            <BrowserRouter>
              <AppRoutes />
            </BrowserRouter>
          </AuthGate>
        </AuthProvider>
      </QueryClientProvider>
    </StrictMode>,
  );
}

void start();
