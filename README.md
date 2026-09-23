# billkit CLI

The command-line interface for [BillKit](https://billkit.eu) — forward live
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
tag=v0.3.0
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
# stored in ~/.billkit/config.json, profile named after the key prefix
billkit login --profile staging   # ...or after a name you choose
billkit config list      # see the profiles; * marks the default
billkit config use test  # switch the default without deleting credentials
billkit logout           # remove the default profile (or --profile NAME / --all)
```

The prompt does not echo what you type, so a pasted live key does not end up in
your scrollback or in a screen recording. Echo is only turned off when stdin is
a terminal, so `cat key.txt | billkit login` and CI redirection keep working.

The profile you just logged into becomes the default, so the mode `login`
reports is the mode your next command runs in. The config file is written
atomically and re-tightened to 0600 on every save; a loose-permission config
is called out on stderr. The directory is only tightened when it is one the
CLI owns, namely `~/.billkit` or one it just created, so pointing
`BILLKIT_CONFIG_HOME` at a directory of your own never changes its mode; you
get a warning instead. On Windows a file mode means nothing, because Go maps
it to one read-only attribute bit, so the config is protected there by the
permissions on `%USERPROFILE%` instead, and the CLI says so if
`BILLKIT_CONFIG_HOME` moves it outside your profile.

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

Set that as your app's webhook secret and verify with any BillKit SDK — no ngrok
required. Filter with `--events customer.created,subscription.updated`.

Forwarded requests carry the same headers a real delivery does:
`BillKit-Signature`, `BillKit-Event-Id`, `BillKit-Event-Type` and
and `BillKit-Delivery-Attempt`, so a handler that deduplicates on the event id,
or logs the attempt count, behaves here exactly as it will in production. The
`User-Agent` names both the dispatcher and this CLI, so your logs can still
tell a relayed event from a live one.

A forward that never reaches your app at all (connection refused, or a
timeout, which is what a dev server restarting on a file save looks like) is
retried three times over about four seconds. An HTTP response of any status is
final: a 500 is your handler's answer, and re-posting it would manufacture
duplicates a real endpoint would not get. If all three attempts fail, the
event id is printed with `billkit events retrieve` as the way to read it
back.

The stream reconnects itself and resumes from the last event it forwarded, so
anything recorded while it was down is replayed before the live tail continues
— in order, once, and under the same `--events` filter. If that resume point
has aged out of your event log, `listen` says so and names
`billkit events list` as the reconcile step instead of skipping quietly.

## Fire a test event

```bash
billkit trigger customer.created     # makes the real test-mode API call
billkit trigger                      # list supported events
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

# a single off-session charge (no subscription, no saved mandate)
billkit checkout one-shot \
  --customer cus_123 --amount 1999 --method ideal \
  --success-url https://example.com/thanks
billkit checkout retrieve osp_123
```

`--method` is one of `creditcard`, `directdebit`, `ideal`, `bancontact`, `eps`,
`applepay`, `paypal`, `banktransfer`. `--tax-behavior inclusive|exclusive` says
whether `--amount` is quoted gross or net; omit it to inherit the country
default, and read the response's `amount_cents` for what was actually charged.

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
billkit events list --limit 20 --starting-after evt_123   # page back through the log
billkit events retrieve evt_123

# generic escape hatch for any route:
billkit api GET /v1/customers
billkit api POST /v1/customers --data email=ada@example.com --data name=Ada

# --data holds flat values only. Arrays and nested objects (an API key's
# scopes, a webhook endpoint's enabled_events, any resource's metadata) need
# the whole body as JSON, inline, from @file, or from - for stdin:
billkit api POST /v1/api_keys --json '{"name":"ci","scopes":["events:read"]}'
billkit api POST /v1/webhook_endpoints --json @endpoint.json
```

`billkit api` takes `--idempotency-key` too, and keys anything that isn't a GET.

## Output & shell completion

JSON output is pretty-printed and colorized when stdout is a terminal; piping
turns color off automatically. Force it with `--color always|never|auto`
(`NO_COLOR` is also honored). API errors render the envelope's `code`, HTTP
status, `message`, `reason`, and `param`.

```bash
billkit completion zsh > "${fpath[1]}/_billkit"   # bash | zsh | fish | powershell
```

## Development

```bash
go test ./...
go vet ./...
golangci-lint run
go build -o billkit ./cmd/billkit
```

`package main` is at `cmd/billkit/` and the command tree is in `internal/cli/`,
which is why `go install` names the binary `billkit`.

### Exit codes

| Code | Meaning |
|------|---------|
| 0 | The command succeeded. |
| 1 | The command ran and the call failed (an API error, a refused transport, a timeout). |
| 2 | Bad invocation: an unknown or malformed flag, or the wrong number of arguments. |

A script can use that to tell a mistake it should stop and fix from a call it
may be worth retrying.

## License

Apache-2.0
