# backstage-plugin-godwit

A read-only view of [godwit](https://github.com/SamuelMolling/godwit) targets for Backstage: applied and
pending migrations, the version each target is at, its credential store, drift with the diff of every open
event and the history of resolved ones, and recent runs. It calls godwit through the Backstage proxy with a
`read` token, and has no action that changes anything.

## Install

```bash
yarn --cwd packages/app add backstage-plugin-godwit
```

```tsx
import { godwitPlugin } from 'backstage-plugin-godwit';

const app = createApp({ apis, plugins: [godwitPlugin] });
```

On the godwit service, add a `read` token to `GODWIT_TOKENS`, e.g. `backstage:read:<secret>`. In
`app-config.yaml`:

```yaml
proxy:
  endpoints:
    '/godwit':
      target: 'https://godwit.internal.example.com'
      credentials: require
      allowedMethods: ['POST']
      headers:
        Authorization: 'Bearer ${GODWIT_BACKSTAGE_TOKEN}'
```

## Use

On a catalog entity, name the targets:

```yaml
metadata:
  annotations:
    godwit/targets: orders-staging, orders-production
```

```tsx
import { EntityGodwitContent, isGodwitAvailable } from 'backstage-plugin-godwit';

<EntityLayout.Route path="/godwit" title="Migrations" if={isGodwitAvailable}>
  <EntityGodwitContent />
</EntityLayout.Route>
```

On any other page, pass the targets directly; no entity is needed:

```tsx
import { GodwitTargetsView } from 'backstage-plugin-godwit';

<GodwitTargetsView targets={['orders-production']} />
```

The full guide — the proxy's reach, a custom proxy path, what each field means — is
[docs/use/backstage.md](https://github.com/SamuelMolling/godwit/blob/main/docs/use/backstage.md).
