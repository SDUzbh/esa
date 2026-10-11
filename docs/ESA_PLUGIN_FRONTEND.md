# ESA Redmine Frontend

The plugin follows the native Redmine layout, tables, forms, links, and role permissions. Its interface and all new development artifacts use English. The Redmine core remains unchanged.

The global **ESA** menu opens a list of processed issues visible to the signed-in user. Columns include issue ID, subject, source key, project, tracker, status, ESA state, assignee, source, update time, category, and route. Column headers sort on the server. Filters support project, tracker, status, assignee, source, category, route, ESA state, subject/source-key search, and update-time bounds. Pages contain 25 records by default.

**Upload tickets** accepts UTF-8 `.json` and `.csv` files, up to 10 MB and 2,000 records. The user selects an authorized project, previews valid rows and validation errors, then imports valid records. Existing source keys are skipped rather than overwritten. The original upload is preserved in the plugin database. Header aliases are fixed in this version; configurable mapping is future work. Source timestamps without a timezone are interpreted as UTC.

Ticket detail links back to the original Redmine issue and shows classification, route, state, missing information, candidate causes, evidence, recommendations, verification steps, limitations, provider usage, advisory token warnings, pending environment change requests, and run history. **Analyze current snapshot** submits current visible issue content before queuing analysis. Forced runs are explicit and may consume another budget.

The metadata form accepts components and affected versions separated by semicolons, an environment JSON object, an optional category override, resolution reason, cause, solution, and validation. It saves supplemental ESA metadata and retains the original upload.

Closed tickets expose case review. A trusted case needs explicit cause, solution, validation, component, affected version, and category. Approval updates applicable recommendations without extra model calls. Engineering reports expose a separate approval/rejection control while awaiting review. These actions do not close the original issue.

Enable the **ESA** project module, then grant **View ESA**, **Submit ESA**, and **Review ESA** permissions. Upload additionally requires **Add issues**. Issue visibility may be narrower than project visibility; the plugin supplies both scopes to Go. Private issues are excluded from shared handling. The service credential never appears in HTML or client requests.

The current deployment limits each authorized scope to 2,000 visible ESA issues. A database-backed authorization implementation is required before full-archive use. Git execution, release verification, configurable import mappings, and rules administration have no UI yet because their backend capabilities remain future work.
