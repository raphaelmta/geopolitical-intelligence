// Loopback forwarders for the e2e browser (started by playwright.config.ts
// as its webServer). The browser must use the production URLs
// (http://localhost:5173 for the app, :8080 for the API, :8081 for
// Keycloak): localhost is a secure context (PKCE needs Web Crypto), and the
// realm, KC_HOSTNAME, CSP and CORS then hold their production values. Inside
// the playwright container those loopback ports are forwarded to the
// compose services. E2E_FORWARD: "5173=web:5173,8080=api:8080,...".
// Nothing is published on the host: these ports exist only in this container.
import net from 'node:net';

const spec = process.env.E2E_FORWARD ?? '';
if (!spec) {
  console.error('forward: E2E_FORWARD is empty');
  process.exit(1);
}

for (const pair of spec.split(',')) {
  const [local, target] = pair.split('=');
  const [host, port] = (target ?? '').split(':');
  if (!local || !host || !port) {
    console.error(`forward: bad entry ${pair}`);
    process.exit(1);
  }
  for (const addr of ['127.0.0.1', '::1']) {
    const server = net.createServer((client) => {
      const upstream = net.connect(Number(port), host);
      client.pipe(upstream).pipe(client);
      upstream.on('error', () => client.destroy());
      client.on('error', () => upstream.destroy());
    });
    server.on('error', (err) => {
      // A container without IPv6 has no ::1: IPv4 alone is enough.
      if (addr === '::1' && (err.code === 'EADDRNOTAVAIL' || err.code === 'EAFNOSUPPORT')) return;
      console.error(`forward: ${addr}:${local}: ${err.message}`);
      process.exit(1);
    });
    server.listen(Number(local), addr);
  }
  console.log(`forward: localhost:${local} -> ${host}:${port}`);
}
