import { createApiRef } from '@backstage/core-plugin-api';
import { DriftEvent, Plan, Run, TargetStatus, TargetSummary } from './types';

export interface GodwitApi {
  getTargetStatus(target: string): Promise<TargetStatus>;
  listTargets(): Promise<TargetSummary[]>;
  listRuns(target: string): Promise<Run[]>;
  listDriftEvents(target: string): Promise<DriftEvent[]>;
  listPlans(target: string): Promise<Plan[]>;
}

export const godwitApiRef = createApiRef<GodwitApi>({ id: 'plugin.godwit.api' });
