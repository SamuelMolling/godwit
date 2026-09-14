import { Grid } from '@material-ui/core';
import { EmptyState, InfoCard } from '@backstage/core-components';
import { useApi } from '@backstage/core-plugin-api';
import useAsync from 'react-use/lib/useAsync';
import { AsyncState } from 'react-use/lib/useAsyncFn';
import { godwitApiRef } from '../api/GodwitApi';
import { TargetSummary } from '../api/types';
import { DriftPanel } from './DriftPanel';
import { RunsTable } from './RunsTable';
import { TargetOverview } from './TargetOverview';

export interface GodwitTargetsViewProps {
  targets: string[];
  maxRuns?: number;
}

interface TargetCardProps {
  target: string;
  summaries: AsyncState<TargetSummary[]>;
  maxRuns: number;
}

const TargetCard = ({ target, summaries, maxRuns }: TargetCardProps) => {
  const api = useApi(godwitApiRef);
  const status = useAsync(() => api.getTargetStatus(target), [api, target]);
  const plans = useAsync(() => api.listPlans(target), [api, target]);
  const events = useAsync(() => api.listDriftEvents(target), [api, target]);
  const runs = useAsync(() => api.listRuns(target), [api, target]);

  return (
    <InfoCard title={target} subheader="godwit target">
      <Grid container direction="column" spacing={3}>
        <Grid item>
          <TargetOverview target={target} status={status} plans={plans} summaries={summaries} />
        </Grid>
        <Grid item>
          <DriftPanel target={target} events={events} status={status} />
        </Grid>
        <Grid item>
          <RunsTable target={target} runs={runs} maxRuns={maxRuns} />
        </Grid>
      </Grid>
    </InfoCard>
  );
};

export const GodwitTargetsView = ({ targets, maxRuns = 10 }: GodwitTargetsViewProps) => {
  const api = useApi(godwitApiRef);
  const summaries = useAsync(() => api.listTargets(), [api]);
  const names = [...new Set(targets.map(t => t.trim()).filter(Boolean))];

  if (names.length === 0) {
    return (
      <EmptyState
        missing="info"
        title="No godwit targets"
        description="Pass the names of the godwit targets to show."
      />
    );
  }
  return (
    <Grid container spacing={3} direction="column">
      {names.map(name => (
        <Grid item key={name}>
          <TargetCard target={name} summaries={summaries} maxRuns={maxRuns} />
        </Grid>
      ))}
    </Grid>
  );
};
