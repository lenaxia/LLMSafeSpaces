# Chart Upgrade Notes

This document captures upgrade-time procedures and known operational gotchas for the `llmsafespaces` Helm chart. It covers things that `helm upgrade` does NOT handle automatically and that operators must know to perform manually when applicable.

For configuration-rotation procedures (KEK rotation, etc.), see `KEK-ROTATION.md`.

---

## Grafana dashboard UID stability

The chart ships two Grafana dashboards via the `dashboards-configmap.yaml` template, picked up by the Grafana sidecar provisioner via the `grafana_dashboard: "1"` ConfigMap label.

The dashboards are identified by their top-level `uid` field (NOT by filename or by Grafana's internal numeric ID). The current UIDs are:

- `llmsafespaces-operational` — operational health dashboard
- `llmsafespaces-billing` — billing & metering dashboard

The chart_test `TestMonitoring_DashboardUIDsAreStable` pins these values and prevents accidental changes.

### Why UIDs matter

The dashboard UID is the only stable contract between operator-facing URLs and the dashboard itself. Operators bookmark URLs of the form:

```
https://grafana.example.com/d/llmsafespaces-operational/llmsafespaces-operational
```

If a chart upgrade renames the UID, the bookmark breaks (Grafana returns 404 or shows a different unrelated dashboard if the new UID happens to collide with something else). Worse, the **old dashboard with the old UID** stays in Grafana's database — the sidecar provisioner does NOT garbage-collect rows whose source files vanished. Operators end up looking at the stale dashboard with stale, release-mismatched PromQL queries, while the new dashboard sits unused at a different URL.

### What was discovered during worklog 0522 incident response

Worklog 0522 documented "No data" on Grafana dashboards from the URL-template-variable angle (stale `var-controller_job=` in bookmarked URLs). During incident response, a deeper failure mode was uncovered that this document captures:

The chart was previously named `llmsafespace` (singular) and shipped dashboards with UIDs `llmsafespace-operational` and `llmsafespace-billing`. After the chart was renamed to `llmsafespaces` (plural), the dashboard UIDs followed the chart name and became `llmsafespaces-operational` / `llmsafespaces-billing`.

Result on the production cluster:
- The old singular UIDs lingered in Grafana's `resource` table (the new unified-storage backend Grafana 11+ uses)
- The new plural UIDs were what the sidecar provisioner tried to write
- Grafana's optimistic-concurrency check saw "found 2 with same internal hash, desired 1" and refused to write either
- Dashboards persistently showed "No data" because:
  - Operators' bookmarks pointed at the singular UIDs (the stale variant)
  - The stale variant had stale `job=~"llmsafespaces-api"` matchers (now-renamed plural form) that did not match the cluster's actual `job=llmsafespace-api` ServiceMonitor labels
  - The sidecar couldn't update either copy

The fix required: SQL-deleting the orphan singular rows, then scaling Grafana's deployment to 0 and back up to clear in-memory replication state across the three Grafana pods. **This was preventable** — see the procedures below.

### When to NOT change a dashboard UID

The UIDs should NEVER change unless there's a deliberate, operator-coordinated migration. Specifically:

- **Don't change the UID when renaming the chart.** If the chart goes from `llmsafespaces` to some other name, the dashboards' UIDs should stay `llmsafespaces-operational` / `llmsafespaces-billing` (or whatever the originals were). UIDs are a separate identifier from the chart name.
- **Don't change the UID when reorganizing dashboards.** Even if a dashboard's content is rewritten or replaced, keep its UID.
- **Don't change the UID when promoting a dashboard from preview to GA.** That's a content change, not an identity change.

The only legitimate reason to change a UID is if the original UID was already broken in production AND the migration cost is acceptable. In that case follow the procedure below.

### Procedure for an intentional UID change

If a UID change is genuinely required:

1. **Update the dashboard JSON's top-level `"uid"` field** in `helm/dashboards/<dashboard>.json`.
2. **Update the chart_test pin** in `helm/chart_test.go` (function `TestMonitoring_DashboardUIDsAreStable`, the `expectedUIDs` map). The test will FAIL until you do this — that's the guardrail working as designed.
3. **Update the script's expected UID list** in `helm/scripts/grafana-purge-stale-dashboards.sh` (variable `EXPECTED_UIDS`).
4. **Notify operators** that bookmarked URLs will break after the next chart upgrade.
5. **After the chart upgrade lands**, run the purge script (below) to remove the old UID rows from Grafana so the sidecar provisioner doesn't trip the optimistic-concurrency check.

### Manual cleanup procedure (after a UID change OR after observing the symptoms below)

Symptoms that warrant cleanup:
- Grafana logs contain `"unexpected number of dashboards for id ...: found 2, desired 1"` for files in `/tmp/dashboards/`
- Dashboards persistently show "No data" on most panels despite Prometheus having metric series
- A previous chart version with different UIDs is known to have been deployed on this cluster

Cleanup steps:

1. **Locate the Grafana admin password** (commonly stored in a Kubernetes secret named `grafana-admin-creds` or similar):

   ```sh
   GRAFANA_PASS=$(kubectl get secret -n monitoring grafana-admin-creds \
       -o jsonpath='{.data.admin-password}' | base64 -d)
   ```

2. **Copy the purge script into the Grafana pod** (it depends only on `sh`, `curl`, `grep`, `sed`, `sort` — no python/jq):

   ```sh
   GRAFANA_POD=$(kubectl get pod -n monitoring \
       -l app.kubernetes.io/name=grafana -o jsonpath='{.items[0].metadata.name}')
   kubectl cp helm/scripts/grafana-purge-stale-dashboards.sh \
       monitoring/${GRAFANA_POD}:/tmp/purge.sh -c grafana
   ```

3. **Run dry-run first** to see what would be deleted:

   ```sh
   kubectl exec -n monitoring -c grafana deploy/grafana -- sh -c "
       export GRAFANA_URL=http://localhost:3000
       export GRAFANA_USER=admin
       export GRAFANA_PASS='${GRAFANA_PASS}'
       sh /tmp/purge.sh"
   ```

   The script lists every dashboard whose UID begins with `llmsafespace-` (singular) or `llmsafespaces-` (plural), then highlights the orphans (UIDs in Grafana but NOT in the chart's expected set).

4. **Apply** if the dry-run output looks correct:

   ```sh
   kubectl exec -n monitoring -c grafana deploy/grafana -- sh -c "
       export GRAFANA_URL=http://localhost:3000
       export GRAFANA_USER=admin
       export GRAFANA_PASS='${GRAFANA_PASS}'
       sh /tmp/purge.sh --apply"
   ```

5. **If the sidecar provisioner is still in a bad state**, scale Grafana down and back up to clear in-memory replication state:

   ```sh
   ORIGINAL=$(kubectl get deploy -n monitoring grafana -o jsonpath='{.spec.replicas}')
   kubectl scale -n monitoring deploy/grafana --replicas=0
   sleep 10
   kubectl scale -n monitoring deploy/grafana --replicas=${ORIGINAL}
   ```

   This is also documented in `MONITORING-OPERATIONAL.md` along with the underlying multi-replica race condition.

---

## Other upgrade procedures

(Currently only the dashboard UID procedure is documented here. Add additional sections as new upgrade-time gotchas are discovered.)
