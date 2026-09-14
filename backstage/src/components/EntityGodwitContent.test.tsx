import { screen } from '@testing-library/react';
import { renderInTestApp, TestApiProvider } from '@backstage/test-utils';
import { EntityProvider } from '@backstage/plugin-catalog-react';
import { Entity } from '@backstage/catalog-model';
import { GodwitApi, godwitApiRef } from '../api/GodwitApi';
import { EntityGodwitContent } from './EntityGodwitContent';

const api: GodwitApi = {
  getTargetStatus: jest.fn(async () => ({})),
  listTargets: jest.fn(async () => []),
  listRuns: jest.fn(async () => []),
  listDriftEvents: jest.fn(async () => []),
  listPlans: jest.fn(async () => []),
};

const entity = (annotations: Record<string, string>): Entity => ({
  apiVersion: 'backstage.io/v1alpha1',
  kind: 'Component',
  metadata: { name: 'orders', annotations },
});

async function show(e: Entity) {
  await renderInTestApp(
    <TestApiProvider apis={[[godwitApiRef, api]]}>
      <EntityProvider entity={e}>
        <EntityGodwitContent maxRuns={5} />
      </EntityProvider>
    </TestApiProvider>,
  );
}

describe('EntityGodwitContent', () => {
  it('shows the targets the annotation names', async () => {
    await show(entity({ 'godwit/targets': 'orders-staging, orders-production' }));
    expect(await screen.findByText('orders-staging')).toBeInTheDocument();
    expect(screen.getByText('orders-production')).toBeInTheDocument();
  });

  it('points at the annotation when the entity has none', async () => {
    await show(entity({}));
    expect(screen.getByText('godwit/targets')).toBeInTheDocument();
  });
});
