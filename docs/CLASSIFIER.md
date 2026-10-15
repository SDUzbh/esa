# Optional human-supervised ticket classifier

The classifier is implemented in Go with the standard library. It is disabled in the shipped configuration. It supplies **multiple label suggestions for human confirmation** and never admits tickets, changes routes, calls an Agent, forwards an issue, approves a case or updates Redmine's Tracker. ESA still accepts only manually forwarded snapshots.

## Model and data contract

Each project has a separate model and a label set derived from reviewed training examples or the explicit `-labels` option. There is no preset category catalog. Every label needs positive and negative training examples. A human review supplies the complete label set, including an explicit empty array when none apply; missing labels are not interpreted as negatives.

Use UTF-8 JSONL, one reviewed ticket per line:

```json
{"id":"redmine:123","project_id":"1","text":"Subject\nDescription at forwarding time","labels":["bug","performance"],"reviewer":"engineer-login","reviewed_at":"2025-01-01T00:00:00Z"}
```

`id` must be stable across the original dataset and feedback export. Optional `group_id` groups related or duplicate tickets for train/test isolation; it defaults to `id` and must remain stable on correction. Curate exported feedback to retain any external grouping before retraining. Each dataset/model contains one project. Input parsing rejects unknown fields, absent review provenance, duplicate labels, oversized records and malformed JSON. Do not pass a raw Jira export directly: curate its subject/description snapshots and human labels into this format first. A reviewer string is provenance provided by the trusted integration/operator, not independent authentication.

Only the subject and description of the last forwarded snapshot become online features. Resolutions, comments, versions, Tracker, status, assignee, IDs, source keys, predictions and engineering outcomes are excluded. Offline `text` must follow the same snapshot contract; arbitrary hand-authored text cannot be independently proven historical by the classifier. The implementation does not edit the original ticket data.

