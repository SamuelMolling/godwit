import { render, screen } from '@testing-library/react';
import { TargetOverview, TargetOverviewProps } from './TargetOverview';

const done = <T,>(value: T) => ({ loading: false, value });
const failed = (message: string) => ({ loading: false, error: new Error(message) });

function show(props: Partial<TargetOverviewProps>) {
  render(
    <TargetOverview
      target="orders"
      status={done({ provider: 'vault', applied: [{ version: '3' }, { name: 'v', repeatable: true }] })}
      plans={done([])}
      summaries={done([{ name: 'orders', provider: 'vault', attentionRuns: 2 }])}
      {...props}
    />,
  );
}

const fact = (label: string) => screen.getByText(label).nextSibling?.textContent;

describe('TargetOverview', () => {
  it('waits for every source', () => {
    show({ status: { loading: true } });
    expect(screen.getByTestId('progress')).toBeInTheDocument();
  });

  it('shows the counts, the version and where the credential is read from', () => {
    show({});
    expect(fact('Applied')).toBe('1');
    expect(fact('At version')).toBe('3');
    expect(fact('Pending')).toBe('no ready plan');
    expect(fact('Provider')).toBe('vault');
    expect(fact('Credential store')).toBe('VAULT_ADDR');
    expect(fact('Runs needing attention')).toBe('2');
  });

  it('reads a target with nothing applied as none', () => {
    show({ status: done({}), summaries: done([{ name: 'orders' }]) });
    expect(fact('Applied')).toBe('0');
    expect(fact('At version')).toBe('none');
    expect(fact('Provider')).toBe('unknown');
    expect(fact('Credential store')).toBe('none (unknown provider)');
    expect(fact('Runs needing attention')).toBe('0');
  });

  it('says the journal is unreachable rather than showing zero', () => {
    show({ status: done({ provider: 'vault', unreachable: 'credential store vault-a is not registered' }) });
    expect(screen.getByRole('alert')).toHaveTextContent(
      'godwit could not read the journal of orders: credential store vault-a is not registered',
    );
    expect(fact('Applied')).toBe('unknown');
    expect(fact('At version')).toBe('unknown');
  });

  it('names every source it could not read', () => {
    show({
      status: failed('not_found'),
      plans: failed('plans down'),
      summaries: failed('targets down'),
    });
    expect(screen.getAllByRole('alert').map(a => a.textContent)).toEqual([
      'Could not read the status of orders: not_found',
      'Could not read the plans of orders: plans down',
      'Could not read the registered targets: targets down',
    ]);
    expect(fact('Applied')).toBe('unknown');
    expect(fact('Pending')).toBe('unknown');
    expect(fact('Provider')).toBe('unknown');
    expect(fact('Credential store')).toBe('unknown');
    expect(fact('Runs needing attention')).toBe('unknown');
  });

  it('falls back to the registered provider and names a target godwit does not list', () => {
    show({ status: failed('x'), summaries: done([{ name: 'orders', provider: 'kubernetes' }]) });
    expect(fact('Provider')).toBe('kubernetes');
    show({ summaries: done([{ name: 'other' }]) });
    expect(screen.getAllByText('Credential store')[1].nextSibling?.textContent).toBe('unknown');
  });
});
