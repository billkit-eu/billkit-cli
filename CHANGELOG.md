# Changelog

All notable changes to `billkit`, the BillKit command-line interface, are
documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Versioning is independent of the SDKs and of the API.

Releases are published as GitHub releases on `billkit-eu/billkit-cli` and as a
Homebrew cask in `billkit-eu/tap`. Versions before 0.2.2 were never published.

## [Unreleased]

## [1.0.0] - 2026-10-14

Includes the work prepared as 0.5.0, which was never tagged.
1.0.0 freezes the command surface, flags, output contract, exit codes and
config format: from here on, a change to any of them is a major version.

### Upgrading from 0.x

Every breaking change, in the order you are likely to hit them:

- **Config location.** The config moves from `~/.billkit/config.json` to
  `$XDG_CONFIG_HOME/billkit/config.json` (default `~/.config/billkit/`) on
  macOS and Linux, and `%APPDATA%\billkit\config.json` on Windows. The first
  1.0 run moves it for you, deletes the old copy and leaves `MOVED.txt` in
  `~/.billkit`. Scripts that read or plant `~/.billkit/config.json` should use
  `billkit config path`, or set `BILLKIT_CONFIG_HOME`, which still wins and
  turns the move off. Downgrading to 0.x afterwards finds no config.
- **Live keys move to the OS keychain.** `login` (and the move above) put
  `bk_live_` keys in the macOS Keychain, Windows Credential Manager or the
  Linux Secret Service; the config file records only which entry. Anything
  that read a live key out of the config file will not find it there. Use
  `login --insecure-storage` to keep one in the file.
- **`checkout one-shot` is `one-shots create`, and `checkout retrieve` is
  `one-shots retrieve`.** The `checkout` group is gone, with no alias.
  `one-shots list` is new.
- **`billkit api --json <body>` is `billkit api --input <file|->`.** `--input`
  takes a file path or `-` for stdin, as `gh api --input` does; inline JSON
  and `@file` are no longer accepted. `--json` is now a global flag meaning
  "print JSON".
- **Exit codes.** A missing or rejected credential (no key, a 401, something
  that is not a BillKit key, an unreadable keychain entry) exits 4, not 1.
  A key without a `bk_test_` or `bk_live_` prefix is refused by every
  command before it is sent anywhere, not only by `login`.
  Every invalid flag value now exits 2 rather than 1: `--limit` outside 1 to
  100 (it used to be dropped silently), `--color` other than
  auto/always/never (it used to be accepted), an unknown `--type`, `--events`
  or trigger event, a non-http(s) `--forward-to`, an unknown `--payment-method`,
  `--ui-mode` or `--tax-behavior`, a non-positive `--amount`, zero or two
  refund targets, a malformed `--metadata` or `--data`, a missing required
  flag, a profile name that is not configured (`logout --profile`,
  `config use`, `--profile`), and a live-mode money command with no terminal
  and no `--yes`.
- **`one-shots create --method` is `--payment-method`**, with no alias. The
  API 1.0 renamed the one-shot field from `method` to `payment_method`, and
  the flag follows the field it sets. The values are unchanged.
- **`one-shots create --payment-method directdebit` is refused locally**
  (exit 2): the API no longer accepts it for a one-shot, because BillKit uses
  SEPA Direct Debit only for subscription renewals (it was always refused at
  run time). It is no longer listed in `--payment-method` help or completion.
- **`listen` needs API 1.0.** It sends its `--events` filter to
  `GET /v1/events/stream` as `type`, the 1.0 name for what was `types`.
- **stdout carries output only.** `login`, `logout` and `config use` print
  their confirmations on stderr, and stdout is empty. A bare `trigger` prints
  the event types one per line with no indentation, its heading on stderr.
  `config list` with no profiles prints its notice on stderr.
- **Stray arguments are refused.** `listen` and `events list` take no
  positional arguments, and one they used to ignore is now a bad invocation
  that exits 2. An unknown top-level command (`billkit notacommand`) exits 2
  too; it exited 1.
- **`config list` shows where a live key is.** A profile whose key is in the
  keychain is listed as `bk_live_… (keychain)`, not as a masked key.
- **`-v` is no longer `--version`.** Use `--version` or `billkit version`.
  The line is now `billkit version X.Y.Z (commit <sha>, built <date>,
  <go version> <os>/<arch>)`; 0.x printed `billkit version X.Y.Z`. Use
  `billkit version --json` rather than parsing it.
- **Amounts in messages.** The mode line, the live-mode banner and the
  "outcome unknown" warning show `EUR 19.99` rather than `1999 EUR` or
  `500 cents`. Anything matching those lines in a log needs updating.

