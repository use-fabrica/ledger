# Single tenant per deployment

One ledger deployment serves exactly one app; there are no tenant columns and no tenant-scoping in queries. "Pluggable into many apps" means many apps can each run their own deployment. Multi-tenancy was rejected for v1 because it pollutes every table, index, and query, and because running N deployments is operationally cheap compared to retrofitting tenancy later.
