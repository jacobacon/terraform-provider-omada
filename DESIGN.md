# Design & contribution guide

The map for contributors — human or agent — working on
`terraform-provider-omada`. It covers **how the provider is built**, **what is
already covered**, and **what still needs doing**, with enough per-item detail to
pick something up without reverse-engineering the repo first.

[`README.md`](README.md) is the user-facing document: installation,
authentication, and the limitations a practitioner needs to know. This file does
not repeat it. For toolchain versions and the security rules, see
[`AGENTS.md`](AGENTS.md).

---

## 1. What this provider is, and the constraint that shapes it

A Terraform provider for the **TP-Link Omada** controller (v6 — OC200/OC300 or
Software Controller), driving the controller's **undocumented web API**
(`/{omadacId}/api/v2/…`) — the same API the Omada web UI calls.

That choice is the single fact that explains most of this codebase. TP-Link
publishes no specification for the web API, so every endpoint, verb and payload
shape here was learned from the UI and confirmed against a real controller. The
alternative — TP-Link's documented Open API — covers far less; the web API is
the only surface with full configuration coverage, including gateway, firewall
and site settings. A handful of capabilities exist *only* on the Open API, so the
provider speaks both (§2.7).

Three consequences run through everything below:

1. **Nothing is assumed.** Verbs differ per endpoint, read and write shapes
   disagree, and error codes carry meaning. Each is confirmed, and the
   confirmation is recorded in a comment next to the code that depends on it.
2. **Unmodelled fields are preserved, never blanked.** A document usually
   carries more than the provider models, and some of it the controller owns.
   Read-modify-write is the default (§2.3).
3. **The blast radius is physical.** This provider configures the network the
   operator is reaching the controller over. A resource that guesses wrong does
   not fail a test — it takes a site off the internet. Where a write path could
   not be validated, it is marked as such rather than shipped quietly.

### 1.1 The validation environment

Resources are exercised against a live Omada **v6.2** controller in a dedicated
test environment: a gateway (ER707-M2), two switches (ES205GP) and three access
points (EAP610).

That hardware determines what can be verified, and several gaps in §5 exist
purely because of it — no static-IP WAN, no BLE-capable access point, no gateway
reporting `customAcl: true`. When something cannot be exercised there, the rule
is to say so in the schema and in §5 rather than ship an untested write path.

Contributors do **not** need hardware: unit and acceptance tests run entirely
against an in-process mock (§4.0). Hardware is needed only to *discover* a new
endpoint or confirm a verb.

---

## 2. Architecture

### 2.1 Two layers, one rule

```
internal/omada/       ← the client: pure Go, HTTP + JSON, zero Terraform types
internal/provider/    ← the provider: framework schema <-> client, per resource
```

**The rule: never import `terraform-plugin-*` into `internal/omada/`.** The client
is a plain Go SDK that could stand alone; the provider layer is the only place that
knows about `types.String`, diagnostics, schema, etc. This keeps the client
unit-testable and the mapping logic obvious.

Each resource is one file in `internal/provider/<name>_resource.go` with a matching
`<name>_resource_test.go`, and its controller calls live in one
`internal/omada/<domain>.go`.

### 2.2 The controller handshake (`internal/omada/auth.go`, `client.go`)

1. `GET /api/info` → `omadacId`, `controllerVer`, `apiVer`.
2. `POST /{omadacId}/api/v2/login` with username/password → a **`token`**.
3. Every subsequent call sends that token as the **`Csrf-Token`** header **and**
   carries the session cookie (the client holds a cookie jar).
4. All responses use the envelope `{ errorCode, msg, result }`. `errorCode == 0`
   is success; anything else becomes an `APIError` (`client.go`).
5. On an expired-session error code the client **re-logs in once and retries**
   (`isSessionExpired` + the `retry` arg in `do`). Callers never handle this.
6. `skip_tls_verify` (default **true**) installs a permissive TLS transport —
   controllers ship self-signed certs. This default is intentional; don't change it.

### 2.2a Two-factor authentication at login (`internal/omada/totp.go`)

A controller with 2FA enforced (*Global View → Settings → Account Security*)
answers step 2 above with a challenge instead of a token:

```
POST /{omadacId}/api/v2/login  {"username","password"}
-> errorCode -30165, result {}          // 6.1.0.19; other builds carry
                                        // "MFAId" and "supportedMFATypes"

POST /{omadacId}/api/v2/checkMFACodeAndLogin
   {"username","password","code","MFAId","mfaType":3}
-> result {"token": …, "roleType": …}          // the same token as a plain login
```

Learned from the controller's own login page (`modules/login/{models,controllers}`
under the UI's static assets), which is the only description of it that exists.
`mfaType` is `2` for an emailed code and `3` for an authenticator app; the UI
sends `-30138` down the same path as `-30165`. Both challenge fields are
optional — a live OC200 returns an empty result, and the login page falls back
to an empty `MFAId` and an authenticator-app code, which the controller
accepts. A wrong code returns `-30139`
with `result.codeRemainAttempts`, counting down to a temporary account lock.

Three consequences shape the implementation:

- **Only TOTP can be automated.** An emailed code needs a human, so an account
  whose `supportedMFATypes` lacks `3` fails with an error naming its actual
  methods rather than a generic rejection.
- **Retry a rejected code exactly once.** Codes are single-use, and a separate
  process — the previous terraform command, a browser login — may already have
  spent this window's, which looks identical to a wrong secret. One retry in
  the next window rescues that; a second would start walking a genuinely wrong
  secret toward the lock. A live 6.1.0.19 controller sends `-30139` with no
  `codeRemainAttempts` at all, so the retry cannot be gated on the budget —
  when the field *is* present and nearly spent, stop at the first rejection.
  A malformed secret is caught when the client is built, before a code is sent.
- **Never replay a code.** A re-login after a session timeout can land in the
  same 30-second window as the previous one, so `nextTOTPCode` waits for the
  next window rather than resending the code the controller already saw.

`totp_secret` is optional and documented as the weakening it is: the secret
lives beside the password, so it buys automation for one account rather than
real second-factor protection. What it preserves is 2FA *enforcement* for every
human account on the controller, which is otherwise the thing an operator has
to give up to run this provider at all.

### 2.3 Sites (`internal/omada/sites.go`)

Everything is site-scoped. `ResolveSiteID(ctx, name)` maps a site *name* to its ID,
and an **empty name resolves to the controller's primary site** — real sites are
often named `Home`, not `Default`, so we never hard-code a name. Resources cache
the resolved `site_id` in state and accept both `<id>` and `<site>/<id>` on import.

### 2.4 Read-modify-write (the most important pattern)

Most controller objects carry dozens of fields the provider doesn't model
(derived values, capability flags, sub-objects the UI manages). A naive `PATCH`
with only our fields would **blank the rest**. So updates do read-modify-write:

- `RawByID(ctx, listPath, idKey, id)` fetches the current object as a
  `map[string]any` (`client.go`).
- `mergeInto(cur, fields, deepKeys...)` overlays our fields onto it, **deep-merging**
  the named sub-objects instead of replacing them (`portprofile.go:141`).
- The merged map is sent back.

This is why, e.g., a port-profile update preserves the STP `instances` list and
`prohibitModify`, and an SSID update preserves the existing `pskSetting.securityKey`.
When you add fields to an existing resource, decide whether each new nested object
needs to be in that resource's `deepKeys` list.

### 2.5 Singleton settings documents

Several controller settings are **flat singleton documents**: one JSON object per
site behind a fixed path, with no id, that can only be read and updated — never
created or deleted. SSH, ALG and attack defense are all this shape.

They are implemented once, not per endpoint:

