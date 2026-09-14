import { ComponentProps } from 'react';
import { screen, within } from '@testing-library/react';
import { renderInTestApp } from '@backstage/test-utils';
import { Run } from '../api/types';
import { kindLabel, RunsTable, stateLabel } from './RunsTable';

const show = (runs: RunsTableRuns) => renderInTestApp(<RunsTable target="orders" runs={runs} maxRuns={10} />);
type RunsTableRuns = ComponentProps<typeof RunsTable>['runs'];

describe('RunsTable', () => {
  it('waits, then names an unread list', async () => {
    const { unmount } = await show({ loading: true });
    expect(screen.getByTestId('progress')).toBeInTheDocument();
    unmount();
    await show({ loading: false, error: new Error('unavailable') });
    expect(screen.getByRole('alert')).toHaveTextContent('Could not read the runs on orders: unavailable');
  });

  it('says when a target has no run', async () => {
    const first = await show({ loading: false, value: [] });
    expect(screen.getByText('No run has been created on orders.')).toBeInTheDocument();
    first.unmount();
    await show({ loading: false, value: undefined });
    expect(screen.getByText('No run has been created on orders.')).toBeInTheDocument();
  });

  it('shows state, kind, who created the run and when', async () => {
    const runs: Run[] = [
      {
        id: '11111111-aaaa',
        state: 'RUN_STATE_NEEDS_ATTENTION',
        error: 'lock timeout',
        kind: 'migrate',
        phase: 'expand',
        createdBy: 'github:acme/orders:(autoplan)',
        createdAt: '2026-09-02T12:00:00Z',
        finishedAt: '2026-09-02T12:01:00Z',
      },
      { id: '22222222-bbbb', state: 'RUN_STATE_UNSPECIFIED' },
    ];
    await show({ loading: false, value: runs });
    const rows = within(screen.getByRole('table', { name: 'runs on orders' })).getAllByRole('row');
    expect(rows[1]).toHaveTextContent('11111111');
    expect(within(rows[1]).getByText('needs attention')).toBeInTheDocument();
    expect(rows[1]).toHaveTextContent('lock timeout');
    expect(rows[1]).toHaveTextContent('migrate (expand)');
    expect(within(rows[1]).getByTitle('github:acme/orders:(autoplan)')).toHaveTextContent(
      'autoplanGitHub App on acme/orders',
    );
    expect(rows[1]).toHaveTextContent('2026-09-02 12:00 UTC');
    expect(rows[1]).toHaveTextContent('2026-09-02 12:01 UTC');
    expect(rows[2]).toHaveTextContent('unspecified');
    expect(rows[2]).toHaveTextContent('unknown');
  });

  it.each([
    ['RUN_STATE_QUEUED', 'queued'],
    ['RUN_STATE_RUNNING', 'running'],
    ['RUN_STATE_SUCCEEDED', 'succeeded'],
    ['RUN_STATE_FAILED', 'failed'],
    ['RUN_STATE_AWAITING_CONTRACT', 'awaiting contract'],
    ['RUN_STATE_REVERTED', 'reverted'],
  ] as const)('labels %s', async (state, label) => {
    await show({ loading: false, value: [{ id: 'r', state }] });
    expect(screen.getByText(label)).toBeInTheDocument();
  });
});

describe('labels', () => {
  it('reads an absent state as unspecified', () => {
    expect(stateLabel(undefined)).toBe('unspecified');
  });

  it('names a revert and defaults the kind', () => {
    expect(kindLabel({ id: 'r', reverts: 'abcdef0123' })).toBe('revert of abcdef01');
    expect(kindLabel({ id: 'r', kind: 'baseline' })).toBe('baseline');
    expect(kindLabel({ id: 'r' })).toBe('migrate');
  });
});
