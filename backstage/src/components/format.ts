import { AppliedMigration, Plan } from '../api/types';

export interface Actor {
  who: string;
  via: string;
}

const markers: Record<string, string> = {
  '(autoplan)': 'autoplan',
  '(report)': 'report',
};

export function formatActor(createdBy: string | undefined): Actor {
  const raw = createdBy ?? '';
  if (!raw) {
    return { who: 'unknown', via: '' };
  }
  if (raw.startsWith('github:')) {
    const rest = raw.slice('github:'.length);
    const colon = rest.indexOf(':');
    if (colon < 0) {
      return { who: rest, via: 'GitHub App' };
    }
    const repository = rest.slice(0, colon);
    const commander = rest.slice(colon + 1);
    const marker = markers[commander];
    if (marker) {
      return { who: marker, via: `GitHub App on ${repository}` };
    }
    return { who: `@${commander}`, via: `GitHub App on ${repository}` };
  }
  if (raw.startsWith('ui:')) {
    return { who: raw.slice('ui:'.length), via: 'godwit UI' };
  }
  if (raw === 'anonymous') {
    return { who: raw, via: 'unnamed token' };
  }
  return { who: raw, via: 'token' };
}

export function formatTime(value: string | undefined): string {
  if (!value) {
    return '';
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return `${date.toISOString().slice(0, 16).replace('T', ' ')} UTC`;
}

function newer(a: string, b: string): boolean {
  return a.length !== b.length ? a.length > b.length : a > b;
}

export function newestVersion(applied: AppliedMigration[] | undefined): string {
  let newest = '';
  for (const m of applied ?? []) {
    if (!m.repeatable && m.version && newer(m.version, newest)) {
      newest = m.version;
    }
  }
  return newest;
}

export function versionedCount(applied: AppliedMigration[] | undefined): number {
  return (applied ?? []).filter(m => !m.repeatable).length;
}

export interface Pending {
  planId: string;
  count: number;
}

export function pendingOf(plans: Plan[]): Pending | undefined {
  const ready = plans.find(p => p.state === 'ready');
  if (!ready) {
    return undefined;
  }
  const count = (ready.migrations ?? []).filter(m => !m.applied).length;
  return { planId: ready.id, count };
}

export function shortId(id: string | undefined): string {
  return (id ?? '').slice(0, 8);
}