- `internal/omada/settings.go` — a `SettingDoc{Path, Verb}` per endpoint, plus
  `GetSetting` / `UpdateSetting` (read-modify-write).
- `internal/provider/settings_singleton.go` — a generic table-driven resource.
  Adding one is then just a `settingsSpec` listing `attr → controller key → kind`
  (see `alg_resource.go`, ~40 lines including docs).

Two behaviours are baked in because every one of these endpoints shares them:

- **Reads carry controller-owned metadata** — a `resource` counter and the
  `support*` / `exist*` capability flags the UI uses to decide what to render.
  These must be stripped before writing back; `controllerOwnedKey` does it, and a
  mock handler fails the test if any leaks into a write.
- **A settings document can carry secrets.** `/setting/snmp` returns the SNMP
  v3 `password` in **plaintext** once v3 is enabled, and the v1/v2c
  `communityString` likewise. The singleton scaffold models such keys with
  `kindStringWO`: `WriteOnly` in the schema, read from `req.Config` (write-only
  values are null in the plan), and never written to state. The read-modify-write
  then preserves a secret that was not re-supplied, because `cur` still holds it.
- **A document can mix settings with reference data.** `/setting/ips` returns
  `lowCategories` / `mediumCategories` / `highCategories` / `allCategories`
  describing what each protection level covers. Those are not configuration:
  the controller keeps them whether or not they are sent. Declare such keys in
  the `SettingDoc`'s `ReadOnlyKeys` so the read-modify-write drops them, and
  model them with `kindIntListRO` so they are reported as Computed-only.
- **The update verb varies.** `/setting/ssh`, `/setting/transmission/alg` and
  `/setting/firewall/attackdefense` reject `PATCH` with `-1600` and need `PUT`;
  `/setting/dot1x` and `/setting/accessControl` are the reverse. Each `SettingDoc`
  records the verb confirmed on hardware.

Unlike list-backed resources, the singleton resource addresses plan/state by
`path.Root(attr)` instead of a struct with `tfsdk` tags, because the field set
differs per spec and one Go type cannot cover them all.

### 2.6 Invariants you must not break

These are enforced by tests and/or matter for safety. Read before touching the
relevant resource.

- **Secrets the controller returns in plaintext are never read back.** The SSID
  list returns the WiFi `psk` in plaintext and RADIUS profiles return
  `authServer[].radiusPwd` the same way, so neither is ever decoded into state
  from the API, and updates preserve the stored value when a new one isn't
  supplied (`pskSetting` deep-merge; `carryRadiusSecrets`).
- **Not reading a secret back is only half the job.** Terraform persists
  *configured* values in state regardless of what the provider reads, so
  `Sensitive: true` alone still writes the secret to the state file. Only
  `WriteOnly: true` (Terraform ≥ 1.11, framework ≥ 1.14) keeps it out of state
  and plan entirely. `omada_radius_profile.shared_secret` uses it, and an
  acceptance test greps the whole state for the secret to prove it.
  ⚠️ `omada_wireless_network.psk` and `omada_portal.password` are **still only
  `Sensitive`**, so their configured values *do* land in state despite what
  their descriptions imply — converting them is a small, worthwhile follow-up.
- **`deviceAccount` is never sent.** Site-settings updates must never include the
  device-credential object. A mock test asserts it survives untouched.
- **Null is not false.** Some controller fields come back `null`; writing `false`
  over a null is a *change*, not a no-op, and shows up as an unwanted diff. Model
  such fields as `Optional` + `Computed` and leave them unset unless the user sets
  them. (Seen on port-profile `dhcpL2RelaySettings` and several SSID toggles.)
- **Verbs vary per endpoint.** Some items update with `PATCH`, some with `PUT`;
  some delete at `/{id}`, some at `/{type}/{id}`. Static routes reject `PATCH`
  outright (`-1600`). Never assume — confirm against the live controller.

### 2.7 The Open API is a second, separately-authenticated surface

Two capabilities live only on TP-Link's documented **Open API** under
`/openapi/`: creating a network (§5.1) and per-device configuration (§5.5).

It does **not** accept the web session — a request carrying a valid
`Csrf-Token` and cookie is refused `-44116`. The controller UI only reaches it
because its requests are proxied through TP-Link's cloud connector, which
authenticates on the operator's behalf. Locally the only way in is a
client-credentials grant against an application registered under *Settings →
Platform Integration → Open API*:

```
POST /openapi/authorize/token?grant_type=client_credentials
{"omadacId": …, "client_id": …, "client_secret": …}
-> {"result": {"accessToken": …, "expiresIn": 7200}}
```

then `Authorization: AccessToken=<token>` on every call.
`internal/omada/openapi.go` implements that: the token is cached until shortly
before expiry, and a call refused with `-44112`/`-44113`/`-44116` refreshes once
and retries, so a token lapsing mid-apply is not a failed apply.

Credentials are provider-level and **optional** (`openapi_client_id` /
`openapi_client_secret`). When absent, operations needing them fail with a
message naming the setting and the UI page — not a bare "unauthorized", which
would send a practitioner to check the wrong credentials entirely.

Verified live against a v6.2 controller. Two path rules cost real time to find
and are worth stating plainly:

- **Paginated collections require `?page=&pageSize=`.** Without them the
  controller answers `-1001`, not `-1600`. That distinction is the useful part:
  `-1600` means the route does not exist, `-1001` means it does and the request
  was wrong. A probe that only checks "did it work" throws that signal away.
- **The version prefix is per-endpoint, not global.** `devices` and
  `switches/{mac}/ports` are `v1`; `lan-profiles` is `v2`. There is no single
  prefix to assume, so `OpenAPIPath` builds `v1` and anything on `v2` spells its
  path out.

Confirmed working: `GET /openapi/v1/{cid}/sites?page=1&pageSize=10`,
`GET /openapi/v1/{cid}/sites/{site}/devices?page=1&pageSize=20`,
`GET /openapi/v2/{cid}/sites/{site}/lan-profiles?page=1&pageSize=10`,
`PATCH /openapi/v1/{cid}/sites/{site}/switches/{mac}/ports/{port}`.

Two shapes of Open API dependency have now appeared, and they are worth keeping
apart because they fail differently:

- **Write-only crossover** (`omada_switch_port`): the read is on the web API, so
  a plan and a refresh work without credentials and only `apply` fails. (On
  v5.15 the write has a web-API fallback, §5.5.)
- **Whole-document** (`omada_iot_radio`): `/setting/iot/radio` is served *only*
  under `/openapi/v1` — the web API answers `-1600` for the same path — so
  reading needs credentials too, and the failure appears on refresh. The
  settings scaffold carries an `OpenAPI bool` on `SettingDoc` for this, and
  `doSetting` dispatches on it.

### 2.7a `site` must be Optional **and** Computed

Every resource takes an optional `site`, and it forces replacement. Declaring it
`Optional` alone looks right and is a trap — one that reached a live plan on real
hardware before it was caught.

The import id for these resources allows omitting the `<site>/` prefix. When it
is omitted, an `Optional`-only `site` imports as **null**. A configuration that
names its site then reads as a *change* to a replace-forcing attribute, so the
plan proposes **replacing the hardware it has just adopted** — and drags every
computed attribute to "known after apply" with it.

So `site` is `Optional` + `Computed` + `UseStateForUnknown` + `RequiresReplace`,
and read/apply record the **canonical resolved name** (`ResolveSite(...).Name`,
not what the caller asked for). Both halves matter: the schema alone still lets
an unset site drift, and writing the name alone would be an inconsistent result
for an Optional-only attribute.

`TestAccGatewayImportWithoutSitePrefix` pins it — bare import id, config naming
the site, plan asserted empty.

### 2.7b Controller-scoped resources take no `site` at all

