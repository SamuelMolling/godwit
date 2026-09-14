import { DiscoveryApi, FetchApi } from '@backstage/core-plugin-api';
import { GodwitApi } from './GodwitApi';
import { DriftEvent, Plan, Run, TargetStatus, TargetSummary } from './types';

export const DEFAULT_PROXY_PATH = '/godwit';

export class GodwitRequestError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = 'GodwitRequestError';
    this.status = status;
    this.code = code;
  }
}

export interface GodwitClientOptions {
  discoveryApi: DiscoveryApi;
  fetchApi: FetchApi;
  proxyPath?: string;
}

function normalizePath(path: string | undefined): string {
  const trimmed = (path ?? '').trim().replace(/\/+$/, '');
  if (!trimmed) {
    return DEFAULT_PROXY_PATH;
  }
  return trimmed.startsWith('/') ? trimmed : `/${trimmed}`;
}

function parseBody(text: string): { code?: unknown; message?: unknown } | null {
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

async function errorOf(res: Response): Promise<GodwitRequestError> {
  const body = parseBody(await res.text());
  if (typeof body?.message === 'string') {
    return new GodwitRequestError(res.status, String(body.code ?? ''), body.message);
  }
  const reason = res.statusText ? `${res.status} ${res.statusText}` : `${res.status}`;
  return new GodwitRequestError(res.status, '', `godwit request failed: ${reason}`);
}

export class GodwitClient implements GodwitApi {
  private readonly discoveryApi: DiscoveryApi;
  private readonly fetchApi: FetchApi;
  private readonly proxyPath: string;

  constructor(options: GodwitClientOptions) {
    this.discoveryApi = options.discoveryApi;
    this.fetchApi = options.fetchApi;
    this.proxyPath = normalizePath(options.proxyPath);
  }

  async getTargetStatus(target: string): Promise<TargetStatus> {
    return this.call<TargetStatus>('GetTargetStatus', { target });
  }

  async listTargets(): Promise<TargetSummary[]> {
    const res = await this.call<{ targets?: TargetSummary[] }>('ListTargets', {});
    return res.targets ?? [];
  }

  async listRuns(target: string): Promise<Run[]> {
    const res = await this.call<{ runs?: Run[] }>('ListRuns', { target });
    return res.runs ?? [];
  }

  async listDriftEvents(target: string): Promise<DriftEvent[]> {
    const res = await this.call<{ events?: DriftEvent[] }>('ListDriftEvents', { target });
    return res.events ?? [];
  }

  async listPlans(target: string): Promise<Plan[]> {
    const res = await this.call<{ plans?: Plan[] }>('ListPlans', { target });
    return res.plans ?? [];
  }

  private async call<T>(method: string, body: object): Promise<T> {
    const base = await this.discoveryApi.getBaseUrl('proxy');
    const res = await this.fetchApi.fetch(
      `${base}${this.proxyPath}/godwit.v1.GodwitService/${method}`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
    );
    if (!res.ok) {
      throw await errorOf(res);
    }
    return (await res.json()) as T;
  }
}
