import { DEFAULT_PROXY_PATH, GodwitClient, GodwitRequestError } from './GodwitClient';

function setup(response: Response, proxyPath?: string) {
  const fetch = jest.fn().mockImplementation(async () => response.clone());
  const client = new GodwitClient({
    discoveryApi: { getBaseUrl: jest.fn().mockResolvedValue('http://backstage/api/proxy') },
    fetchApi: { fetch },
    proxyPath,
  });
  return { client, fetch };
}

const json = (body: unknown, status = 200, statusText = '') =>
  new Response(JSON.stringify(body), { status, statusText, headers: { 'Content-Type': 'application/json' } });

describe('GodwitClient', () => {
  it('posts connect JSON to the service path under the default proxy path', async () => {
    const { client, fetch } = setup(json({ target: 'orders', provider: 'vault' }));
    await expect(client.getTargetStatus('orders')).resolves.toEqual({ target: 'orders', provider: 'vault' });
    expect(DEFAULT_PROXY_PATH).toBe('/godwit');
    expect(fetch).toHaveBeenCalledWith('http://backstage/api/proxy/godwit/godwit.v1.GodwitService/GetTargetStatus', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{"target":"orders"}',
    });
  });

  it.each([
    ['godwit-prod/', '/godwit-prod'],
    ['/a/b//', '/a/b'],
    ['  ', '/godwit'],
  ])('normalizes proxy path %p to %p', async (path, want) => {
    const { client, fetch } = setup(json({}), path);
    await client.listTargets();
    expect(fetch.mock.calls[0][0]).toBe(`http://backstage/api/proxy${want}/godwit.v1.GodwitService/ListTargets`);
  });

  it('unwraps list responses and reads omitted lists as empty', async () => {
    const run = { id: 'r1' };
    expect(await setup(json({ targets: [{ name: 'a' }] })).client.listTargets()).toEqual([{ name: 'a' }]);
    expect(await setup(json({ runs: [run] })).client.listRuns('a')).toEqual([run]);
    expect(await setup(json({ events: [{ id: '1' }] })).client.listDriftEvents('a')).toEqual([{ id: '1' }]);
    expect(await setup(json({ plans: [{ id: 'p' }] })).client.listPlans('a')).toEqual([{ id: 'p' }]);
    expect(await setup(json({})).client.listTargets()).toEqual([]);
    expect(await setup(json({})).client.listRuns('a')).toEqual([]);
    expect(await setup(json({})).client.listDriftEvents('a')).toEqual([]);
    expect(await setup(json({})).client.listPlans('a')).toEqual([]);
  });

  it('sends the target in every per-target request', async () => {
    const { client, fetch } = setup(json({}));
    await client.listRuns('orders');
    await client.listDriftEvents('orders');
    await client.listPlans('orders');
    expect(fetch.mock.calls.map(c => [c[0].split('/').pop(), c[1].body])).toEqual([
      ['ListRuns', '{"target":"orders"}'],
      ['ListDriftEvents', '{"target":"orders"}'],
      ['ListPlans', '{"target":"orders"}'],
    ]);
  });

  it('raises the connect error godwit returned', async () => {
    const { client } = setup(json({ code: 'permission_denied', message: 'ListRuns requires scope read' }, 403));
    const err = await client.listRuns('a').catch(e => e);
    expect(err).toBeInstanceOf(GodwitRequestError);
    expect(err).toMatchObject({ status: 403, code: 'permission_denied', message: 'ListRuns requires scope read' });
  });

  it('keeps a connect error without a code', async () => {
    const err = await setup(json({ message: 'boom' }, 500)).client.listRuns('a').catch(e => e);
    expect(err).toMatchObject({ status: 500, code: '', message: 'boom' });
  });

  it('names the HTTP status when something other than godwit answered', async () => {
    const html = await setup(new Response('<html>', { status: 502, statusText: 'Bad Gateway' }))
      .client.listRuns('a')
      .catch(e => e);
    expect(html).toMatchObject({ status: 502, code: '', message: 'godwit request failed: 502 Bad Gateway' });
    const unnamed = { ok: false, status: 401, statusText: '', text: async () => '{}' } as unknown as Response;
    const bare = await setup({ clone: () => unnamed } as unknown as Response).client.listRuns('a').catch(e => e);
    expect(bare.message).toBe('godwit request failed: 401');
    expect(bare.name).toBe('GodwitRequestError');
  });
});
