# billkit CLI

The command-line interface for [BillKit](https://billkit.eu): forward live
events to your local app, fire test events, and hit the API from your terminal.
A single static Go binary (à la the Stripe CLI), using BillKit's own API, auth,
and `BillKit-Signature` scheme.

## Install

Homebrew, on macOS (the tap ships a cask, and casks are macOS-only):

```bash
brew tap billkit-eu/tap
brew install billkit
```

On Linux, or on macOS without Homebrew:

```bash
curl -fsSL https://raw.githubusercontent.com/billkit-eu/billkit-cli/main/install.sh | sh
```

The script installs to `/usr/local/bin`. Set `BILLKIT_INSTALL_DIR` to change
that, or `BILLKIT_VERSION` to install a specific release.

On Windows, take the `.zip` for your architecture from the
[releases page](https://github.com/billkit-eu/billkit-cli/releases) and put
`billkit.exe` on your `PATH`. macOS and Linux archives are there too, as
`.tar.gz`, and every release publishes `checksums.txt` beside them.

The macOS binaries are signed with a Developer ID Application certificate and
notarized by Apple, so none of these paths asks you to override Gatekeeper.
This CLI keeps live API keys on your machine; it should not be teaching anyone
to click past a security warning. Check for yourself:

```bash
codesign --verify --deep --strict --verbose=2 "$(command -v billkit)"
spctl -a -vvv -t install "$(command -v billkit)"
```

Every release also publishes a Sigstore signature over its `checksums.txt`, so
you can check that a download came from this project's release workflow and not
just that it arrived intact. `install.sh` does this automatically when `cosign`
is installed, and says so plainly when it is not. By hand:

```bash
tag=v1.0.0
base="https://github.com/billkit-eu/billkit-cli/releases/download/$tag"
curl -fsSLO "$base/checksums.txt" -O "$base/checksums.txt.sig" -O "$base/checksums.txt.pem"
cosign verify-blob \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity "https://github.com/billkit-eu/billkit-cli/.github/workflows/publish.yml@refs/tags/$tag" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
```

If you already have a Go toolchain:

```bash
go install github.com/billkit-eu/billkit-cli/cmd/billkit@latest
```

That lands as `billkit`, the same name the other two routes install.

## Log in

```bash
billkit login            # prompts for a bk_test_… / bk_live_… key
# profile named after the key prefix; live keys go to the OS keychain
billkit login --profile staging   # ...or after a name you choose
billkit config list      # see the profiles; * marks the default (--json for JSON)
billkit config use test  # switch the default without deleting credentials
billkit config path      # where the config file is
billkit logout           # remove the default profile (or --profile NAME / --all)
```

The prompt does not echo what you type, so a pasted live key does not end up in
your scrollback or in a screen recording. Echo is only turned off when stdin is
a terminal, so `cat key.txt | billkit login` and CI redirection keep working.

The profile you just logged into becomes the default, so the mode `login`
reports is the mode your next command runs in.

### Where credentials live

The config file is `$XDG_CONFIG_HOME/billkit/config.json` (default
`~/.config/billkit/config.json`) on macOS and Linux, the same place the Stripe
CLI uses, and `%APPDATA%\billkit\config.json` on Windows. `BILLKIT_CONFIG_HOME`
moves it to `config.json` under any directory you name. Versions before 1.0
used `~/.billkit/config.json`; the first 1.0 run moves that file (atomically,
0600), deletes the old copy, and leaves a `MOVED.txt` saying where it went.
Nothing is moved when `BILLKIT_CONFIG_HOME` is set.

Live keys (`bk_live_`) are stored in the OS keychain: the macOS Keychain,
Windows Credential Manager, or the Secret Service on Linux, under the service
name `billkit-cli`. The config file records only which entry holds the key.
Where there is no keychain, as on headless Linux or a CI runner, `login`
stores the key in the file and says so; `login --insecure-storage` does that
on purpose. Test keys always stay in the file. `logout` deletes the keychain
entry too, and `config list` masks every key.

The keychain keeps a live key out of plain files on disk and out of
dotfiles; it is not a wall against other programs you run. On macOS the entry
is created through `/usr/bin/security`, which can therefore read it back
without a prompt, and so can anything running as you that calls it.

Every change to the config (`login`, `logout`, `config use`, the move from
`~/.billkit`) holds a lock on a `.lock` file beside it, so two billkit
processes never write it at once.

The file is written atomically and re-tightened to 0600 on every save; a
loose-permission config is called out on stderr. The directory is only
tightened when it is one the CLI owns, so pointing `BILLKIT_CONFIG_HOME` at a
directory of your own never changes its mode; you get a warning instead. On
Windows a file mode means nothing, because Go maps it to one read-only
attribute bit, so the config is protected there by the permissions on your
user profile instead, and the CLI says so if `BILLKIT_CONFIG_HOME` moves it
outside your profile.

The file carries a schema `version`. Keys this build does not know are kept
on every rewrite, and a file written by a newer schema is read but never
rewritten.

A live key is only ever sent over https. Plain `http://` is accepted for a
loopback host with a test key, which is the local-mock case, and refused
everywhere else.

## Log in without a prompt (scripts and CI)

Put the key in the environment, not in a flag:

```bash
export BILLKIT_API_KEY=bk_test_…
billkit api GET /v1/customers
```

`BILLKIT_API_KEY`, `BILLKIT_BASE_URL` and `BILLKIT_PROFILE` are read by every
command, and nothing has to be written to disk first. Precedence is **flag,
then environment, then stored profile**: the flag is what you typed on this
invocation, so it wins over a variable your shell may have been carrying for
hours.

`--api-key` still works, but a flag value is visible in the process table for
the life of the call (`ps auxww`, and a world-readable `/proc/<pid>/cmdline` on
Linux), it is echoed by CI logs, and your shell appends it verbatim to
`~/.zsh_history`. Prefer the variable, or pipe the key into `billkit login`.

An environment-supplied key is held to exactly the same rules as a flag one: a
live key pointed at a plain-http host is refused before anything is dialled.

## Forward webhooks to localhost (the headline feature)

```bash
billkit listen --forward-to http://localhost:3000/billkit/webhook
```

`listen` opens a live event stream and POSTs each event to your local URL,
signed with a freshly-generated secret it prints on start:

```
> Your webhook signing secret is bkwhsec_…
```

Set that as your app's webhook secret and verify with any BillKit SDK; no ngrok
required. Filter with `--events customer.created,subscription.updated`.

Forwarded requests carry the same headers a real delivery does:
`BillKit-Signature`, `BillKit-Event-Id`, `BillKit-Event-Type` and
`BillKit-Delivery-Attempt`, so a handler that deduplicates on the event id,
or logs the attempt count, behaves here exactly as it will in production. The
`User-Agent` names both the dispatcher and this CLI, so your logs can still
tell a relayed event from a live one.

A forward that never reaches your app at all (connection refused, which is
what a dev server restarting on a file save looks like) is retried three times
over about four seconds. An HTTP response of any status is final: a 500 is
your handler's answer, and re-posting it would manufacture duplicates a real
endpoint would not get. A forward your app received but did not answer within
15 seconds is not retried either, for the same reason; it is reported as
unconfirmed. Whenever an event was not delivered, its id is printed with
`billkit events retrieve` as the way to read it back. A slow handler does not
cost you the stream: `listen` keeps it open while a forward is in flight.

The stream reconnects itself and resumes from the last event it forwarded, so
anything recorded while it was down is replayed before the live tail continues:
in order, once, and under the same `--events` filter. If that resume point
has aged out of your event log, `listen` says so and names
`billkit events list` as the reconcile step instead of skipping quietly.

## Fire a test event

```bash
billkit trigger customer.created     # makes the real test-mode API call
billkit trigger                      # list supported events, one per line
billkit trigger --json               # ...or as JSON
```

Each trigger makes the real test-mode call that emits the event, so `listen`
forwards a genuine one. Customers, products, prices, coupons and webhook
endpoints are covered, across create, update, archive and delete. Events that
need a real Mollie payment (`invoice.paid`, `payment.failed`,
`subscription.*`) are not triggerable.

## Refunds & one-shot payments

```bash
# refund a payment, a one-shot payment, or a subscription's last charge
billkit refunds create --payment pay_123 --amount 500 --reason "duplicate"
billkit refunds create --one-shot osp_123          # full refund (omit --amount)
billkit refunds list --limit 20
billkit refunds retrieve re_123

# a single one-off charge, paid by the customer on a hosted page
# (no subscription, no saved mandate)
billkit one-shots create \
  --customer cus_123 --amount 1999 --payment-method ideal \
  --success-url https://example.com/thanks
# ...or one paid inside your own page: no --payment-method, prints a client_secret
billkit one-shots create --ui-mode embedded \
  --customer cus_123 --amount 1999 \
  --success-url https://example.com/thanks
billkit one-shots list --customer cus_123
billkit one-shots retrieve osp_123
```

Amounts are integers in the currency's minor units, like the API: `--amount
1999` is EUR 19.99, or JPY 1999. The CLI shows them back formatted with the
currency's own exponent (`charge EUR 19.99 to cus_123`), reading a payment's
currency before a partial refund and falling back to "minor units" when it
cannot.

`--payment-method` is one of `creditcard`, `ideal`, `bancontact`, `eps`, `applepay`,
`paypal`, `banktransfer`. `directdebit` is refused: SEPA Direct Debit needs an
existing mandate, which a one-off charge does not have. `--tax-behavior inclusive|exclusive` says
whether `--amount` is quoted gross or net; omit it to inherit the country
default, and read the response's `amount_cents` for what was actually charged.
`--ui-mode embedded` takes no `--payment-method` (the customer picks it in the BillKit
payment element) and prints the payment with a short-lived `client_secret` to
hand to `@billkit-eu/js` instead of a `redirect_url`. Passing `--payment-method` with
it, or omitting `--payment-method` without it, is a usage error (exit 2), as is an
unknown `--payment-method`, a non-positive `--amount`, zero or two refund targets, or
a `--limit` outside 1 to 100.

Every mutating call carries an `Idempotency-Key`: yours via
`--idempotency-key`, otherwise one the CLI mints and prints on stderr before
the request, so a retry reuses it instead of spending twice. The same
`Idempotency-Key` contract every BillKit SDK uses. Connection errors,
timeouts, 429 and 5xx are retried twice with the same key, and if the outcome
is still unknown the CLI says what to check and prints the exact safe re-run.

Money commands print the resolved mode and profile on stderr first. In live
mode they ask for confirmation; with no terminal (CI, a pipe) they refuse and
ask for `--yes` rather than blocking on a prompt.

## Talk to the API

```bash
billkit events list --type customer.created
billkit events list --type invoice.paid,invoice.voided   # several types at once
billkit events list --limit 20 --starting-after evt_123   # page back through the log
billkit events retrieve evt_123

# generic escape hatch for any route:
billkit api GET /v1/customers
billkit api POST /v1/customers --data email=ada@example.com --data name=Ada

# --data holds flat values only. Arrays and nested objects (an API key's
# scopes, a webhook endpoint's enabled_events, any resource's metadata) need
# the whole body as a JSON object: --input FILE, or --input - for stdin
# (gh api's flag, with gh's meaning):
billkit api POST /v1/webhook_endpoints --input endpoint.json
echo '{"label":"ci","scopes":["events:read"]}' | billkit api POST /v1/api_keys --input -
```

`billkit api` takes `--idempotency-key` too, and keys anything that isn't a GET.

## Output contract

- **stdout** is a command's output and nothing else: API JSON from every
  command that calls the API, the table from `config list`, the path from
  `config path`, the event types from a bare `trigger`, the version line. With
  the global `--json`, `config list`, `trigger` and `version` print JSON too.
- **stderr** is every message: the mode line and live-mode banner, the
  idempotency key, the non-default-host notice, confirmations from `login`,
  `logout` and `config use`, warnings, and errors. Under `--json` the
  informational notices (the `>` lines and confirmations) are not written;
  warnings (`!` lines, the live-mode banner and its prompt, the
  outcome-unknown guidance) still are, and a failed command's JSON error is
  always the last line.
- `listen`'s one line per event is its output, on stdout
  (`HH:MM:SS  <type>  <id>`, or `  <type>  <id>  -> <status>` with
  `--forward-to`). With `--print-json`, stdout carries each event's JSON, one
  per line, and those lines move to stderr. Start-up notices, the signing
  secret, reconnects and warnings are always on stderr.

JSON is pretty-printed and colorized when stdout is a terminal; piping turns
color off. Force it with `--color always|never|auto` (`NO_COLOR` is honored).

Errors are one line on stderr: `Error: <message>`, where an API error reads
`<code> (HTTP <status>): <message> [reason: …, param: …]`. Under `--json` the
error is instead one JSON object on stderr: the API's `error` envelope plus
`http_status` and the process `exit_code`; errors the CLI raises itself have
`type: "cli_error"` and `code` `invalid_usage`, `authentication_required` or
`command_failed`.

```bash
billkit completion zsh > "${fpath[1]}/_billkit"   # bash | zsh | fish | powershell
```

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | The command succeeded (a bare group such as `billkit refunds` prints help and exits 0). |
| 1 | The command ran and failed: an API error other than 401 (including a 402 `payment_error`, the payment provider refusing a charge or refund), a network failure or timeout, a refused transport or redirect, a declined confirmation. |
| 2 | Bad invocation, nothing sent: unknown command or flag, missing required flag, a malformed or out-of-range value, an unknown profile name, or a live money command with no terminal and no `--yes`. |
| 4 | No usable credential: no key from any source, a key the API answered 401 to, something that is not a BillKit key, or a keychain entry that cannot be read. (gh uses 4 for the same.) |

`listen` runs until stopped. Ctrl-C or SIGTERM after it has reached the event
stream is a clean stop and exits 0. A key revoked while `listen` is running
ends it with the server's message and exit 4. A listener that never reached the stream
exits 1 however it ends (4 if the key was rejected), so a CI step that wraps
it cannot read "never connected" as success.

## Version, updates, telemetry

```bash
billkit version          # version, commit, build date, Go version, platform
billkit version --json
billkit --version        # the same line; -v is not a version flag
```

The CLI sends no telemetry and never checks for updates. The only hosts it
talks to are the API (`api.billkit.eu`, or `--base-url`) and, for `listen`,
the `--forward-to` address you give it. Upgrade with the route you installed
by: `brew upgrade billkit`, `install.sh` again, or `go install` again.

## Development

```bash
go test -race ./...
go vet ./...
golangci-lint run
go mod tidy -diff          # go.mod/go.sum must be tidy; the release does not tidy them
go build -o billkit ./cmd/billkit
```

`package main` is at `cmd/billkit/` and the command tree is in `internal/cli/`,
which is why `go install` names the binary `billkit`.

`test/integration` drives the built binary against a live API and skips
unless `BILLKIT_INTEGRATION_BASE_URL` is set (and the build tag is given):

```bash
BILLKIT_INTEGRATION_BASE_URL=http://127.0.0.1:8025 \
  go test -tags integration -count=1 ./test/integration/...
```

## License

Apache-2.0
