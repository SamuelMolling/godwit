import { formatActor, formatTime, newestVersion, pendingOf, shortId, versionedCount } from './format';

describe('formatActor', () => {
  it.each([
    ['github:acme/orders:alice', { who: '@alice', via: 'GitHub App on acme/orders' }],
    ['github:acme/orders:(autoplan)', { who: 'autoplan', via: 'GitHub App on acme/orders' }],
    ['github:acme/orders:(report)', { who: 'report', via: 'GitHub App on acme/orders' }],
    ['github:acme/orders', { who: 'acme/orders', via: 'GitHub App' }],
    ['ui:oncall', { who: 'oncall', via: 'godwit UI' }],
    ['anonymous', { who: 'anonymous', via: 'unnamed token' }],
    ['ci', { who: 'ci', via: 'token' }],
    ['', { who: 'unknown', via: '' }],
    [undefined, { who: 'unknown', via: '' }],
  ])('renders %p', (raw, want) => {
    expect(formatActor(raw)).toEqual(want);
  });
});

describe('formatTime', () => {
  it('prints UTC to the minute and passes through what it cannot parse', () => {
    expect(formatTime('2026-09-08T10:14:06.793Z')).toBe('2026-09-08 10:14 UTC');
    expect(formatTime('yesterday')).toBe('yesterday');
    expect(formatTime(undefined)).toBe('');
  });
});

describe('versions', () => {
  const applied = [
    { version: '20260101000000', name: 'a' },
    { version: '9', name: 'short' },
    { version: '20260301000000', name: 'c' },
    { version: '20260201000000', name: 'b' },
    { name: 'views', repeatable: true },
    {},
  ];

  it('takes the newest versioned migration, comparing int64 strings numerically', () => {
    expect(newestVersion(applied)).toBe('20260301000000');
    expect(newestVersion([{ version: '10' }, { version: '9' }])).toBe('10');
    expect(newestVersion(undefined)).toBe('');
  });

  it('counts versioned migrations only', () => {
    expect(versionedCount(applied)).toBe(5);
    expect(versionedCount(undefined)).toBe(0);
  });
});

describe('pendingOf', () => {
  it('counts the unapplied migrations of the newest ready plan', () => {
    expect(
      pendingOf([
        { id: 'bound', state: 'bound', migrations: [{ applied: false }] },
        { id: 'ready-new', state: 'ready', migrations: [{ applied: true }, { applied: false }, {}] },
        { id: 'ready-old', state: 'ready', migrations: [] },
      ]),
    ).toEqual({ planId: 'ready-new', count: 2 });
    expect(pendingOf([{ id: 'p', state: 'ready' }])).toEqual({ planId: 'p', count: 0 });
    expect(pendingOf([{ id: 'p', state: 'superseded' }])).toBeUndefined();
  });
});

describe('shortId', () => {
  it('keeps eight characters', () => {
    expect(shortId('0123456789')).toBe('01234567');
    expect(shortId(undefined)).toBe('');
  });
});
