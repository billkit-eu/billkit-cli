# Contributing

This repository is a **read-only mirror**. It is published from the BillKit monorepo, and
anything pushed here directly is overwritten by the next release.

Bug reports and feature requests are welcome as issues. For a code change, open an issue
first and we will apply it upstream with attribution.

## Running the tests

```bash
go test ./...
go vet ./...
go build -o billkit ./cmd/billkit
```

Releases are built by GoReleaser from the tag, so the binaries you download are built
here, in public, from the commit the tag points at.
