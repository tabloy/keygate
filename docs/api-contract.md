# API contract

The rules an SDK can build on. Deliberately small: only what holds
across endpoints and is covered by a test. Anything specific to one
endpoint belongs in `openapi.yaml` or in the response itself, not here.

## The envelope

Every response under `/api/` is the same shape, success or failure,
apart from the raw-body routes listed below.

```json
{ "success": true,  "data": { ... } }
{ "success": false, "error": { "code": "LICENSE_NOT_FOUND", "message": "...", "details": {} } }
```

- `data` and `error` are mutually exclusive; exactly one is present.
- `error.code` is `SCREAMING_SNAKE_CASE`, and **a given code always
  arrives with the same HTTP status**, so a client can branch on the
  code and decide retries from the status. Enforced by
  `TestErrorCodeMapsToExactlyOneStatus`.
- `error.details` is optional and carried by only a few codes, for
  example `ACTIVATION_LIMIT`:
  `{"code":"ACTIVATION_LIMIT","message":"...","details":{"current":1,"max":1}}`.
  Treat it as absent unless a code documents it.
- `204 No Content` is the only success with no body.
- An unmatched path or a wrong method answers with the same envelope
  (`404 NOT_FOUND`), so a decoder needs one path, not a second one for
  "the URL was wrong".

### Raw-body routes

These are read by something that is not our SDK, so their bodies are
whatever that consumer requires. They are the only exceptions above.

| Route | Body |
|---|---|
| `GET /releases/:slug/feed.xml` | RSS/Sparkle XML |
| `GET /releases/:slug/feed.json` | a bare JSON array |
| `GET /releases/:slug/upgrade.json` | a bare Tauri manifest, or `204` when there is no update |
| `GET /admin/products/:id/signing-key/public.pem` | the PEM itself |
| `POST /webhook/stripe` | `{"received": …}`, for Stripe; **errors are not the envelope either** |
| `GET /pay/:checkout_id` | a 302 to Stripe, plain text on error; outside `/api/` |

The four read routes still answer *errors* with the envelope: ask
`feed.xml` for a product that does not exist and you get `404
NOT_FOUND` as JSON. So check the status first and decode the route's
own format only on success. The two write routes do not.

## Naming and types

- `snake_case` throughout.
- A field ending in `_at` is an RFC3339 timestamp, always UTC.
- A chart bucket (`date`, `period`) is a plain `YYYY-MM-DD` date, not a
  timestamp. Do not hand both to the same parser.
- An empty collection is `[]`, never `null`.

## Pagination

```
GET /api/v1/admin/licenses?limit=50&offset=0

{ "success": true, "data": {
    "licenses": [ ... ],   // named after the resource, not a fixed "items"
    "total": 1234,         // rows matching the filter, not rows in this page
    "limit": 50,           // the limit actually applied
    "offset": 0
} }
```

**Lists are paginated, and the default is 50.** A client that reads the
collection and assumes it received everything will silently see only
the first page. This changed: these lists used to return every row.

- The cap is 200, and asking for more is **clamped, not refused**: ask
  for 1000 and you get 200, with `limit: 200` in the response. Compute
  page counts from the response, never from the request.
- `limit < 1`, a negative `offset`, or anything unparseable reads as
  "not asked for" and takes the default. `limit=0` does not mean
  "everything".
- Every paginated list orders by a unique tiebreaker (`, id`) and sorts
  `NULL` last, so tied rows cannot repeat or vanish across pages.

Paginated resource keys: `addons`, `api_keys`, `audit_logs`,
`deliveries`, `events`, `licenses`, `members`, `plans`, `products`,
`releases`, `seats`, `sessions`, `users`, `webhooks`.

Some endpoints answer with a bounded collection and no paging at all:
`/portal/licenses`, `/products/:slug/plans`, `/admin/system/migrations`
and the `/admin/analytics*` and `/admin/stats` aggregates. Do not wrap
those in a paging loop; it would spin on the same page.

### Sorting

Only `GET /admin/licenses` reads `?sort=` and `?order=`. Columns are an
allowlist: an unknown one is `400 INVALID_SORT` and the message lists
what is accepted, and `order` takes `asc` or `desc` else `400
INVALID_ORDER`.

Every other list has a fixed order and **ignores `sort` and `order`
silently**. Do not write one sorting helper and point it at every list.

## Authentication

- `/license/*` and the public feeds need no API key: the `license_key`
  in the body is the credential.
- `/admin/*` takes `Authorization: Bearer kg_live_…` or a session
  cookie. An API key bound to one product gets `403
  PRODUCT_SCOPE_MISMATCH` reaching for another's resources.
- `/portal/*` is a customer session.
  `/portal/licenses/:license_key/activations` carries the key in the
  path; the server redacts that segment before logging and an SDK's own
  logging should too.

## Idempotency

Three endpoints accept an `Idempotency-Key` header:

```
POST /license/activate
POST /license/usage
POST /license/floating/checkout
```

A replay returns the first response and carries
`Idempotent-Replayed: true`. Keys live 24h. The middleware answers
before the handler runs:

| Code | Status |
|---|---|
| `INVALID_IDEMPOTENCY_KEY` | 400 |
| `BODY_TOO_LARGE` | 413 (over 256 KiB) |
| `IDEMPOTENCY_KEY_CONFLICT` | 422 (same key, different body) |
| `IDEMPOTENCY_IN_FLIGHT` | 409 (`Retry-After: 1`) |

The other `POST /license/*` routes are idempotent by nature and ignore
the header.

## Retrying

Decide from the status, not the code:

- **429** back off. `QUOTA_EXCEEDED` is the exception: a spent usage
  quota does not refill by waiting.
- **502 / 503** back off, except `*_NOT_CONFIGURED` and `*_DISABLED`,
  which mean the feature is off for this install and will answer the
  same way forever.
- **500** `INTERNAL_ERROR`, always the same opaque message; the detail
  goes to the server log. Worth one retry.
- **4xx otherwise** the request is the problem; retrying it unchanged
  will not help.