§2.7a is a rule about site-scoped resources, not a requirement that every
resource have a site. A few controller documents live *above* sites: their path
has no `/sites/{id}/` in it and there is exactly one of them per controller.
`omada_controller_settings` is the first.

Controller scope does not imply a singleton. `omada_firmware_upgrade_schedule`
is controller-scoped and a *collection*: its path has no site, but there can be
many schedules, each with its own id (the controller's `autoCheckId`), and each
schedule names the sites it covers in a `sites` attribute. It follows the same
rule: no `site`, no `site_id`.

Such a resource simply omits `site` and `site_id`, never calls `ResolveSite`,
and uses the controller's own id (`client.OmadacID()`) as its Terraform id. No
second provider and no muxing is needed for this: the provider's own `site`
argument is already `Optional`, and `Configure` only stores it as `defaultSite`
for the resources that choose to consume it.

Two consequences worth stating, since a singleton behaves unlike the rest:

- **Create and Update are the same operation.** The document always exists, so
  "create" means "start managing". Neither checks for prior existence.
- **Delete changes nothing on the controller**, and says so with a warning.
  Resetting a controller-wide document on destroy would mean the provider
  inventing defaults for something every adopted device depends on.

### 2.8 Sparse keyed collections

Some documents carry a long list of keyed entries where most of each entry is
description rather than configuration. `/logs/notification` has 63 alert and 68
event notifications, each `{key, shortMsg, module, level, deviceTypes, email,
webhook, enable}` — only the last three are settings.

Modelling that as a list would force a configuration to restate 131 entries and
would let a stale config silently revert entries it did not mean to touch. The
pattern used instead, worth copying for anything similar:

- expose a **map keyed by the controller's own key**, marked `Optional`;
- patch entries **in place by key** during the read-modify-write, leaving every
  other entry and every unmodelled field untouched;
- refresh **only the declared keys**, so a sparse config stays sparse;
- make the per-entry toggles `Optional` and **not** `Computed`, and refresh only
  the ones actually set. Nested `Computed` attributes inside an `Optional` map
  are planned as null rather than unknown, so filling them fails as an
  inconsistent apply.

The cost is that `terraform import` cannot populate the map — which of the 131
you intend to manage is not discoverable — so an import is followed by a
one-time diff adding the declared entries. That is inherent, and the acceptance
tests use `ImportStateVerifyIgnore` for those attributes.

---

## 3. Coverage matrix

Legend: **live** = exact endpoint + verbs confirmed against a real v6.2 controller
with a throwaway object; **mock** = has acceptance-test coverage against the
in-process mock; **subset** = a practical field subset is modelled, the rest
preserved via read-modify-write.

| Resource / data source | CRUD | Verified | Notes |
|---|---|---|---|
| `omada_network` | C/I/R/U/D | live | create routes by purpose: `interface` via the Open API, `vlan` via the web API (gateway-less sites) — see §5.1; `ipv6` nested attribute covers "Get from Prefix Delegation" (`proto = "rdnss"`) only — the other IPv6 Interface Type modes (DHCPv6, SLAAC+Stateless DHCP, Pass-Through) are unverified |
| `omada_lan_dns` | CRUD | live | |
| `omada_port_forward` | CRUD | live | |
| `omada_ip_group` | CRUD | live | delete path is `/groups/{type}/{id}` |
| `omada_port_group` | CRUD | live | type-1 group; ACL ref via `*_type = 2` |
| `omada_firewall_acl` | CRUD | live | ACL type auto-discovered on import; custom ports/devices sent empty (§5.3) |
| `omada_wlan_group` | CRUD | live | |
| `omada_mdns_reflector` | CRUD | live | |
| `omada_mdns_profile` | CRUD | live, see §5.3 | separate collection (`/setting/profiles/mdns`) from the reflector rules; controller caps custom profiles per site (5 on dev hardware), reported as `mdnsCustomMaxProfileNum` |
| `omada_port_profile` | CRUD | live · subset | STP block deep-merged; on v5, create fills in five fields v5.15 will not leave null |
| `omada_wireless_network` | CRUD | live · subset | `psk` write-only; `vlan_enable` needs `lan_network_id` — see §5.3 |
| `omada_static_route` | CRUD | live | update is `PUT` (`PATCH` → `-1600`) |
| `omada_portal` | CRUD | live · subset | write-only `password`; bare-array list; PATCH RMW |
| `omada_vpn` | CRUD | **read live, writes inferred** | see §5.2 |
| `omada_mac_filter` | R/U (singleton) | live | master toggle only; entries live elsewhere |
| `omada_attack_defense` | R/U (singleton) | live · subset | flood defense / packet anomaly / IP options; update is `PUT` |
| `omada_ips_whitelist` | C/R/D | live | read at `/grid/`, write one level up; no update verb |
| `omada_snmp` | R/U (singleton) | live | update is `PUT`; v3 password returned in plaintext, so write-only |
| `omada_ips` | R/U (singleton) | live | update is `PATCH`; `*Categories` are controller-owned reference data |
| `omada_upnp` | R/U (singleton) | live | update is `PUT` |
| `omada_gateway_bandwidth_control` | R/U (singleton) | live | reads nested, writes flat — dotted keys (§2.5) |
| `omada_portal_access_control` | R/U (singleton) | live | switches only; policy lists preserved |
| `omada_session_limit` | R/U (singleton) | live | `PUT`; the per-host `table` is dropped before write |
| `omada_alg` | R/U (singleton) | live | FTP/H.323/PPTP/IPsec/SIP ALGs; update is `PUT` |
| `omada_ssh_settings` | R/U (singleton) | live | device SSH; update is `PUT` |
| `omada_mac_auth` | R/U (singleton) | live | update is `PATCH` |
| `omada_dot1x` | R/U (singleton) | live | site-wide 802.1X; update is `PATCH` |
| `omada_rate_limit_profile` | CRUD | live | bare-array list; a limit is absent while its enable flag is false |
| `omada_service_type` | CRUD | live | create returns the id as a **bare string**; update is `PUT` |
| `omada_qos_bandwidth_control` | CRUD | live | one rule per WAN port (`-43310`); create returns null, resolved by WAN |
| `omada_time_range` | CRUD | live | schedule profile; create returns `profileId`; list has no `totalRows` |
| `omada_radius_profile` | CRUD | live | bare-array list; `radiusPwd` write-only, carried across updates |
| `omada_dhcp_reservation` | CRUD | live | item path keyed on **MAC**; unknown key still answers 0 |
| `omada_disable_nat` | CRUD | live (delete inferred) | plural list / singular item paths; update is `PUT`; one rule per WAN port |
| `omada_notification_settings` | R/U (singleton) | live | outside `/setting/`; `PATCH` full doc; sparse keyed maps |
| `omada_audit_notification` | R/U (singleton) | live | as above; entries carry only `webhook` |
| `omada_site_settings` | R/U (singleton) | live · subset | ~45 fields; large object |
| `omada_sites` (data) | R | live | |
| `omada_networks` (data) | R | live | |
| `omada_port_profiles` (data) | R | live (v5.15) | `origin` from the profile's `flag`: 0 built-in, 1 made with a VLAN network, 2 user |
| `omada_wan` (data) | R | live | **read-only by design** — see §5.4 |
| `omada_wan_ipv6` | CRUD (adopt-only) | **read live, writes inferred** | narrow exception to "Writable WAN" — see §5.3; models only `wanPortIpv6Setting`, leaves IPv4/MAC untouched |
| `omada_port_forwards` (data) | R | mock | discovery — list rules + IDs |
| `omada_firewall_acls` (data) | R | mock | discovery — lists all ACL types |
| `omada_devices` (data) | R | live | inventory — gateways/switches/APs |
| `omada_controller_settings` | R/U (singleton) | live · subset | **controller-scoped: no `site`** — see §2.7b; `PATCH /controller/setting`, sections sent whole; SMTP/certificate/RADIUS deliberately unmodelled |
| `omada_firmware_upgrade_schedule` | CRUD | live | **controller-scoped collection** — see §2.7b; `/upgrade/autoCheck`: no GET by id (read via the list + `/sites/{id}`), `PATCH` needs the whole body (partial → `-1001`), `DELETE` of an unknown id succeeds; models come from `POST /upgrade/models` |

---

## 4. Adding a resource — the recipe

### 4.0 Building and testing locally

Toolchain versions are pinned in [`.tool-versions`](.tool-versions); with
[asdf](https://asdf-vm.com), `asdf install` gets them. Then once per clone:

```sh
go mod download
make tools            # tfplugindocs + golangci-lint into GOPATH/bin
```

```sh
make build            # compile
make test             # unit tests
make testacc          # acceptance tests (TF_ACC=1)
make lint             # golangci-lint
make docs             # regenerate docs/ from schema + examples/
```

**Neither test suite needs a controller.** Unit tests exercise the client against
an `httptest` mock; acceptance tests drive a real Terraform binary against an
in-process mock controller (`internal/provider/provider_test.go`). Both run in CI
on every PR, offline and without secrets.

The mock is not a stub that says yes. It reproduces the controller's actual
behaviour — including the parts that bite: rejecting the wrong verb with `-1600`,
refusing read-only keys, answering `-1` to a create that succeeded, enforcing
which lists belong to the firmware. **When you discover a controller quirk, teach
the mock about it**, so the next regression fails in CI rather than on someone's
network.

### 4.1 Running against a real controller

Needed only to discover an endpoint or confirm a verb. Point the CLI at a local
build via `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "wncservices/omada" = "/path/to/gopath/bin"
  }
  direct {}
}
```

```sh
make install
export OMADA_URL="https://omada.example.com"
export OMADA_USERNAME="…"
export OMADA_PASSWORD="…"
terraform plan        # no `terraform init` under dev_overrides
```

Terraform warns that dev overrides are in effect; that is expected.

### 4.2 The recipe

This is the exact loop the existing resources were built with. An agent can follow
it end to end.

1. **Capture the API.** In the Omada UI, perform the action (add the object, change
   a field) with browser devtools open. Record the request path, method, and JSON
   body. Where the UI builds the path dynamically, mine the web-app JS bundle.
2. **Confirm with a throwaway.** Replay create → update → delete against a **real
   controller** using a disposable object, and note the exact verbs and the delete
   path shape. Leave nothing behind. This is what "verified live" means.
3. **Client layer** (`internal/omada/<domain>.go`): a typed struct for the fields
   you model, plus `List` / `Get` / `Create` / `Update` / `Delete`. Use `RawByID`
   + `mergeInto` for updates if the object has fields you don't model (almost all
   do). Follow an existing file — `staticroute.go` is a small clean example,
   `wireless.go` shows deep-merge + a write-only field.
4. **Provider layer** (`internal/provider/<name>_resource.go`): the schema, the
   model struct, and `Create/Read/Update/Delete/ImportState`. Mirror an existing
   resource; keep the site-resolution and import boilerplate identical.
5. **Register** it in `internal/provider/provider.go` (`Resources` or
   `DataSources`).
6. **Mock + test.** Add handlers to the in-process mock in
   `internal/provider/provider_test.go` (`newMockController`) and a
   `<name>_resource_test.go` that does create → import (`ImportStateVerify`) →
   update. Assert that any unmodelled keys you rely on survive the update (see the
   port-profile test's `checkPreserved`).
7. **Example + docs.** Add `examples/resources/omada_<name>/resource.tf` (and
   `import.sh` if it imports), write good `MarkdownDescription`s in the schema, then
   `make docs`. **Never hand-edit `docs/`** — CI fails on a stale diff.
8. **Gate.** `make build && make test && TF_ACC=1 make testacc && make lint && make docs`
   all clean.

### Finding an undocumented endpoint

Step 1 of the recipe assumes you can capture the UI's request. When you can't
(or want to check first whether an endpoint exists at all), these work well and
need only a read-only session:

- **Probe paths directly.** An unknown path returns `-1600 Unsupported request
  path.`; a real one returns data or a *different* error (`-1001` invalid
  parameters, `-34326` object does not exist). That distinction makes a
  brute-force sweep cheap and unambiguous. Watch the casing: paths are
  **camelCase** (`/setting/radiusProfiles`, `/setting/accessControl`,
  `/setting/transmission/portForwardings`) even though a few are all-lower
  (`/setting/firewall/attackdefense`) — **and some are kebab-case and
  abbreviated**: `/setting/wan-ports`,
  `/setting/wired-networks/disable-nats`, and one-to-one NAT at
  `/setting/transmission/otonats` ("oto" = one-to-one). A sweep over
  camelCase full words alone will miss those entirely, which is exactly how
  they went unfound for a while. Sweep all three casings, and try
  abbreviations.
- **Ask the controller what the gateway supports.**
  `GET /sites/{id}/setting/capacity` returns a feature→bool map (`oneToOneNat`,
  `disableNat`, `customAcl`, `policyRouting`, `ipsec`, …). Use it to tell "this
  gateway can't do that" apart from "I haven't found the path yet", and to
  explain empty payload fields.
- **Determine the update verb without changing anything.** Read the document,
  strip controller-owned keys, and `PUT`/`PATCH` its own values straight back.
  A wrong verb answers `-1600` and a right one answers `0` — and because the
  payload is what was already there, nothing is modified. This is how every verb
  in `settings.go` was confirmed on live hardware.
- **Mine the web app.** `js/app/*.js` and `js/su/*.js` hold a handful of paths,
  but the settings pages are lazy-loaded and not reachable by URL guessing, so
  this is a weaker source than the three techniques above.

### Testing model — note

Testing is **mock-controller based**, not fixture-file based. `newMockController`
in `provider_test.go` is a stateful `httptest` server that emulates the handshake
and each endpoint; acceptance tests (`TF_ACC=1`) drive a real Terraform binary
against it, so CI needs no hardware or secrets. There is **no `internal/omada/testdata/`
fixture directory** despite what older notes imply. Live-controller validation is
done by hand via the `dev_overrides` flow (README → Local development).

---

## 5. What's left — the road to "complete"

"Complete" here means: **every configuration surface the Omada v6 web UI
exposes for a site can be managed declaratively.** Read-only telemetry, one-shot
actions and per-client runtime state are out of scope (§5.7).

Everything below was established against the validation environment (§1.1). An
endpoint's absence from this list means it was **not found** — not that it does
not exist. Three items were listed here as "presumably unsupported" for months
and turned out to be present under names nobody had guessed, so treat absence as
weak evidence.

[`README.md`](README.md#limitations) carries the short, user-facing version of
the same ground: what a practitioner cannot do today, without the
implementation notes. Keep the two in step, but do not copy detail into it —
users need the boundary, contributors need the reason.

### 5.0 Coverage audit

Every configuration endpoint found on the controller, and where it stands.

| Endpoint | Status |
|---|---|
| `/setting/lan/networks` | ✅ `omada_network` (**create blocked**, §5.1) |
| `/setting/lan/dns` | ✅ `omada_lan_dns` |
| `/setting/lan/profiles` | ✅ `omada_port_profile` (subset) |
| `/setting/wan/networks` | ⚠️ read-only data source by design (§5.3) |
| `/setting/wan-ports` | ⚠️ query parameter unknown — but **not needed**, see §5.8 |
| `/setting/wired-networks/disable-nats` | ✅ `omada_disable_nat` |
| `/setting/wlans` + `/wlans/{id}/ssids` | ✅ `omada_wlan_group`, `omada_wireless_network` (subset) |
| `/setting/transmission/portForwardings` | ✅ `omada_port_forward` |
| `/setting/transmission/staticRoutings` | ✅ `omada_static_route` |
| `/setting/transmission/alg` | ✅ `omada_alg` |
| `/setting/transmission/otonats` | 🚫 **blocked** — needs a static-IP WAN (§5.1) |
| `/setting/transmission/policyRoutings` | ❌ §5.2 |
| `/setting/transmission/sessionLimits` | ✅ `omada_session_limit` (per-host table not modelled) |
| `/setting/transmission/bandwidthControls` | ✅ `omada_gateway_bandwidth_control` |
| `/setting/qos/gateway/bwc` | ✅ `omada_qos_bandwidth_control` |
| `/setting/firewall/acls` | ✅ `omada_firewall_acl` (inline ports: §5.6) |
| `/setting/firewall/attackdefense` | ✅ `omada_attack_defense` |
| `/setting/firewall/macfilter` | ✅ `omada_mac_filter` (toggle only) |
| `/setting/firewall/urlfilterings` | ❌ §5.2 — needs its query parameter |
| `/setting/ips` | ✅ `omada_ips` |
| `/setting/ips/whitelist` | ✅ `omada_ips_whitelist` |
| `/setting/ips/grid/blacklist`, `/setting/ips/signature` | ⚠️ read-only and empty (§5.4) |
| `/setting/portals` | ✅ `omada_portal` (landing page: §5.3) |
| `/setting/accessControl` | ✅ `omada_portal_access_control` (switches; policy lists not modelled) |
| `/setting/dot1x` | ✅ `omada_dot1x` |
| `/setting/radiusProfiles` | ✅ `omada_radius_profile` |
| `/setting/macAuth` | ✅ `omada_mac_auth` |
| `/setting/profiles/groups` | ✅ `omada_ip_group`, `omada_port_group` |
| `/setting/profiles/timeranges` | ✅ `omada_time_range` |
| `/setting/profiles/service-type` | ✅ `omada_service_type` |
| `/setting/profiles/rateLimits` | ✅ `omada_rate_limit_profile` |
| `/setting/profiles/apns` | ❌ §5.2 — cellular APNs |
| `/setting/service/mdns` | ✅ `omada_mdns_reflector` |
| `/setting/profiles/mdns` | ✅ `omada_mdns_profile` — the custom-service catalog `profile_ids` references; not discoverable by path-guessing under `/setting/service/mdns/*`, it lives in the `/setting/profiles/*` family alongside `service-type` |
| `/setting/service/dhcp` | ✅ `omada_dhcp_reservation` |
| `/setting/service/ddns` | ❌ §5.2 |
| `/setting/service/rebootSchedules`, `/poeSchedules` | ❌ §5.2 |
| `/setting/snmp` | ✅ `omada_snmp` |
| `/setting/ssh` | ✅ `omada_ssh_settings` |
| `/setting/upnp` | ✅ `omada_upnp` |
| `/setting/vpns` | ✅ `omada_vpn` (**writes inferred**, §5.3) |
| `/setting` (site settings) | ✅ `omada_site_settings` (~45 of many fields, §5.3) |
| `/setting/iptv` | ✅ `omada_iptv` |
| `/setting/dns-proxy` | ✅ `omada_dns_proxy` |
| `/logs/notification` | ✅ `omada_notification_settings` |
| `/site/audit-notification` | ✅ `omada_audit_notification` |
| `/rfPlanning` | ⚠️ an action, not config (§5.4) |
| `/setting/vpns/greTunnel` | ✅ `omada_gre_tunnel` |
| `/setting/iot/radio` | ✅ `omada_iot_radio` — **Open API only** |
| `/setting/iot/devices/config` | ⚠️ `omada_iot_beacon` — create/delete unverified (§5.1) |
| `/setting/iot/servers` | ✅ `omada_iot_server` |
| per-device configuration | ⚠️ `omada_switch_port`, `omada_switch`, `omada_gateway`, `omada_access_point` (§5.5) |

Not found despite looking, and so presumably unsupported on this hardware or
named unlike anything tried: DMZ, port triggering, multi-nets NAT, switch-side
QoS, standalone WLAN schedules.

**Three of that list have since been found**, all of them named unlike anything
guessed — which is the lesson: absence of a match is weak evidence, and a UI
capture settles in seconds what hours of guessing cannot.

| Endpoint | Was assumed | Actually |
|---|---|---|
| `/setting/iptv` | unsupported | ✅ `omada_iptv` |
| `/setting/firewall/imbs` | unsupported (IP-MAC binding) | exists, **0 rows** — needs a row first |
| `/setting/firewall/macfilters` | unsupported (MAC filter entries) | exists, **0 rows** — needs a row first |

The two empty ones join DDNS and policy routes in the "create one in the UI and
the shape falls out" category.

### 5.1 Blocked on something outside the provider

These cannot be finished by writing code alone.

1. **Network create** — **done**, and it took four rounds of discovery rather
   than one. For an `interface` network it lives on the Open API at
   `POST /openapi/v2/{omadacId}/sites/{site}/lan-networks`. The web API rejects
   that POST, and there is **no `/networks/confirm` two-step** — that path
   answers `-1600` on both v1 and v2, so the earlier note here was wrong.

   **"The web API rejects the POST outright" is true of gateway sites only**,
   which is what the rig is. On a site with **no gateway** — verified on
   v6.2.14.11, a site whose only device was an EAP670 — the same web-API POST
   creates an L2-only VLAN from four fields and returns the new id as a bare
   string:

   ```json
   {"name": "IoT", "vlan": 56, "purpose": "vlan", "igmpSnoopEnable": false}
   ```

   No `gatewaySubnet`, no `interfaceIds`: the controller assigns both (it came
   back with four interface ids and `allLan: true`). So `-33515` and `-35930`
   below are rules of **gateway sites**, not of networks in general, and the
   provider routes create by purpose — `vlan` to the web API, `interface` to the
   Open API, leaving the proven path untouched. See `createVLANNetwork`.

   This matters beyond tidiness: a gateway-less site is exactly the case where
   the Open API create is unreachable, so before this there was **no way to make
   a VLAN at all** except by hand in the UI.

   The first round mapped the required field set — `name`, `purpose`, `vlan`,
   `igmpSnoopEnable` — off the endpoint's own validation errors *without
   creating anything*, by seeding the probe body with `vlan: 99999`. That is
   outside the 1–4094 range, so the request can never succeed no matter which
   other fields get filled in, and the walk can be pushed to a complete body
   while only ever returning validation errors. Worth copying whenever a create
   contract has to be mapped on hardware someone depends on.

   **But that technique has a limit, and the next three rounds are it.** A body
   that can never succeed only reveals the checks that run *before* the one
   deliberately failed. Each of these appeared only once the previous was
   satisfied:

   | Code | Meaning | Consequence |
   |---|---|---|
   | `-33515` | `LAN interfaces could not be none` | `interfaceIds` is required at create; it cannot be deferred to the follow-up update |
   | `-35930` | `When Purpose is set to Interface, gatewaySubnet cannot be null` | `gatewaySubnet` joins the seed |
   | `-1001` | `Invalid request parameters` | the surfaces spell `purpose` differently — see below |

   So: expect a second round of discovery once the body is otherwise valid, and
   do not report a contract as "mapped" on the strength of an unsatisfiable
   probe alone.

   **The surfaces disagree on `purpose`.** The web API calls it `"interface"`, a
   string; the Open API calls it `1`, an int. Passing the web API's value
   straight through produced a bare `-1001 Invalid request parameters` with
   nothing to point at the cause. `openAPIPurpose` translates it, and maps only
   `"interface"` — the sole value observed live — erroring rather than guessing
   on anything else, since a guess here creates a network of the wrong kind.

   **A fifth round came from the update half, not the create.** With the
   network created, the follow-up update failed `-1001 Parameter [proto] should
   not be null`. The web API's GET returns `lanNetworkIpv6Config` as `{enable}`
   only, but its PATCH rejects the object unless `proto` is present — a
   read-modify-write cannot restore a key the read never returned. Dropping the
   block instead answers `-1 General error`, so it has to be sent, with a
   default. Zero is not a guess: the Open API, which does return the field,
   reports `proto: 0` for every network on the site.

   That also exposed a real bug in `UpdateNetwork` affecting *existing*
   networks, not just new ones: it merged `dhcpSettings` but replaced every
   other nested object wholesale, so any update touching one would drop the
   controller-owned keys inside it. It now merges by shape rather than by name,
   which fixes the nested objects nobody has hit yet as well.

   **The `proto: 0` placeholder came back to matter once `ipv6` grew a real
   `proto` field.** It was harmless while the provider only tracked
   `ipv6_config_enable` (a bare int), but `NetworkIPv6Config.Proto` is a
   `string` now — decoding a literal `0` into it fails. Rather than touch the
   verified injection value, `NetworkIPv6Config.UnmarshalJSON` tolerates a
   non-string `proto` by treating it as "no mode" (`""`), the same as it being
   absent. Confirmed live: a network enabled via "Get from Prefix Delegation"
   (SLAAC+RDNSS) reports `{"proto":"rdnss","enable":1,"rdnss":{...},"ra":{...}}`;
   a disabled one reports only `{"enable":0}` — no `proto`, `rdnss`, or `ra` key
   at all, matching what the controller UI itself produces when set to IPv6
   Interface Type "None".

   `CreateNetwork` sends those five fields plus `gatewaySubnet`, then applies
   the rest of the configuration through the ordinary web-API update. That split
   is the whole reason the `purpose` mismatch was a one-line fix rather than a
   systematic hazard: every field carried across the boundary is a translation
   that has to be right, and only create would exercise it. The cost is that
   create is two calls, so an interruption between them leaves a network that
   exists but is not fully configured. The error says exactly that, and the next
   apply reconciles it.

   Verified end to end on the live controller: apply, a clean second plan, then
   destroy, with the site's network list back to its original five.

2. **iBeacon create and delete** — the resource ships, but only read, import
   and update are confirmed. A profile must name at least one AP in `macList`,
   and the controller validates that against its own IoT-capable inventory at
   `/setting/iot/devices`, which is **empty on this site**: the EAP610s have no
   BLE radio, and `/setting/capacity` reports no IoT flag at all. Every create
   is refused with `-33284 "The devices in the device list are not in the
   current site"`.

   Shipping it anyway is a departure from the rule that governs one-to-one NAT
   below, and the difference is blast radius: a beacon profile that fails to
   create is an error message, while a NAT write path that misbehaves takes the
   internet down. The mock emulates the device inventory so the create and
   delete paths are at least exercised somewhere. Needs an EAP650/670-class AP
   to confirm.

2. **One-to-one NAT** — the field set is complete
   (`name`, `status`, `externalIp`, `internalIp`, `dmz`, `interfaceIds`), but
   `-34282` says it requires a **WAN on a static-IP connection**, which the
   validation environment does not have. No write path can be exercised, and shipping
   an untestable NAT write path is how you take someone's internet down.
3. *(resolved — see §5.8. `/setting/wan-ports` still rejects every query
   parameter tried, but it turned out not to be needed.)*
4. **WLAN optimization** (`/rfPlanning`) — an *action*, not configuration:
   `PUT /rfPlanning/config` validates the document and persists nothing, and
   `/rfPlanning/result` reports a job status. Poor fit for Terraform unless the
   schedule turns out to persist; needs a UI capture of **Save** to tell.

### 5.2 Mapped, unblocked, ready to implement 🟢

Each was found and read on hardware. Most are a `settingsSpec` on the singleton
scaffold (§2.5) or a small CRUD resource; the shapes below are what the live
controller returned.

| Would become | Endpoint | Shape / notes |
|---|---|---|
| `omada_policy_route` | `/setting/transmission/policyRoutings` | paginated, empty on the validation environment — needs one entry or a capture for the item shape |
| `omada_ddns` | `/setting/service/ddns` | paginated, empty; `support*` flags indicate TP-Link DDNS + custom providers |
| `omada_reboot_schedule`, `omada_poe_schedule` | `/setting/service/rebootSchedules`, `/poeSchedules` | paginated, empty; pair naturally with `omada_time_range` |
| `omada_url_filter` | `/setting/firewall/urlfilterings` | **needs its query parameter** — answers `-1001` to every one tried |
| `omada_apn_profile` | `/setting/profiles/apns` | cellular APNs; only relevant with an LTE/5G WAN |
| `omada_ip_mac_binding` | `/setting/firewall/imbs` | paginated, empty; exists despite §3 having long claimed otherwise |
| `omada_mac_filter_entry` | `/setting/firewall/macfilters` | paginated, empty; the entries behind `omada_mac_filter`'s master toggle |

The empty ones share a trap worth naming: with no stored row, the item shape is
guesswork. Either create one entry in the UI first, or capture the `POST` — see
§4 on why probing alone stalls on endpoints whose validation names no fields.

**Two resources now share one pattern, and a third will want it.** `omada_iptv`
and `omada_dns_proxy` both wrap a document containing a **firmware-owned list**
(gateway ports; built-in DoH resolvers) alongside practitioner-owned data. In
both, the resource models *the set of enabled entries* rather than the list, the
read-modify-write preserves every row, an unknown id is an error rather than a
silent no-op — the controller accepts such writes and drops them — and the full
list is surfaced read-only so the ids are discoverable without a capture. Copy
that shape rather than inventing a third.

### 5.3 Breadth inside resources that already exist

- **`omada_mdns_profile`**: create answers `{"profileId": "..."}`; update is
  `PATCH` to the item path (`PUT`, and `PATCH` to the base path, both
  `-1600`); delete is `DELETE` to the item path. None of this matches
  `ServiceType`'s convention on the same `/setting/profiles/*` family —
  don't assume a sibling endpoint's shape without checking.
- **`omada_site_settings`** models ~45 fields of a large object. Add more the
  same table-driven way. Note `remoteLog` holds only `{enable}` while remote
  logging is off — the syslog server fields appear once enabled, so pinning
  `remote_log_port` today can diff against a controller that omits it.
- **`omada_wireless_network`** and **`omada_port_profile`** model practical
  subsets. Unmodelled fields are preserved by read-modify-write, never blanked.
  One exception: `vlan_enable` needs `lan_network_id` (the bound
  `omada_network` id) on create — `vlanSetting` binds an SSID's VLAN tag to a
  network object, not a bare number, and "preserved on update" only helps
  once that binding already exists. Confirmed live: `rateLimit`,
  `ssidRateLimit`, `rateAndBeaconCtrl`, `dhcpOption82`, and `vlanSetting` all
  fail a create that omits them, each a bare `-1001` with no field named
  except `rateLimit`.
- **`omada_vpn`** manages only `name`/`enable`, and its write verbs are
  **inferred** — the read shape is live-verified but create/update/delete were
  never exercised, because the validation environment's only VPN was removed. Prefer importing
  and toggling `enable` until someone validates the verbs on hardware.
- **`omada_portal`** covers the functional settings; the landing-page design
  (logo, colours, terms, **background image**) is preserved but not manageable.
  A background needs a multipart upload to `/setting/portals/media`, captured
  from the UI.
- **Writable WAN** stays a deliberate non-goal in general: `/setting/wan/networks`
  mixes config with read-only `support*` flags, its write verbs are undocumented,
  and unlike every other endpoint the write path **cannot be validated with a
  throwaway** — the only WAN is the live one. If attempted, do it with
  out-of-band access, in a window, modelling a narrow subset.

  **`omada_wan_ipv6`** is that narrow subset, attempted exactly this way: it
  models only `wanPortIpv6Setting` (confirmed live to be a clean sub-object,
  not mixed with `support*` flags the way the WAN document as a whole is) and
  read-modify-writes the full port document so `wanPortIpv4Setting` /
  `wanPortMacSetting` are never touched. Unlike `omada_vpn`'s inferred verb,
  which really has never been exercised, this one at least has a confirmed
  read path and a plausible verb by convention (`PATCH` to the item path,
  matching `UpdateNetwork`) — but the write itself is genuinely unconfirmed.
  Import an existing port and verify a no-op plan before trusting it further,
  the same advice the resource docs give a practitioner.

### 5.4 Read-only surfaces worth a data source

Straightforward, but each needs one real row before the item shape is known:

- `/setting/ips/grid/blacklist` and `/setting/ips/signature` — both read-only
  (every write verb `-1600`) and empty on the validation environment.
- `omada_clients` — per-client runtime state.
- `omada_service_types`, `omada_wan_ports` — listings that would make the opaque
  ids in §5.1 and §5.8 usable by name.

### 5.5 Per-device configuration — switch ports, gateway and APs shipped

`omada_devices` covers read-only inventory. Per-device *config* now has its
first resource, `omada_switch_port`, and it is the provider's only
**cross-surface** object: the read comes from the web API and the write from
the Open API.

That split is not a preference. The web API serves the full port document at
`GET /api/v2/sites/{site}/switches/{mac}/ports`, but has no per-port write:
`.../ports/{port}` answers `-1001` rather than `-1600`, so the route exists and
the port *number* is its key (the id form answers `-39701 This port does not
exist`) — yet it rejects the whole document, every small subset tried, **and
the UI's exact field set**. The Open API has the mirror-image gap: it takes the
write

```
PATCH /openapi/v1/{omadacId}/sites/{site}/switches/{mac}/ports/{port}
{duplex, linkSpeed, name, nativeNetworkId, networkTagsSetting,
 profileId, profileOverrideEnable, profileVlanOverrideEnable, tagIds}
```

and offers no read — that same path, `/openapi/v1/.../switches`, and the
paginated `.../ports` all answer `-1600` to a GET. Both halves confirmed live;
the PATCH was proved by writing a port's own values back to it, which is the
idempotent-probe technique in §4.

Three decisions in the resource worth keeping if it is extended to APs and the
gateway:

- **The PATCH body is an allow-list, not a round-trip of the read document.**
  The controller returns ~60 keys per port, most of them telemetry. Echoing
  them back would mean sending undocumented and read-only fields to a device
  carrying live traffic. Only the nine keys observed in the UI's own request
  are sent, and the mock rejects anything else so a regression fails in CI.
- **Every settable attribute is `Optional` + `Computed`, applied by
  read-modify-write.** Managing only a port's name must not blank its VLAN
  configuration. The cost is that an omitted attribute never reports drift;
  that is the right trade for hardware where a wrong write is an outage.
- **There is no delete.** Ports are physical. Resetting one to a notional
  default on destroy would make `terraform destroy` silently reconfigure live
  switching, using this provider's guess rather than the practitioner's — so
  Delete drops the resource from state and warns, and nothing is written.

**v5.15 controllers accept the whole document on the web API.** On v5.15
(confirmed with ES206X-M2 and ES210X-M2 switches) `PATCH .../ports/{port}`
takes the full port document as read and still refuses any subset with
`-1001 General error`. That breaks the allow-list rule above, so it is only a
fallback: `UpdateSwitchPort` uses it when no Open API credentials are
configured and the controller reports a 5.x version, reads the port as an
untyped map, changes only the nine keys the Open API body carries, and sends
the rest back as it was read. The read and the write are separate calls, so a
UI edit made in between is lost. Any other version without credentials gets
`ErrOpenAPINotConfigured` before anything is sent, as before, and an error
from the v5 write is passed through as it came back. v5.15 answers an unknown
`profileId` with success and keeps the old profile, so the port is read again
after the write and a profile that didn't stick is an error.

**A switch's own name is a partial update.** `PATCH /sites/{site}/switches/{mac}`
with `{"name": …}` is what the UI sends to rename a switch, and nothing else on
the device changes (confirmed on v5.15 with ES206X-M2 and ES210X-M2 switches).
`omada_switch` models only that; its read comes from the device list.

**The gateway turned out to be the easy one.** `PATCH /sites/{site}/gateways/{mac}`
on the *web* API is a genuine partial update — a body with one key changes that
key and nothing else (PUT and POST answer -1600). No Open API needed: the
gateway's Open API document exists but carries only telemetry. Every field
`omada_gateway` models was confirmed by writing the device its own value back.

Its **physical ports are deliberately excluded**, and that is a scope decision
rather than a missing feature. `portConfigs` is where the WAN lives, so a wrong
write there is not a misconfigured feature but a site with no internet. The
scoped resources (`omada_wan`, port forwards, disable-NAT, firewall) cover the
same ground reviewably. `UpdateGateway` refuses a body containing `portConfigs`
even if some future caller builds one.

**APs turned out to be the gateway's case, with one extra rule.**
`PATCH /sites/{site}/eaps/{mac}` on the web API is a genuine partial update
(PUT and POST answer -1600), so `omada_access_point` needs no Open API. Its
fields split three ways, and conflating them breaks a live site:

- **Safe**: `ledSetting`, `lldpEnable`, `snmp`, `l3AccessSetting`, `ofdma*`,
  `lb*`, `rssi*`, `qos*`. Each confirmed by the §4 idempotent probe.
- **Disruptive**: `radioSetting2g` / `radioSetting5g`. Writing a radio document
  **restarts that radio even when every value in it is identical**. Observed
  live: writing `radioSetting5g` back to itself dropped every 5GHz client.
  So the idempotent probe that validates everything else in this provider is
  itself an outage here, and the resource compares against the device and
  sends a radio only when something differs. The acceptance test asserts the
  write count, not just the state.
- **Accepted, reported success, silently ignored**: `channel`. `"36"` or `36`,
  with the rest of the object intact, returns `errorCode 0` and leaves it at
  `"0"`. This is §5.5a's shape, so `channel` is read-only and the client layer
  refuses a body that sets it rather than letting an apply look clean and
  drift forever. The real mechanism is likely site-level RF planning.

`ipSetting` is rejected (`-1001`) in the shape the read returns, so it is not
modelled. And one field is booby-trapped: writing `mvlanEnable` with the
device's **own current value** cleared the coupled `mvlanNetworkId` from a real
id to `null`, and writing the id back returned `errorCode 0` without restoring
it — so `mvlanEnable` is refused unless `mvlanNetworkId` is set in the same
body. That is the sharpest illustration of why the allow-list rule above exists.

Still to do: the rest of the switch port document — PoE, per-port QoS, storm
control, spanning tree — none of which appear in the UI capture, so each needs
its own capture before anything is written. On APs, `ssidOverrides` and
`mgtSsidSetting` are likewise unmapped.

Supporting reads already available:
`GET /api/v2/sites/{site}/setting/lan/profileSummary` (port profiles as
`{id, name, type}`) and `.../setting/lan/networks-split` (networks with their
`interfaceIds`).

### 5.4a Resolve-by-name after create, and when it is safe

Ten client files create an object and then find it by name, because the POST
returns no id. That reads as fragile — and an automated security scan raised it
against `CreateACL` as a state-binding vulnerability.

It is not, and the reason is a **controller-side uniqueness constraint** rather
than anything in this code: creating a second ACL with an existing name is
refused with `-33006 "This name already exists in this site"`. Confirmed live.
Other endpoints do the same — IoT servers answer `-33249` for a duplicate
transport-stream name.

Two things follow, both worth knowing before "hardening" one of these:

- **Record the constraint next to the code that depends on it.** An unwritten
  assumption gets re-raised, by a person or a bot, roughly forever. `acl.go`
  now names the error code.
- **Duplicate-detection after the POST is worse than nothing.** Any such check
  runs after the write has already succeeded, so failing there orphans the
  object it just created — and the next apply then collides with that orphan's
  name permanently. If a create endpoint genuinely does need post-hoc recovery,
  the pattern is §5.5a's: treat the list as authoritative and *recover*, never
  bail.

### 5.5a A create that lies about failing

`POST /setting/iot/servers` stores the server and **then answers `-1 "General
error"`**. Confirmed repeatedly on live hardware: the row is in the list every
time.

Trusting the error code abandons an object that exists. Worse, it is silently
self-worsening — the orphan is invisible to Terraform, and the next apply
collides with it (`-33249 "This transport stream name already exists"`), so the
resource can never converge.

`CreateIoTServer` therefore treats **the list as authoritative, not the response
code**: on any create failure it re-reads and looks for what it asked for,
reporting the original error only if the object genuinely is not there. The mock
reproduces the lie, so a version that trusted the code fails in CI.

Worth checking for on any create endpoint before assuming an error means
nothing happened.

### 5.6 Firewall ACL inline ports — likely unbuildable on this hardware

Port-scoped rules work today through `omada_port_group` (referenced via
`source_type`/`destination_type = 2`). The ACL's **inline** `customAclPorts` /
`customAclDevices` are sent empty, and the ER707-M2 reports
**`customAcl: false`** in `/setting/capacity` — so the populated shape cannot be
captured on it at all. Needs hardware that reports `customAcl: true`.

### 5.7 Explicitly out of scope

Not gaps, and not planned: statistics and telemetry, log retrieval, one-shot
actions (reboot, upgrade, RF optimization runs, speed tests), client
block/unblock, and controller-level (as opposed to site-level) administration
such as users, roles and cloud access.

A *standing* firmware upgrade schedule is configuration, not a one-shot
action, so it is in scope (`omada_firmware_upgrade_schedule`); triggering an
upgrade now remains out.

### 5.8 The `1_<hex>` WAN interface id — resolved

Three features identify a WAN port by an opaque
`1_c967cf39292e474291e409b4dfe7f0cd` string: `omada_disable_nat.interface`,
`omada_qos_bandwidth_control.wan`, and one-to-one NAT's `interfaceIds`.

**It is the `portUuid`, and the provider already exposes it.** The `omada_wan`
data source flattens `/setting/wan/networks → wanPortSettings[]` and reports
`port_uuid` alongside `port_name`, so a configuration can reference a WAN by
its human name instead of hard-coding the id:

```hcl
data "omada_wan" "this" {}

locals {
  wan = { for p in data.omada_wan.this.ports : p.port_name => p.port_uuid }
}

resource "omada_qos_bandwidth_control" "wan" {
  wan = local.wan["2.5G WAN1"]
  # ...
}
```

Earlier revisions of this document claimed no endpoint listed these ids and
that `/setting/wan-ports` had to be cracked first. That was wrong twice over:
the ids are in a document the provider already reads, and the data source
already surfaced them. The cause was a probe whose regex was mangled by shell
quoting; it returned nothing and the empty result was taken as evidence rather
than checked. Worth keeping as a rule: when a probe reports "not found",
confirm the probe works before concluding anything about the API.

The full physical port inventory — including LAN-capable ports not currently
serving as a WAN — is at `osgPortInfo.wanLanPortSettings[]` in the same
document and is not surfaced. It would only matter for referencing a port that
is not yet a WAN.

### 5.9 Already covered — don't re-implement

Two things are commonly asked for and are already modelled, just not obvious
from the resource names:

- **Isolation** — `omada_network.isolation` (LAN-to-LAN) and
  `omada_wireless_network.guest_net` (guest/client isolation on an SSID).
- **Multicast** — `omada_network` carries `igmp_snoop_enable`,
  `mld_snoop_enable`, `fast_leave_enable`; `omada_wireless_network` carries the
  `multicast_*` family.

Also done and occasionally re-proposed: list pagination
(`internal/omada/pagination.go`), and discovery data sources for port forwards,
firewall ACLs and devices.

## 6. Release & versioning

- Semver via signed tags. On a `v*` tag, GoReleaser builds multi-platform archives
  and **GPG-signs** the checksums; the workflow then creates a **draft** release,
  uploads all 16 artifacts, and publishes it. The Terraform Registry ingests it on
  publication. Current line: `v0.6.x`.
- **Cut releases by pushing a tag, not from the GitHub UI.** The UI creates the
  tag *and* an empty published release; the tag push then starts the workflow,
  which used to add a second release object (drafts are not tag-bound), leaving
  a draft and a published release with the same name. The workflow now clears
  any pre-existing **empty** release for the tag first — and refuses to touch
  one that already has assets, so a re-run can never destroy a shipped release.
- **Why the draft dance:** with GitHub *immutable releases* enabled, a release is
  sealed the moment it is published and then rejects asset uploads with
  `422 Cannot upload assets to an immutable release`. GoReleaser publishes before
  uploading, so releasing directly produced a *published but empty* release — which
  looks like success. That silently shipped nothing for **v0.4.0, v0.5.0 and
  v0.5.1**, and the Registry stayed on 0.3.0 the whole time. Setting GoReleaser's
  own `release.draft: true` did **not** help (it still created the release
  published), so the workflow runs it with `--skip=publish` and does the
  draft → upload → publish sequence itself. The final step asserts at least 16
  assets are attached, so an empty release fails the build instead of passing
  quietly.
- Breaking schema changes wait for `v1.0.0`. Until then, additive field coverage and
  new resources are the normal cadence.
- CI (`.github/workflows/test.yml`) runs build, unit + acceptance tests, lint, and a
  `tfplugindocs` staleness check on every PR. Keep all green.

---

## 7. Where to look first

| You want to… | Start in |
|---|---|
| understand auth / retry / the envelope | `internal/omada/client.go`, `auth.go` |
| copy a small clean resource | `internal/omada/staticroute.go` + `internal/provider/static_route_resource.go` |
| add a flat settings singleton (cheapest new resource) | `internal/omada/settings.go` + `internal/provider/alg_resource.go` — a spec is ~40 lines |
| handle a secret correctly | `internal/provider/radius_profile_resource.go` (`WriteOnly`, read from `req.Config`) and `internal/omada/radiusprofile.go` (carry it across an update) |
| model a long keyed list sparsely | `internal/provider/notification_settings_resource.go` (§2.7) |
| deal with a natural key that is not the id | `internal/provider/dhcp_reservation_resource.go` (keyed on MAC, `RequiresReplaceIf`) |
| see deep-merge + read-only reference data | `internal/omada/wireless.go`, `internal/provider/ips_resource.go` |
| see import type-discovery | `internal/provider/firewall_acl_resource.go` |
| add mock endpoints / assertions | `internal/provider/provider_test.go` |
| prove a secret never reaches state | `internal/provider/secret_leak_test.go` |
| understand a data source | `internal/provider/networks_data_source.go` |
| find out what is left to build | §5 — start at the audit table in §5.0 |
