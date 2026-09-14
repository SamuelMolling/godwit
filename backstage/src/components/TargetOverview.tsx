import { ReactNode } from 'react';
import { Grid, Typography } from '@material-ui/core';
import { Progress } from '@backstage/core-components';
import { AsyncState } from 'react-use/lib/useAsyncFn';
import { Plan, TargetStatus, TargetSummary } from '../api/types';
import { newestVersion, pendingOf, shortId, versionedCount } from './format';
import { Unavailable } from './Section';

const Fact = ({ label, children }: { label: string; children: ReactNode }) => (
  <Grid item xs={6} md={4}>
    <Typography variant="caption" color="textSecondary" component="div">
      {label}
    </Typography>
    <Typography variant="body1" component="div">
      {children}
    </Typography>
  </Grid>
);

export interface TargetOverviewProps {
  target: string;
  status: AsyncState<TargetStatus>;
  plans: AsyncState<Plan[]>;
  summaries: AsyncState<TargetSummary[]>;
}

function credentialStore(summary: TargetSummary): string {
  if (summary.provider !== 'vault') {
    return `none (${summary.provider ?? 'unknown'} provider)`;
  }
  return summary.credentialStore || 'VAULT_ADDR';
}

export const TargetOverview = ({ target, status, plans, summaries }: TargetOverviewProps) => {
  if (status.loading || plans.loading || summaries.loading) {
    return <Progress />;
  }
  const summary = summaries.value?.find(s => s.name === target);
  const pending = plans.value && pendingOf(plans.value);
  const reachable = !!status.value && !status.value.unreachable;
  const applied = status.value?.applied;

  return (
    <>
      {status.error && <Unavailable what={`the status of ${target}`} error={status.error} />}
      {status.value?.unreachable && (
        <Typography role="alert" color="error" variant="body2">
          godwit could not read the journal of {target}: {status.value.unreachable}
        </Typography>
      )}
      {plans.error && <Unavailable what={`the plans of ${target}`} error={plans.error} />}
      {summaries.error && <Unavailable what="the registered targets" error={summaries.error} />}
      <Grid container spacing={2}>
        <Fact label="Applied">{reachable ? versionedCount(applied) : 'unknown'}</Fact>
        <Fact label="At version">
          {reachable ? newestVersion(applied) || 'none' : 'unknown'}
        </Fact>
        <Fact label="Pending">
          {plans.value === undefined && 'unknown'}
          {plans.value && !pending && 'no ready plan'}
          {pending && `${pending.count} in plan ${shortId(pending.planId)}`}
        </Fact>
        <Fact label="Provider">{status.value?.provider ?? summary?.provider ?? 'unknown'}</Fact>
        <Fact label="Credential store">{summary ? credentialStore(summary) : 'unknown'}</Fact>
        <Fact label="Runs needing attention">{summary ? summary.attentionRuns ?? 0 : 'unknown'}</Fact>
      </Grid>
    </>
  );
};
