import { screen, within } from '@testing-library/react';
import { renderInTestApp, TestApiProvider } from '@backstage/test-utils';
import { GodwitApi, godwitApiRef } from '../api/GodwitApi';
import { GodwitTargetsView } from './GodwitTargetsView';

function fakeApi(overrides: Partial<GodwitApi> = {}): jest.Mocked<GodwitApi> {
  return {
    getTargetStatus: jest.fn(async (target: string) => ({
      target,
      provider: 'vault',
      applied: [{ version: '20260101000000', name: 'init' }],
      driftBaseline: { takenAt: '2026-09-01T00:00:00Z' },
    })),
    listTargets: jest.fn(async () => [
      { name: 'orders-staging', provider: 'vault', credentialStore: 'staging-vault', attentionRuns: 1 },
      { name: 'orders-production', provider: 'static' },
    ]),
    listRuns: jest.fn(async (target: string) => [
      {
        id: `${target}-run-0001`,
        state: 'RUN_STATE_SUCCEEDED' as const,
        kind: 'migrate',
        createdBy: 'github:acme/orders:alice',
        createdAt: '2026-09-02T12:00:00Z',
      },
    ]),
    listDriftEvents: jest.fn(async () => []),
    listPlans: jest.fn(async () => [{ id: 'plan-abcdef12', state: 'ready', migrations: [{ applied: false }] }]),
    ...overrides,
  } as jest.Mocked<GodwitApi>;
}

async function render(api: GodwitApi, targets: string[], maxRuns?: number) {
  await renderInTestApp(
    <TestApiProvider apis={[[godwitApiRef, api]]}>
      <GodwitTargetsView targets={targets} maxRuns={maxRuns} />
    </TestApiProvider>,
  );
}

describe('GodwitTargetsView', () => {
  it('renders one card per distinct target, in the order given', async () => {
    const api = fakeApi();
    await render(api, ['orders-staging', ' orders-production ', 'orders-staging', '']);

    expect(await screen.findAllByText('godwit target')).toHaveLength(2);
    const text = document.body.textContent!;
    expect(text.indexOf('orders-staging')).toBeLessThan(text.indexOf('orders-production'));
    expect(api.listTargets).toHaveBeenCalledTimes(1);
    expect(api.getTargetStatus.mock.calls.map(c => c[0])).toEqual(['orders-staging', 'orders-production']);

    expect(await screen.findByText('staging-vault')).toBeInTheDocument();
    expect(screen.getByText('none (static provider)')).toBeInTheDocument();
    expect(screen.getAllByText('20260101000000')).toHaveLength(2);
    expect(screen.getAllByText('1 in plan plan-abc')).toHaveLength(2);
    expect(screen.getAllByText('@alice')).toHaveLength(2);
    expect(screen.getAllByText('no drift')).toHaveLength(2);
  });

  it('shows an empty state without targets', async () => {
    const api = fakeApi();
    await render(api, [' ']);
    expect(screen.getByText('No godwit targets')).toBeInTheDocument();
    expect(api.getTargetStatus).not.toHaveBeenCalled();
  });

  it('caps the runs shown per target', async () => {
    const runs = Array.from({ length: 12 }, (_, i) => ({ id: `run-${String(i).padStart(4, '0')}` }));
    await render(fakeApi({ listRuns: jest.fn(async () => runs) }), ['orders-staging'], 3);
    const table = await screen.findByRole('table', { name: 'runs on orders-staging' });
    expect(within(table).getAllByRole('row')).toHaveLength(4);
  });

  it('shows ten runs by default', async () => {
    const runs = Array.from({ length: 12 }, (_, i) => ({ id: `run-${String(i).padStart(4, '0')}` }));
    await render(fakeApi({ listRuns: jest.fn(async () => runs) }), ['orders-staging']);
    const table = await screen.findByRole('table', { name: 'runs on orders-staging' });
    expect(within(table).getAllByRole('row')).toHaveLength(11);
  });
});
