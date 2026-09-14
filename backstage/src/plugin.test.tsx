import { screen } from '@testing-library/react';
import { mockApis, renderInTestApp, TestApiProvider } from '@backstage/test-utils';
import { EntityProvider } from '@backstage/plugin-catalog-react';
import * as index from './index';
import { godwitApiRef } from './api/GodwitApi';
import { GodwitClient } from './api/GodwitClient';

describe('godwitPlugin', () => {
  it('builds a client on the configured proxy path', async () => {
    const factory = [...index.godwitPlugin.getApis()].find(f => f.api.id === godwitApiRef.id)!;
    const fetch = jest.fn(async () => new Response('{}'));
    const client = factory.factory({
      discoveryApi: { getBaseUrl: async () => 'http://b/api/proxy' },
      fetchApi: { fetch },
      configApi: mockApis.config({ data: { godwit: { proxyPath: '/godwit-prod' } } }),
    }) as GodwitClient;
    expect(client).toBeInstanceOf(GodwitClient);
    await client.listTargets();
    expect(fetch).toHaveBeenCalledWith('http://b/api/proxy/godwit-prod/godwit.v1.GodwitService/ListTargets', expect.anything());
  });

  it('exports extensions that render lazily', async () => {
    const api = {
      getTargetStatus: async () => ({}),
      listTargets: async () => [],
      listRuns: async () => [],
      listDriftEvents: async () => [],
      listPlans: async () => [],
    };
    await renderInTestApp(
      <TestApiProvider apis={[[godwitApiRef, api]]}>
        <index.GodwitTargetsView targets={['direct']} />
        <EntityProvider entity={{ apiVersion: 'v1', kind: 'Component', metadata: { name: 'x' } }}>
          <index.EntityGodwitContent />
        </EntityProvider>
      </TestApiProvider>,
    );
    expect(await screen.findByText('direct')).toBeInTheDocument();
    expect(await screen.findByText('godwit/targets')).toBeInTheDocument();
    expect(index.GODWIT_TARGETS_ANNOTATION).toBe('godwit/targets');
    expect(Object.keys(index).sort()).toEqual([
      'DEFAULT_PROXY_PATH',
      'EntityGodwitContent',
      'GODWIT_TARGETS_ANNOTATION',
      'GodwitClient',
      'GodwitRequestError',
      'GodwitTargetsView',
      'getGodwitTargets',
      'godwitApiRef',
      'godwitPlugin',
      'isGodwitAvailable',
      'parseGodwitTargets',
    ]);
    expect(Object.values(index).every(Boolean)).toBe(true);
  });
});
