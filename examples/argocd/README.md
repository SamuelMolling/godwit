# ArgoCD example

The hook Jobs live in [deploy/argocd](../../deploy/argocd/): [presync-job.yaml](../../deploy/argocd/presync-job.yaml) runs `godwit migrate --rollout=expand-contract` before the pods roll out, [postsync-confirm.yaml](../../deploy/argocd/postsync-confirm.yaml) runs `godwit run confirm --latest --allow-none` once they are healthy. Its [README](../../deploy/argocd/README.md) explains the ConfigMap the migrations come from, the token and the exit codes. They are not repeated here.

[application.yaml](application.yaml) is what wires them to the service:

| Application | Source | Notes |
|---|---|---|
| `godwit` | `deploy/helm/godwit` of this repository, `valuesObject` for the image and the Secret | sync-wave `-1`; the Secret (`GODWIT_MASTER_KEY`, `GODWIT_TOKENS`, `GODWIT_STORE_DSN`) is created out of band, see the [chart README](../../deploy/helm/godwit/README.md) |
| `orders` | the application's own chart | its templates carry the app, the `orders-migrations` ConfigMap and both hook Jobs copied from `deploy/argocd/` |

Order of a sync of `orders`:

```
PreSync   godwit migrate --target orders --dir /migrations --rollout expand-contract   (exit 0 on succeeded / awaiting_contract)
Sync      Deployment rolls out on the expanded schema
PostSync  godwit run confirm --latest --allow-none --target orders                    (no-op when nothing waits)
```

To use it: replace `registry.example.com/platform/godwit`, `https://github.com/example/orders.git`, `deploy/chart` and `orders`; register the target on the service (`godwit target add orders ...`) before the first sync; put one `pipeline` token per application in `GODWIT_TOKENS` and in the `orders-godwit` Secret the Jobs read.
