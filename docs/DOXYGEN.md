# Code comments and Doxygen

From the repository root, build the code reference:

```sh
./scripts/build_docs.sh
```

From another working directory, use an absolute path to the script.
It requires Go 1.27+ and a separately installed Doxygen (`brew install doxygen`
on macOS). The current configuration is verified with Doxygen 1.18.0. Graphviz,
Python packages, Agent access and application deployment are not required.
`ESA_GO_BIN` can select a Go executable; otherwise the local toolchain or `go`
from PATH is used.

The script builds the Go input adapter, recreates its owned `.tools/doxygen/`
output, generates HTML and XML, and fails on Doxygen documentation warnings.
Outputs are ignored build artifacts:

| Path | Content |
| --- | --- |
| `.tools/doxygen/html/index.html` | Browsable entry point |
| `.tools/doxygen/xml/index.xml` | Machine-readable documentation index |
| `.tools/doxygen/warnings.log` | Doxygen diagnostics; empty on a clean build |
| `.tools/doxygen-go` | Built Go input adapter |

## Language coverage

Doxygen's [supported parsers and input-filter configuration](https://www.doxygen.nl/manual/config.html)
do not include Go or Ruby. `tools/doxygen-go` uses Go's standard AST parser to
extract package comments, types, functions/methods and documented values into
Doxygen reference pages. Each declaration retains Go syntax and a source
path/line; function bodies are omitted. Both exported and internal declarations
are listed so workflow implementation remains discoverable. Existing GoDoc
comments continue to work with Go tools.

This adapter does not create C++ declarations, infer types across packages,
resolve build tags or generate call graphs. Go entries are under Related Pages,
not the native class/function index. The AST source line is authoritative for
the documented declaration; generated page line numbers are not source lines.

Python scripts and the Java SootUp launcher use native Doxygen parsing. Python
docstrings are interpreted as documentation through `PYTHON_DOCSTRING = NO`.
Ruby source comments remain native Ruby comments; a separate Redmine integration
page documents controllers, forwarding, storage and browser behavior. Ruby and
browser JavaScript symbols are not automatically indexed.

Inputs are allowlisted in `Doxyfile`. Test files, caches, credentials, Jira
exports, product checkouts, runtime results and generated reports are excluded.
The standalone `system_replay` module contributes reference pages without an
import or build dependency on ESA.

## Comment conventions

Use English and describe the contract rather than restating the implementation:

- Explain inputs, outputs, state changes and relevant failure conditions.
- Record repeatability requirements, ownership, leases and transaction boundaries.
- Separate reviewed trust, citation provenance, reproduction and causal evidence.
- Describe environment cleanup and what happens on cancellation or failed setup.
- For persisted/public structures, explain identity, time, scope and revision meaning.

Go comments sit immediately above the declaration and start with its name:

```go
// ProcessOne claims at most one eligible task and invokes its provider outside
// state transactions. Stale lease owners cannot publish a result.
// Returns whether work was selected plus any processing error.
func (e *Engine) ProcessOne(ctx context.Context, analyzer Analyzer) (bool, error)
```

Use ordinary GoDoc prose for parameter/result contracts. Page-level Doxygen
commands such as `@note` can be used where useful; native `@param` validation
does not apply to Go declaration pages. Use Javadoc `@param`/`@throws` for Java,
and function/class docstrings for Python. Do not add placeholder comments solely
to make a coverage number grow. The current pass documents core boundaries;
it is not a claim that every helper or field has a complete contract comment.

## Verification and cleanup

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go test ./tools/doxygen-go
./scripts/build_docs.sh
git diff --check
```

The adapter tests check generics, receiver/variadic signatures, source locations,
body omission, malformed input and source immutability. Generation parses source
without importing documented packages, starting services or calling models.
Clean `.tools/doxygen/` and `.tools/doxygen-go` when generated output is no longer
needed. Keep `Doxyfile`, the adapter, build script and source comments in Git.
