import {
  configApiRef,
  createApiFactory,
  createComponentExtension,
  createPlugin,
  discoveryApiRef,
  fetchApiRef,
} from '@backstage/core-plugin-api';
import { godwitApiRef } from './api/GodwitApi';
import { GodwitClient } from './api/GodwitClient';

export const godwitPlugin = createPlugin({
  id: 'godwit',
  apis: [
    createApiFactory({
      api: godwitApiRef,
      deps: { discoveryApi: discoveryApiRef, fetchApi: fetchApiRef, configApi: configApiRef },
      factory: ({ discoveryApi, fetchApi, configApi }) =>
        new GodwitClient({
          discoveryApi,
          fetchApi,
          proxyPath: configApi.getOptionalString('godwit.proxyPath'),
        }),
    }),
  ],
});

export const GodwitTargetsView = godwitPlugin.provide(
  createComponentExtension({
    name: 'GodwitTargetsView',
    component: {
      lazy: () => import('./components/GodwitTargetsView').then(m => m.GodwitTargetsView),
    },
  }),
);

export const EntityGodwitContent = godwitPlugin.provide(
  createComponentExtension({
    name: 'EntityGodwitContent',
    component: {
      lazy: () => import('./components/EntityGodwitContent').then(m => m.EntityGodwitContent),
    },
  }),
);