The vocabulary contains lowercase Unicode word unigrams and adjacent bigrams. It does not segment Chinese words. Fitting uses training documents only, smoothed IDF `1 + log((1+n)/(1+df))`, sublinear TF `1 + log(count)` and L2 normalization, following the [documented TF-IDF conventions](https://scikit-learn.org/stable/modules/generated/sklearn.feature_extraction.text.TfidfTransformer.html). An independent binary Logistic Regression is fitted per label, corresponding to [multilabel one-vs-rest classification](https://scikit-learn.org/stable/modules/generated/sklearn.multiclass.OneVsRestClassifier.html). This is a native implementation, not a scikit-learn dependency or claim of solver equivalence.

Training uses deterministic full-batch gradient descent on class-balanced binary log loss with an L2 weight penalty and an unregularized intercept. The default threshold is 0.5 per label. Scores are independent, do not sum to one, and are not calibrated confidence. Text with no known features abstains with `no_known_features`. Resource limits bound examples, vocabulary candidates, model size and iteration count; Ctrl-C cancels fitting.

## Independent training, testing and prediction

Build `bin/esa-classifier` with `scripts/build_esa.sh`, or build just this command:

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go build -o bin/esa-classifier ./cmd/esa-classifier
bin/esa-classifier train \
  -data examples/classifier/train.jsonl \
  -cutoff 2025-01-01T00:00:00Z \
  -output runtime/classifier/demo-candidate.json
bin/esa-classifier test \
  -model runtime/classifier/demo-candidate.json \
  -data examples/classifier/test.jsonl
bin/esa-classifier predict \
  -model runtime/classifier/demo-candidate.json \
  -text 'Consumer crash null pointer and slow latency regression'
```

Every output model path must be new. Model files are committed atomically with mode 0600 and contain a content fingerprint, dataset fingerprint, feature parameters, label weights, training groups/text fingerprints and latest training review time. Loading validates dimensions, finite weights and content integrity. Fingerprints detect accidental corruption; they are not cryptographic signatures from a trusted model publisher.

Options include `-labels`, `-min-df`, `-max-features`, `-epochs`, `-learning-rate`, `-l2` and `-threshold`. Keep tuning data separate from final test data. Do not select a threshold using the final test metrics and then report them as an independent evaluation.

Testing rejects different projects, unknown labels, training-group overlap, duplicate predictive text and test reviews at or before the model's latest training review. Supply related/duplicate ticket groups consistently; unknown semantic duplicates cannot be automatically ruled out. The output reports per-label precision/recall/F1/support, micro precision/recall/F1, macro F1, exact label-set accuracy, Hamming loss and abstentions. The tracked examples are synthetic smoke fixtures and do not establish Jira archive accuracy.

## Optional production integration

Enable explicitly in a private workflow configuration, preserving the normal `version` and `budget`:

```json
"classifier": {
  "enabled": true,
  "models": {"1": "/absolute/path/to/tested-model.json"}
}
```

Start ESA with `-rules /absolute/path/to/config.json`. Relative model paths are resolved from the service working directory; absolute paths are recommended. Model project IDs must match their configured keys. Projects without a configured model have no classifier UI. Disabled mode does not load models or collect prediction/feedback records. Explicitly enabled invalid models fail startup. Models are loaded at startup and immutable while serving; candidate promotion requires an operator-selected configuration/file and service restart. No live retraining or automatic promotion occurs.

For Compose, use a local override to mount the tested model read-only and the private configuration at `/etc/esa/rules.json`; configure model paths as container paths. The service defaults to UID 1000, configurable through `ESA_UID`/`ESA_GID`; ensure the mount is readable by the configured identity through appropriate ownership/access rather than publishing training data. Model and feedback artifacts belong in ignored `runtime/` or `reports/`, not the public repository. Rebuild the application/plugin images to use new code; no deployment is performed by training commands.

On a forwarded ticket's ESA detail page, a user with **Submit ESA** can click **Suggest labels**. This computes a local prediction for the last forwarded subject/description; merely opening the page or receiving an event does not run the classifier. A user with **Review ESA** confirms the complete label set or corrects errors and supplies a review note. Boxes are not initially preselected from machine output, so predictions do not become training truths by default. After a prior human review, the previously confirmed labels are preselected for amendment.

New predictions are idempotent for the exact forwarded snapshot and model. Feedback stores immutable text, model/prediction identity, human labels, reviewer, note and server review timestamp in ESA's selected state adapter (single-writer JSON or the transactional Redmine API). Reusing a feedback ID with different content is rejected. Feedback for a changed snapshot or different model is rejected; generate fresh suggestions first. Private/unforwarded/invisible tickets and out-of-scope projects cannot be predicted, reviewed or exported. The plugin enforces Redmine roles and supplies the current login; trusted service-token holders must enforce equivalent authorization.

The model's latest training review must be no later than the ticket's forwarding time, preventing a later-trained model from classifying an older replay snapshot. `created_at` alone is not the feature cutoff: the accepted manual forwarding defines the available snapshot. Historical replay operators must freeze the actual historical subject/description and use time-appropriate models.

## APIs and continuous learning

All endpoints require the existing Bearer service token and explicit authorized `X-ESA-Project-IDs`; the Redmine client also supplies visible `X-ESA-Ticket-IDs`.

| Method and path | Purpose |
| --- | --- |
| `GET /v1/tickets/{id}/classifier` | Enabled status, label set, model version, current prediction and human feedback |
| `POST /v1/tickets/{id}/classifier/predict` | Explicitly request local suggestions |
| `POST /v1/tickets/{id}/classifier/feedback` | Record `id`, `prediction_id`, explicit `labels`, `reviewer`, `note` |
| `GET /v1/classifier/feedback?before=<RFC3339>` | JSONL export of the newest visible human review per ticket at a required nonfuture cutoff |

The ordinary ticket detail response includes classifier data only when explicitly enabled; list/routing/report contracts remain unchanged. Disabled prediction, feedback and export requests return 409; status returns `enabled: false`. Feedback export excludes private or currently invisible tickets and reviews after the cutoff. It never emits unlabeled machine predictions. Keep the full state store for audit; the training export contains the latest human examples only.

To collect feedback, request the export through an authorized service client and save its JSONL response privately. For example, an operator can use:

```sh
# Set the token privately; do not put its literal value in shell history.
curl --fail --silent --show-error --get \
  -H "Authorization: Bearer $ESA_API_TOKEN" \
  -H 'X-ESA-Project-IDs: 1' \
  --data-urlencode 'before=2026-10-12T00:00:00Z' \
  http://127.0.0.1:8090/v1/classifier/feedback \
  > runtime/classifier/feedback.jsonl
bin/esa-classifier retrain \
  -data runtime/classifier/human-training.jsonl \
  -feedback runtime/classifier/feedback.jsonl \
  -cutoff 2026-10-12T00:00:00Z \
  -output runtime/classifier/candidate-updated.json
bin/esa-classifier test \
  -model runtime/classifier/candidate-updated.json \
  -data runtime/classifier/heldout-after-cutoff.jsonl
```

Retraining replaces each ticket's old label set with its newest human review at the cutoff, preserving grouping. Conflicting reviews at the same timestamp are rejected. Vocabulary, IDF and all label models are fully refitted; this avoids mixing incompatible feature spaces through partial updates. Original datasets and active models remain untouched. Repeat export → retrain → independent test → manual promotion as feedback accumulates. Keep all related test tickets out of both the base dataset and correction export. This provides supervised continuous learning without an autonomous feedback loop or Agent token cost.

If an export contains no feedback, skip retraining: an empty JSONL dataset is rejected. For larger archives, evaluate the implemented transactional adapter and its whole-state size limit before enabling broad feedback collection; it does not yet provide scalable per-record queries or row-level claims. Ticket edits and privacy changes still require forwarding/reconciliation to reach the backend; this module does not add automatic ingestion.
