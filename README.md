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

If you already have a Go toolchain:

```bash
go install github.com/billkit-eu/billkit-cli/cmd/billkit@latest
```

That lands as `billkit`, the same name the other two routes install.

## Log in

```bash
billkit login            # prompts for an sk_test_… / sk_live_… key
# stored in ~/.billkit/config.json, profile chosen by key prefix
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
is called out on stderr. On Windows a file mode means nothing, because Go maps
it to one read-only attribute bit, so the config is protected there by the
permissions on `%USERPROFILE%` instead, and the CLI says so if
`BILLKIT_CONFIG_HOME` moves it outside your profile.

A live key is only ever sent over https. Plain `http://` is accepted for a
loopback host with a test key, which is the local-mock case, and refused
everywhere else.

## Log in without a prompt (scripts and CI)

Put the key in the environment, not in a flag:

```bash
export BILLKIT_API_KEY=sk_test_…
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
> Your webhook signing secret is whsec_…
```

Set that as your app's webhook secret and verify with any BillKit SDK — no ngrok
required. Filter with `--events customer.created,subscription.updated`.

## Fire a test event

```bash
billkit trigger customer.created     # makes the real test-mode API call
billkit trigger                      # list supported events
```

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
billkit events retrieve evt_123

# generic escape hatch for any route:
billkit api GET /v1/customers
billkit api POST /v1/customers --data email=ada@example.com --data name=Ada
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
go build -o billkit ./cmd/billkit
```

`package main` is at `cmd/billkit/` and the command tree is in `internal/cli/`,
which is why `go install` names the binary `billkit`.

## License

Apache-2.0
