import { expect, test } from 'vitest';
import { parseConfig } from './config';

test('config.json válido, sem barra final', () => {
  expect(
    parseConfig({
      apiBaseUrl: 'http://localhost:8080/',
      oidcAuthority: 'http://localhost:8081/realms/r/',
      oidcClientId: 'web',
    }),
  ).toEqual({ apiBaseUrl: 'http://localhost:8080', oidcAuthority: 'http://localhost:8081/realms/r', oidcClientId: 'web' });
});

test('config.json inválido é recusado', () => {
  expect(() => parseConfig(null)).toThrow();
  expect(() => parseConfig({ apiBaseUrl: 'javascript:alert(1)', oidcAuthority: 'http://a', oidcClientId: 'w' })).toThrow();
  expect(() => parseConfig({ apiBaseUrl: 'http://a', oidcAuthority: 'http://b', oidcClientId: '' })).toThrow();
});
