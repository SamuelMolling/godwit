import { ReactNode, ComponentType } from 'react';
import { Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@material-ui/core';
import {
  Progress,
  StatusAborted,
  StatusError,
  StatusOK,
  StatusPending,
  StatusRunning,
  StatusWarning,
} from '@backstage/core-components';
import { AsyncState } from 'react-use/lib/useAsyncFn';
import { Run, RunState } from '../api/types';
import { formatActor, formatTime, shortId } from './format';
import { Section, Unavailable } from './Section';

const indicators: Partial<Record<RunState, ComponentType<{ children?: ReactNode }>>> = {
  RUN_STATE_QUEUED: StatusPending,
  RUN_STATE_RUNNING: StatusRunning,
  RUN_STATE_SUCCEEDED: StatusOK,
  RUN_STATE_FAILED: StatusError,
  RUN_STATE_NEEDS_ATTENTION: StatusWarning,
  RUN_STATE_AWAITING_CONTRACT: StatusPending,
  RUN_STATE_REVERTED: StatusAborted,
};

export function stateLabel(state: RunState | undefined): string {
  return (state ?? 'RUN_STATE_UNSPECIFIED').replace('RUN_STATE_', '').replace(/_/g, ' ').toLowerCase();
}

export function kindLabel(run: Run): string {
  const kind = run.reverts ? `revert of ${shortId(run.reverts)}` : run.kind || 'migrate';
  return run.phase ? `${kind} (${run.phase})` : kind;
}

const State = ({ run }: { run: Run }) => {
  const Indicator = run.state && indicators[run.state];
  const label = stateLabel(run.state);
  return (
    <>
      {Indicator ? <Indicator>{label}</Indicator> : label}
      {run.error && (
        <Typography variant="caption" color="textSecondary" component="div">
          {run.error}
        </Typography>
      )}
    </>
  );
};

const CreatedBy = ({ run }: { run: Run }) => {
  const actor = formatActor(run.createdBy);
  return (
    <span title={run.createdBy}>
      {actor.who}
      {actor.via && (
        <Typography variant="caption" color="textSecondary" component="div">
          {actor.via}
        </Typography>
      )}
    </span>
  );
};

export interface RunsTableProps {
  target: string;
  runs: AsyncState<Run[]>;
  maxRuns: number;
}

export const RunsTable = ({ target, runs, maxRuns }: RunsTableProps) => {
  let body: ReactNode;
  if (runs.loading) {
    body = <Progress />;
  } else if (runs.error) {
    body = <Unavailable what={`the runs on ${target}`} error={runs.error} />;
  } else if (!runs.value?.length) {
    body = <Typography variant="body2">No run has been created on {target}.</Typography>;
  } else {
    body = (
      <Table size="small" aria-label={`runs on ${target}`}>
        <TableHead>
          <TableRow>
            <TableCell>Run</TableCell>
            <TableCell>State</TableCell>
            <TableCell>Kind</TableCell>
            <TableCell>Created by</TableCell>
            <TableCell>Created</TableCell>
            <TableCell>Finished</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {runs.value.slice(0, maxRuns).map(run => (
            <TableRow key={run.id}>
              <TableCell title={run.id}>
                <code>{shortId(run.id)}</code>
              </TableCell>
              <TableCell>
                <State run={run} />
              </TableCell>
              <TableCell>{kindLabel(run)}</TableCell>
              <TableCell>
                <CreatedBy run={run} />
              </TableCell>
              <TableCell>{formatTime(run.createdAt)}</TableCell>
              <TableCell>{formatTime(run.finishedAt)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    );
  }
  return <Section title="Recent runs">{body}</Section>;
};