### Added
- `billkit one-shots create|list|retrieve`, with `list` filtering by
  `--customer` and `--status`. The "check first" hint after an unknown
  outcome now names `billkit one-shots list --customer <id>`.
- `billkit version`, and `--version`, print the version, commit, build date,
  Go version and platform; `version --json` prints them as an object.
- A global `--json`: `config list`, the bare `trigger` and `version` print
  JSON, and a failed command writes one JSON object to stderr carrying the
  API's error envelope plus `http_status` and `exit_code`.
- `login --insecure-storage`.
- `events list --type` takes a comma-separated list
  (`--type invoice.paid,invoice.voided`), which API 1.0 accepts.
- The config file carries a schema `version`, and keys a newer billkit wrote
  are kept when this one rewrites the file.
- Release archives include `LICENSE` and `CHANGELOG.md`.

### Changed
- An unknown subcommand under any command group exits 2, as documented for
  every other bad invocation. `help` with an unknown topic exits 2 too.
- Releases are built with `-trimpath`, with the Go release `go.mod` names
  (1.26.9) rather than whichever 1.26.x the runner has, and with the commit's
  timestamp, a fixed owner and a fixed mode on every file in every archive.
  Rebuilding a tag therefore gives byte-identical Linux and Windows archives;
  the macOS binaries are signed and notarized during the release, and the
  signature makes those archives unique to each build. The module graph is no
  longer tidied on the release runner.
- macOS signing and notarization are mandatory for a release: a release
  without the credentials fails instead of shipping unsigned.

### Fixed
- Two billkit processes started at the same moment on a 0.x config (or two
  concurrent `login`s) each put the live key into the keychain under a new
  entry, and only one entry was kept: the others stayed in the keychain,
  referenced by nothing. Every change to the config now happens under a lock
  file beside it, and a failed save removes the keychain entry it made.
- When the config could not be moved to its new location (an unwritable
  `~/.config`, say), the next `login` wrote it there anyway, live key in plain
  text, and the old copy was never deleted. It now either finishes the move
  properly or saves back to `~/.billkit/config.json`, and says which.
- A run interrupted between writing the new config and deleting the old one
  left the old plain-text copy behind for good. The next run finishes the
  delete, or warns when the two files disagree.
- Logging in again, or with a config a newer billkit wrote, could overwrite
  or add a keychain entry before a save that then failed or was refused.
- Shell completion for `config use` no longer moves a 0.x config or touches
  the keychain.
- Under `--json`, informational notices (`> Using API host …`, the mode and
  idempotency-key lines, confirmations) are no longer written to stderr, so a
  failed command's stderr ends with the one JSON line.
- `api --input=` with an empty value sent the call with no body, and an
  unreadable `--input` file exited 1; both are bad invocations and exit 2.
