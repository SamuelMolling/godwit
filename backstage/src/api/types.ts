export type RunState =
  | 'RUN_STATE_UNSPECIFIED'
  | 'RUN_STATE_QUEUED'
  | 'RUN_STATE_RUNNING'
  | 'RUN_STATE_SUCCEEDED'
  | 'RUN_STATE_FAILED'
  | 'RUN_STATE_NEEDS_ATTENTION'
  | 'RUN_STATE_AWAITING_CONTRACT'
  | 'RUN_STATE_REVERTED';

export interface Run {
  id: string;
  target?: string;
  state?: RunState;
  error?: string;
  attempts?: number;
  createdAt?: string;
  finishedAt?: string;
  rollout?: string;
  phase?: string;
  reverts?: string;
  kind?: string;
  createdBy?: string;
  source?: string;
  planId?: string;
  retries?: number;
}

export interface AppliedMigration {
  version?: string;
  name?: string;
  checksum?: string;
  appliedAt?: string;
  repeatable?: boolean;
}

export interface DriftBaseline {
  takenAt?: string;
  runId?: string;
  unresolvedDrift?: boolean;
}

export interface TargetStatus {
  target?: string;
  provider?: string;
  applied?: AppliedMigration[];
  lastRun?: Run;
  driftBaseline?: DriftBaseline;
  readyPlans?: number;
  unreachable?: string;
}

export interface TargetSummary {
  name: string;
  provider?: string;
  lastRun?: Run;
  unresolvedDrift?: boolean;
  readyPlans?: number;
  appliedCount?: number;
  attentionRuns?: number;
  credentialStore?: string;
}

export interface DriftEvent {
  id: string;
  target?: string;
  diff?: string;
  detectedAt?: string;
  resolvedAt?: string;
}

export interface PlannedMigration {
  version?: string;
  name?: string;
  applied?: boolean;
  repeatable?: boolean;
}

export interface Plan {
  id: string;
  target?: string;
  state?: string;
  migrations?: PlannedMigration[];
  createdBy?: string;
  createdAt?: string;
}
