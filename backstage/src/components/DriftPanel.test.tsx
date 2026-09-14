import { screen, within } from '@testing-library/react';
import { renderInTestApp } from '@backstage/test-utils';
import { DriftPanel, DriftPanelProps } from './DriftPanel';
import { lineKind } from './DiffBlock';

const done = <T,>(value: T) => ({ loading: false, value });

const baselined = done({ driftBaseline: { takenAt: '2026-09-01T08:00:00Z' } });

async function show(props: Partial<DriftPanelProps>) {
  await renderInTestApp(<DriftPanel target="orders" events={done([])} status={baselined} {...props} />);
}

describe('DriftPanel', () => {
  it('waits for the events and the status', async () => {
    await show({ events: { loading: true } });
    expect(screen.getByTestId('progress')).toBeInTheDocument();
  });

  it('says there is no drift against a named baseline', async () => {
    await show({});
    expect(screen.getByText('no drift')).toBeInTheDocument();
    expect(screen.getByText(/matches the baseline taken 2026-09-01 08:00 UTC/)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('reads an omitted event list as no events', async () => {
    await show({ events: { loading: false, value: undefined } });
    expect(screen.getByText('no drift')).toBeInTheDocument();
  });

  it('does not read an unread drift log as clean', async () => {
    await show({ events: { loading: false, error: new Error('permission_denied') } });
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Could not read drift for orders; its drift state is unknown: permission_denied',
    );
    expect(screen.queryByText('no drift')).not.toBeInTheDocument();
  });

  it('says a target without a baseline is not watched', async () => {
    await show({ status: done({}) });
    expect(screen.getByText('not watched')).toBeInTheDocument();
    expect(screen.queryByText('no drift')).not.toBeInTheDocument();
  });

  it('does not claim a baseline when the status could not be read', async () => {
    await show({ status: { loading: false, error: new Error('x') } });
    expect(screen.getByText('no open drift')).toBeInTheDocument();
    expect(screen.getByText(/cannot say whether a baseline is being compared/)).toBeInTheDocument();
  });

  it('shows every open event with its diff line by line, and how it is resolved', async () => {
    await show({
      events: done([
        {
          id: '2',
          detectedAt: '2026-09-10T03:00:00Z',
          diff: '- index public.orders_idx\n+ column public.orders.hotfix_note text\n',
        },
        { id: '1', detectedAt: '2026-09-09T03:00:00Z' },
      ]),
    });
    expect(screen.getByText('drifted')).toBeInTheDocument();
    expect(screen.getByText('Detected 2026-09-10 03:00 UTC')).toBeInTheDocument();
    expect(screen.getByText('Detected 2026-09-09 03:00 UTC')).toBeInTheDocument();
    expect(screen.getByText('+ column public.orders.hotfix_note text')).toHaveAttribute('data-kind', 'added');
    expect(screen.getByText('- index public.orders_idx')).toHaveAttribute('data-kind', 'removed');
    expect(screen.getByText('godwit drift accept orders')).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  it('lists resolved events with both times beside the current state', async () => {
    await show({
      events: done([
        { id: '3', detectedAt: '2026-09-11T00:00:00Z', diff: '+ x' },
        {
          id: '1',
          detectedAt: '2026-09-08T10:00:00Z',
          resolvedAt: '2026-09-08T11:30:00Z',
          diff: '+ index public.tmp',
        },
        { id: '0', detectedAt: '2026-09-01T10:00:00Z', resolvedAt: '2026-09-01T10:05:00Z' },
      ]),
    });
    expect(screen.getByText('drifted')).toBeInTheDocument();
    const table = screen.getByRole('table', { name: 'resolved drift' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent('2026-09-08 10:00 UTC2026-09-08 11:30 UTC');
    expect(within(rows[0]).getByText('+ index public.tmp')).toBeInTheDocument();
  });
});

describe('lineKind', () => {
  it('classifies by the leading character', () => {
    expect(['+ a', '- a', '~ a', '  a'].map(lineKind)).toEqual(['added', 'removed', 'changed', 'context']);
  });
});
