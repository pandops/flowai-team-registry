# team-registry

Owner of canonical teams: their immutable integer `team_id`, immutable
`team_name` (unique ignoring case, also against archived teams), digest-bearing
`default_image`, `ingested_at` and `archived_at`. Teams are archived, never
deleted. The FlowAI platform OpenSpec defines the team-registry behavior.

## Interfaces

team-registry has no HTTP API. It answers NATS requests in the queue group
`team-registry`:

| Subject (served)                  | Caller publishes                  | Backs                            |
| --------------------------------- | --------------------------------- | -------------------------------- |
| `team-registry.admin.create.sub`  | `team-registry.admin.create.pub`  | `POST /admin/teams`              |
| `team-registry.admin.get.sub`     | `team-registry.admin.get.pub`     | `GET /admin/teams/{id}`          |
| `team-registry.admin.update.sub`  | `team-registry.admin.update.pub`  | `PATCH /admin/teams/{id}`        |
| `team-registry.admin.archive.sub` | `team-registry.admin.archive.pub` | `POST /admin/teams/{id}/archive` |
| `team-registry.service.get.sub`   | `team-registry.service.get.pub`   | team point read                  |

Admin requests carry the headers `team: admin`, `request_id` and
`admin_subject`; `service.get` carries `team` (the requested `team_id`, or
`admin`) and `request_id`. The request body is
`{"params":{...},"body":{...}}`; the reply has the header `status` and the REST
body or `{code, message, request_id}`.

### Team names

A team name is set once, at create, and never changes. It has 1 to 10
characters: lowercase ASCII letters and digits in groups joined by single
hyphens, starting with a letter (`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, for example
`ops` or `qa-2`). Any other create name is `400 invalid_request`. An update
changes only `default_image`; an update body that carries `team_name` is
rejected with `400 team_name_immutable` before the team is looked up, and the
rejection is audited. Archived teams keep their names, so a name is never
reused.

Every change and every administrator request is written to the outbox in the
same transaction and published after commit, in `publisher_seq` order, with
`Nats-Msg-Id: team-registry-<publisher_seq>` and the header `team: admin`:

- `team-registry.event.{team_created,team_updated,team_archived}.pub`
- `team-registry.audit.{team_created,team_read,team_updated,team_archived}.pub`

The JSON Schemas of all requests, replies and messages are in `schemas/`;
`schemas/consumers.json` is empty because team-registry consumes no stream.

## Runtime

- Image: `Containerfile`; entrypoint
  `/team-registry --config /etc/flowai/config.yaml`.
- Configuration: `config.example.yaml` lists every key. NATS must be `tls://`
  with the user credentials at `/run/flowai/nats/user.creds`; PostgreSQL uses
  `sslmode=verify-full`.
- PostgreSQL: only the schema `db.schema` (default `team_registry_schema`). The
  migration runs `CREATE SCHEMA` only when the schema is missing, so the role
  needs no CREATE right on the database when the deployment pre-creates it.
- One listener on `:8080` with `/v1/livez` and `/v1/readyz` (ready once the
  schema is migrated and the database and broker are reachable).

## Tests

```
go test -mod=readonly -count=1 ./...
go test -mod=readonly -count=1 -tags=integration ./test
```

Unit tests run the handler over an in-memory repository and a fake JetStream
publisher; integration tests use a disposable PostgreSQL 16 container started
through Docker or Podman. Run both commands from the module root.