- `listen` stopped (Ctrl-C, or a CI timeout's SIGTERM) before the server
  first answered exited 0. A listener that never reached the stream exits 1.
- The live-mode confirmation for a one-off charge showed the amount in minor
  units (`charge 1999 EUR` for EUR 19.99). Every amount the CLI prints is now
  formatted with the currency's exponent.
- `--limit` out of range was silently dropped, so the API's default page read
  as the one asked for.
- API calls follow a redirect only to an `https` address, never down to plain
  `http`, and drop the `Authorization` header as soon as a redirect leaves the
  original host and port. Invoice and credit-note PDFs served through a
  presigned storage URL download again.
- `listen --forward-to` follows a same-origin `307` or `308` from your local
  app, such as a trailing-slash redirect, and says plainly when it will not
  follow one.
- The confirmation required before a live-mode call that moves money now sees
  through trailing slashes, percent-encoding, dot segments and letter case in
  the path.
- `listen` lost an event behind a local handler slower than the 45-second
  stall timeout: the stream was dropped under the forward, the event was
  neither delivered nor reported, and the resume point moved past it. The
  stream stays up while a forward is in flight, and a forward cut short is
  printed as `NOT forwarded` with the `billkit events retrieve` command and
  replayed on reconnect.
- `listen` re-sent an event to a handler that took longer than the forward
  timeout, up to four times. A timeout after the request reached your app is
  now reported as unconfirmed and not retried.
- `listen` ignored the server's `unauthorized` frame when a key was revoked
  mid-stream and reconnected into a bare 401. It prints the server's message
  and exits 4.
- `install.sh` failed when `BILLKIT_INSTALL_DIR` did not exist yet. It
  creates the directory first.
- The one-off payment "check first" hint prints a command that runs; `login`
  names a non-default host; the `api` help example uses `label`; the
  `customer.deleted` trigger sends a plain `DELETE`.

### Security
- Live API keys are kept in the OS keychain instead of the config file,
  which keeps them out of plain files on disk and out of dotfiles.
- The release workflow pins every action to a commit SHA, and CI runs
  `govulncheck`.
- `install.sh` refuses an unsigned release from v0.3.0 onwards (override with
  `BILLKIT_ALLOW_UNSIGNED=1`) and runs nothing if the script was truncated in
  transit.
- Every release archive is published with an SBOM.

## [0.4.0] - 2026-10-09

### Added
- `checkout one-shot --ui-mode embedded` creates a one-off payment for the
  embedded payment element: `--method` is refused, and the response carries a
  `client_secret` instead of a `redirect_url`. Hosted remains the default.
- `invoice.upcoming` is a known event type.

## [0.3.0] - 2026-09-23

### Added
- `billkit api --json` sends a request body verbatim, inline, from `@file`, or
  from stdin, so arrays, nested objects and metadata can be sent. Amounts
  beyond 2^53 are no longer rounded through a float.
- `checkout one-shot --method` accepts `paypal` and `banktransfer`.
- `trigger` covers eleven more events: `customer.deleted`, `product.updated`,
  `product.archived`, `price.created`, `price.updated`, `price.archived`,
  `coupon.created`, `coupon.updated`, `webhook_endpoint.created`,
  `webhook_endpoint.updated` and `webhook_endpoint.deleted`.
- `events list` takes `--starting-after`, and validates `--type` locally.

### Changed
- `listen` resumes from the server's own cursor after a reconnect, so
  `--events` filtering also holds across reconnects.
- `listen` forwards the `BillKit-Event-Id` and `BillKit-Delivery-Attempt`
  headers, matching a real webhook delivery, so a handler that deduplicates on
  the event id behaves the same locally and in production.
- A failed local forward is retried on connection errors before the cursor
  moves past the event.
- A `409 idempotency_in_progress` is retried with the same key, as every SDK
  does, and the "this may already exist" guidance is shown on any timed-out
  write.
- A live-mode plan change asks for confirmation. `billkit api` now also asks
  before a live-mode `cancel` with `refund_on_cancel` and before
  `reauthorize_payment_method`, both of which can move money.
- Usage errors exit 2. `GET` refuses a request body.
- `login` honours `--profile`.

### Fixed
- `--api-key` is never echoed to stderr, including in the retry hint.
- `config list` no longer prints a short key in full.
- An event larger than the read buffer is reported and skipped instead of
  stalling `listen`.
- The config directory is only tightened to 0700 when it is the default
  `~/.billkit`, not whatever `BILLKIT_CONFIG_HOME` points at.
- `install.sh` resolves the latest release without the rate-limited GitHub
  API.

### Security
- `checksums.txt` carries a keyless Sigstore signature, which `install.sh`
  verifies.

## [0.2.4] - 2026-09-22

The first complete public release: signed and notarized for macOS, with the
Homebrew cask published.

### Fixed
- Release packaging only. No change to the binary's behaviour.

## [0.2.3] - 2026-09-22

Signed, but published without a Homebrew cask. Use 0.2.4 or later.

### Fixed
- Release packaging only.

## [0.2.2] - 2026-09-22

First public release, unsigned. Use 0.2.4 or later.

### Added
- `login`, `logout` and `config`, with named profiles stored in
  `~/.billkit/config.json` at mode 0600.
- `listen` with `--forward-to`, re-signing each event with a local
  `bkwhsec_` secret; `trigger` for test events; `events list`.
- `refunds create|list|retrieve` and `checkout one-shot|retrieve`, with
  `--idempotency-key` on every create.
- `billkit api` for any route, with `--data`.
- JSON output on stdout, coloured on a terminal, honouring `NO_COLOR` and
  `--color`; errors print the envelope's `code`, status, `message`, `reason`
  and `param` on stderr.

### Security
- `install.sh` verifies the archive against `checksums.txt` and installs with
  `install -m 0755`, so a binary on a root-owned path is root-owned.

[Unreleased]: https://github.com/billkit-eu/billkit-cli/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/billkit-eu/billkit-cli/compare/v0.4.0...v1.0.0
[0.4.0]: https://github.com/billkit-eu/billkit-cli/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/billkit-eu/billkit-cli/compare/v0.2.4...v0.3.0
[0.2.4]: https://github.com/billkit-eu/billkit-cli/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/billkit-eu/billkit-cli/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/billkit-eu/billkit-cli/releases/tag/v0.2.2
