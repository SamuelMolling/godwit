import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@material-ui/core';
import { Progress, StatusError, StatusOK, StatusWarning } from '@backstage/core-components';
import { AsyncState } from 'react-use/lib/useAsyncFn';
import { DriftEvent, TargetStatus } from '../api/types';
import { DiffBlock } from './DiffBlock';
import { formatTime } from './format';
import { Section, Unavailable } from './Section';

export interface DriftPanelProps {
  target: string;
  events: AsyncState<DriftEvent[]>;
  status: AsyncState<TargetStatus>;
}

const Legend = () => (
  <Typography variant="caption" color="textSecondary" component="p">
    <code>+</code> is in the live schema and not in the baseline; <code>-</code> is in the baseline and gone from
    the live schema.
  </Typography>
);

const OpenDrift = ({ target, open }: { target: string; open: DriftEvent[] }) => (
  <>
    <Typography variant="body1" gutterBottom>
      <StatusError>drifted</StatusError> {target} has changed outside a migration.
    </Typography>
    {open.map(event => (
      <div key={event.id}>
        <Typography variant="body2" color="textSecondary">
          Detected {formatTime(event.detectedAt)}
        </Typography>
        <DiffBlock diff={event.diff ?? ''} />
      </div>
    ))}
    <Legend />
    <Typography variant="body2" component="p">
      godwit does not undo drift. Write a migration that describes the change, or undo it by hand; if the change is
      meant to stay as it is, an operator runs <code>godwit drift accept {target}</code> to make the live schema the
      new baseline.
    </Typography>
  </>
);

const NoOpenDrift = ({ target, status }: { target: string; status: AsyncState<TargetStatus> }) => {
  const baseline = status.value?.driftBaseline;
  if (baseline) {
    return (
      <Typography variant="body1">
        <StatusOK>no drift</StatusOK> {target} matches the baseline taken {formatTime(baseline.takenAt)}.
      </Typography>
    );
  }
  if (status.value) {
    return (
      <Typography variant="body1">
        <StatusWarning>not watched</StatusWarning> {target} has no baseline yet, so godwit is not
        comparing it; one is taken after its first successful run.
      </Typography>
    );
  }
  return (
    <Typography variant="body1">
      <StatusWarning>no open drift</StatusWarning> No open drift event is recorded for {target}, but
      without its status godwit cannot say whether a baseline is being compared.
    </Typography>
  );
};

const History = ({ resolved }: { resolved: DriftEvent[] }) => (
  <Table size="small" aria-label="resolved drift">
    <TableHead>
      <TableRow>
        <TableCell>Detected</TableCell>
        <TableCell>Resolved</TableCell>
        <TableCell>Diff</TableCell>
      </TableRow>
    </TableHead>
    <TableBody>
      {resolved.map(event => (
        <TableRow key={event.id}>
          <TableCell>{formatTime(event.detectedAt)}</TableCell>
          <TableCell>{formatTime(event.resolvedAt)}</TableCell>
          <TableCell>
            <details>
              <summary>show</summary>
              <DiffBlock diff={event.diff ?? ''} />
            </details>
          </TableCell>
        </TableRow>
      ))}
    </TableBody>
  </Table>
);

export const DriftPanel = ({ target, events, status }: DriftPanelProps) => {
  if (events.loading || status.loading) {
    return (
      <Section title="Drift">
        <Progress />
      </Section>
    );
  }
  if (events.error) {
    return (
      <Section title="Drift">
        <Unavailable what={`drift for ${target}; its drift state is unknown`} error={events.error} />
      </Section>
    );
  }
  const all = events.value ?? [];
  const open = all.filter(e => !e.resolvedAt);
  const resolved = all.filter(e => e.resolvedAt);
  return (
    <Section title="Drift">
      {open.length > 0 ? <OpenDrift target={target} open={open} /> : <NoOpenDrift target={target} status={status} />}
      {resolved.length > 0 && (
        <>
          <Typography variant="subtitle2" component="h4">
            Resolved
          </Typography>
          <History resolved={resolved} />
        </>
      )}
    </Section>
  );
};
