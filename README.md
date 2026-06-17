# google2snipe

Sync **ChromeOS devices** from the Google Admin SDK Directory API into
[Snipe-IT](https://snipeitapp.com/). A sibling of `fleet2snipe`.

## Features

- Full reconciliation sweep (`sync`) for cron, plus single-device sync
  (`--serial` / `--device-id`).
- Idempotent `setup` that creates a 33-field ChromeOS custom-field set in
  Snipe-IT and merges the resulting mappings into your config.
- Configurable field mapping via gjson paths + transforms over the full
  ChromeOsDevice schema (any nested/array field).
- ChromeOS lifecycle status → Snipe status label mapping.
- Optional checkout to the assigned user (`annotatedUser`), falling back to the
  most-recent managed login user (domain-restricted).
- `--dry-run`, `--debug`, local response cache (`--use-cache`), structured logs.

## Authentication

Create a Google Cloud service account, enable the Admin SDK API, and grant it
**domain-wide delegation** for scope
`https://www.googleapis.com/auth/admin.directory.device.chromeos.readonly`
(Admin Console → Security → API controls → Domain-wide delegation). Download the
JSON key and set `google.credentials_file` + `google.impersonate_subject` (an
admin to impersonate).

## Quick start

```bash
go build .
cp settings.example.yaml settings.yaml
$EDITOR settings.yaml          # creds + Snipe IDs
./google2snipe test            # verify connectivity
./google2snipe setup           # create custom fields, merge mappings
./google2snipe sync --dry-run --verbose
./google2snipe sync            # do it
```

Run `./google2snipe sync` from cron (e.g. every 15 min). Projection defaults to
`full`.

## License

MIT
