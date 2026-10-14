# ESA Redmine frontend

The English plugin follows Redmine's native layout and permissions. The ESA menu lists manually forwarded issues visible to the logged-in user, with filters, sorting and pagination. The former category/tag routing controls and Agent classification reports have been removed. When explicitly enabled, an optional ticket detail panel offers local label suggestions and human correction; it does not change forwarding or engineering routing. See [classifier operations](CLASSIFIER.md).

The link on an ordinary issue opens its ESA page even before forwarding. **Forward to ESA** submits the current snapshot through the durable outbox. The optional force choice creates a fresh diagnosis and may spend again. Only this action forwards an issue; new/edit hooks do not.

Upload accepts UTF-8 JSON/CSV, previews rows and creates Redmine issues while preserving original uploads and source-key deduplication. Import does not forward or analyze them. Metadata saves component/version/environment and resolution information locally; forward the issue separately to submit an updated snapshot.

The detail shows intake provenance, processing state, missing information, recommendations, concrete code findings, evidence, usage, advisory alerts and review controls. A forwarded closed issue becomes a case draft. Case/report approval does not close the Redmine issue.

Every forwarded open issue goes to Routing Agent, which retrieves local Issue records and decides whether to reuse an Agent analysis or engineer solution, join a matching `investigating` task, request information, or queue engineering. The English `Investigating` state is filterable and covers capacity waiting, analysis and assignment routing; at most three engineering workers run concurrently. Reuse invokes no engineering Agent. A new investigation shows **Unassigned until analysis completes**. Intake does not create an Engineering Issue. After reuse, the service links the selected Issue. After supported analysis, Engineering Agent submits a grouping request and Routing Agent decides existing/new membership before backend validation. The ticket page shows the request, final routing decision and separate stage call counts. Membership review controls appear once an Issue has been assigned.

View ESA, Submit ESA and Review ESA permissions remain required; import also requires Add issues. Issue visibility can be narrower than project visibility, and private issues are excluded from shared pages. The service token never enters browser requests. The prototype limits authorized scopes to 2,000 visible ESA issues.

The forwarding-column migration is required for the new outbox. Old automatic events remain stored but are not delivered. See [manual forwarding](MANUAL_FORWARDING.md).
