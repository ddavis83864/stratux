# Wi-Fi administration hardening

## Purpose

Adds strict server-side validation and a safe, two-step preview/apply/
confirm/rollback workflow on top of this project's existing Wi-Fi
configuration surface (`main/networksettings.go`'s `WiFi*` fields on
`globalSettings`, exposed today through the general `POST /setSettings`
endpoint). It does **not** replace that endpoint, which remains fully
functional and unchanged - see "Backward compatibility" below.

## Explicit non-goals

This is **not**:

- A general network manager, a captive portal, or a cloud/remote
  administration surface. Everything here runs entirely on-device; no
  new internet dependency is introduced.
- A new security capability. This feature does not add WPA3, protected
  management frames, client isolation, or firewall enforcement - Wi-Fi
  security still depends entirely on the AP mode configured and the
  client hardware/driver involved. A configured password means clients
  need it to join, nothing more.
- Proof that ForeFlight, or any specific application, is connected. GDL90
  client tracking in this project (`main/network.go`) identifies clients
  by DHCP lease/ARP entry only - it cannot and does not identify which
  application, if any, is using a given IP address.
- A change to default behavior. An existing installation's SSID, AP
  address, security mode, and GDL90/ForeFlight compatibility are
  unaffected unless an owner explicitly starts a transaction through this
  feature's own API.

## Current architecture (as found, before this feature)

Traced directly from the source this mission investigated
(`main/networksettings.go`, `main/gen_gdl90.go`, `main/managementinterface.go`,
`debian/*.template`, `debian/stratux-wifi.sh`) - not assumed:

- **Interfaces**: `wlan0` (physical), `ap0` (virtual AP interface, split
  off `wlan0`'s phy via `iw phy0 interface add ap0 type __ap`),
  `p2p-wlan0-0` (Wi-Fi Direct group interface), `eth0` (plain DHCP,
  unmanaged by this feature).
- **AP stack**: no `hostapd`. AP mode is `wpa_supplicant` itself running
  in `mode=2` (`wpa_supplicant_ap.conf`), started by
  `debian/stratux-wifi.sh`. DHCP/DNS is `dnsmasq`
  (`stratux-dnsmasq.conf`). Client-mode/AP+Client uses a second
  `wpa_supplicant` instance (`wpa_supplicant.conf`) via `wpa-roam` in
  `/etc/network/interfaces`. No NetworkManager, no systemd-networkd -
  everything goes through Debian's classic `ifupdown` plus this custom
  post-up hook.
- **Settings**: a single `settings` struct
  (`main/gen_gdl90.go`) holding `WiFiSSID`/`WiFiSecurityEnabled`/
  `WiFiPassphrase`/`WiFiChannel`/`WiFiCountry`/`WiFiMode`/
  `WiFiIPAddress`/`WiFiClientNetworks`/`WiFiDirectPin`/
  `WiFiInternetPassThroughEnabled`, JSON-marshaled whole to
  `/boot/firmware/stratux.conf` by `saveSettings()` - a plain
  truncate-and-write, **not** this project's own established
  temp-file+fsync+atomic-rename pattern.
- **API**: `POST /setSettings` mutates `globalSettings`, calls
  `saveSettings()`, then `applyNetworkSettings(false, false)` - which,
  **asynchronously in a bare goroutine**, after a 1-second sleep, runs
  `ifdown wlan0`, rewrites all four config files (each truncated
  in-place unconditionally, one at a time, with **no backup of the prior
  version**), then `ifup wlan0`. The HTTP response is sent back
  **before** this goroutine even starts - the client is never told
  whether the new configuration actually came up.
- **Validation**: only `WiFiIPAddress` has server-side content
  validation (an IPv4-format regex) - and an invalid value is silently
  **ignored**, not rejected. SSID/passphrase/channel/country validation
  exists only in client-side JavaScript
  (`web/plates/js/settings.js`), trivially bypassed by posting directly
  to the API. `main/settingsvalidate.go` only checks JSON *type*, never
  content.
- **The concrete injection risk**: SSID/passphrase/country/client-network
  values flow through Go's `text/template` (not `html/template` - no
  automatic escaping) directly into `wpa_supplicant_ap.conf`/
  `wpa_supplicant.conf`/`interfaces`. An SSID or passphrase containing a
  literal `"` or a newline is written verbatim inside a quoted config
  value, with no existing sanitization anywhere in that path.
- **GDL90 client discovery** does **not** depend on a fixed broadcast
  address - `main/network.go`'s `refreshConnectedClients`/
  `getDHCPLeases` reads `dnsmasq`'s lease file
  (`/var/lib/misc/dnsmasq.leases`) and opens a unicast UDP socket per
  leased client per configured output port. A subnet change does not
  itself break GDL90 targeting, but a client with a stale lease from
  before the change will not receive traffic until it renews.
- **Existing readiness/preflight coverage of Wi-Fi**: exactly one
  **manual**, human-observed preflight checkbox
  (`CheckIpadConnectedWifi`) - no automated health check of
  `wpa_supplicant`/`dnsmasq` process state or `ap0` interface status
  exists anywhere in this project before this feature.
- **Configuration Backup**: Wi-Fi settings are **explicitly and
  deliberately excluded** already, documented in three places
  (`configbackup/document.go`, `main/configbackupapi.go`,
  `docs/configuration-backup-restore.md`) as "network/credential surface,
  out of scope." This feature does not change that policy - see
  "Configuration Backup" below.
- **This project's own established idioms this feature reuses rather
  than reinventing**: the temp-file+fsync+atomic-rename persistence
  pattern (`main/alertsettings.go`'s `saveAlertSettings`), and the
  opaque-random/boot-session-bound/monotonic-TTL/single-use confirmation
  token shape already used by the controlled-shutdown flow
  (`power/shutdown.go`) and Configuration-Backup apply
  (`configbackup/token.go`).

## Ownership matrix

| Concern | Owner (Go code, existing) | Persistent source | Runtime consumer | Restart required? |
|---|---|---|---|---|
| AP SSID | `setWifiSSID` (`main/networksettings.go`) | `globalSettings.WiFiSSID` in `stratux.conf` | `wpa_supplicant_ap.conf`'s `ssid=` | `ifdown`/`ifup wlan0` |
| AP security mode | `setWifiSecurityEnabled` | `globalSettings.WiFiSecurityEnabled` | `wpa_supplicant_ap.conf`'s `key_mgmt=` | same |
| AP passphrase | `setWifiPassphrase` | `globalSettings.WiFiPassphrase` (plaintext) | `wpa_supplicant_ap.conf`'s `psk=` | same |
| Wi-Fi channel | `setWifiChannel` | `globalSettings.WiFiChannel` | `wpa_supplicant_ap.conf`'s `frequency=` | same |
| Regulatory country | `setWifiCountry` | `globalSettings.WiFiCountry` | both `wpa_supplicant*.conf`'s `country=` | same |
| AP IP/subnet | `setWifiIPAddress` | `globalSettings.WiFiIPAddress` | `interfaces`'s `ap0` static address | same |
| DHCP range | derived, never separately persisted | computed from `WiFiIPAddress` each apply | `stratux-dnsmasq.conf`'s `dhcp-range=` | same |
| Client-mode networks | `setWifiClientNetworks` | `globalSettings.WiFiClientNetworks` | `wpa_supplicant.conf`'s `network={}` blocks | same |
| GDL90 broadcast | N/A - lease/ARP-driven, not broadcast-driven | `dnsmasq.leases` + `StaticIps` | per-client unicast UDP sockets | none |
| Dashboard management (existing) | `handleSettingsSetRequest`/`handleSettingsGetRequest` | n/a | `web/plates/settings.html` | n/a |
| **This feature's own layer** | `wifiadmin.Manager` + `main/wifiadmin*.go` | `wifi-admin-last-known-good.json`/`wifi-admin-pending-transaction.json` | reuses the SAME templates/output paths above | same |

## Threat and failure model

Realistic local threats and operational failures this feature was
designed against - see "Exact implemented controls" for what closes
each one, and "Known limitations" for what remains open:

- Malformed settings (bad channel/country/address/SSID/passphrase)
  reaching a live config file and breaking AP startup.
- SSID/passphrase config-injection via unescaped `text/template` holes.
- A partial, interrupted, or crashed write leaving the four config files
  mutually inconsistent.
- The device rebooting or the process crashing mid-transaction.
- The owner's own device being the one disconnected by the change, with
  no way to know whether the new configuration actually came up.
- The owner reconnecting to the wrong network, or never reconnecting at
  all (a typo'd SSID/passphrase locking them out).
- A confirmation token being replayed, reused, or presented after an
  intervening, unrelated settings change invalidated its baseline.
- Concurrent Wi-Fi transactions, or a Wi-Fi transaction overlapping an
  OTA update, a Configuration Restore, a controlled shutdown, or an
  active recording.
- Secret leakage into logs, diagnostics, or status responses.

**Not addressed by this feature** (disclosed, not silently assumed
solved): the actual RF/cryptographic security of a given Wi-Fi mode;
regulatory legality of a channel/country combination; proof that a
specific application (as opposed to some client) is connected; an
owner physically unable to reach the new network at all (a genuinely
wrong SSID/channel for their hardware).

## Exact implemented controls

- **Strict, rejecting validation** (`wifiadmin.Config.Validate`) - see
  "Configuration schema and validation rules" below for the exhaustive
  list. Unlike the pre-existing `setWifiIPAddress`, an invalid value is
  always **rejected**, never silently ignored, and validation runs
  against the **complete** proposed configuration before anything is
  mutated - no partial application.
- **No shell/config injection**: `wifiadmin.Config.Validate`'s SSID
  charset (alphanumeric plus `()!  ._'-` and space, matching the
  existing client-side convention) and passphrase printable-ASCII/
  no-newline bound close the concrete template-injection risk identified
  above, before a value ever reaches `text/template`.
- **Two-step preview/apply, with a reconnection-confirmation step** -
  see "Preview/apply/confirm workflow" below.
- **Automatic rollback** if reconnection is not confirmed within
  `defaultReconnectTimeoutSeconds` (90s, derived - see that constant's
  own doc comment in `wifiadmin/transaction.go` for the reasoning).
- **Last-known-good retained** until a NEW configuration is explicitly
  confirmed - never overwritten by an unconfirmed apply.
- **Crash/reboot recovery**: a pending transaction record survives a
  process restart; at startup, `wifiadmin.NewManager` always rolls back
  to that record's own previous configuration rather than trusting
  anything unconfirmed - or surfaces an honest `RECOVERY_REQUIRED` state
  if that rollback itself fails.
- **Atomic config-file application**: all four derived files are
  rendered to temp files first; only if every one renders successfully
  are they renamed into place - closing a real gap in the pre-existing
  `applyNetworkSettings`, which truncates each file unconditionally, one
  at a time (see "Known limitations" for the one residual risk window
  this cannot fully close on this project's filesystem).
- **Secret handling**: a passphrase/client-network-password/Wi-Fi-Direct
  PIN is accepted only in a preview/apply request body; every
  status/preview/diagnostics response carries only a `*Set` boolean via
  `wifiadmin.Redacted`, never the value itself.

## Default-compatibility proof

- `wifiadmin.DefaultConfig()` returns exactly the values
  `main/gen_gdl90.go`'s existing `defaultSettings()` already ships (SSID
  `"Stratux"`, open, channel 1, AP mode, `192.168.10.1`) - verified by
  `wifiadmin.TestValidate_DefaultConfigIsValid` and this project's own
  existing `defaultSettings()` remaining byte-for-byte unmodified.
- `wifiAdminManager`'s startup (`initWifiAdmin`) never touches live
  network configuration unless a pending transaction record already
  exists from before a crash - a fresh install, or an install that has
  never used this feature, is completely unaffected.
- The pre-existing `POST /setSettings` endpoint, its handler, its
  validation, and `applyNetworkSettings` are all **completely
  unmodified** by this feature - not touched, not deprecated, not
  routed through this feature's own logic.

## Configuration schema and validation rules

`wifiadmin.Config` (`wifiadmin/config.go`): `SSID`, `SecurityEnabled`,
`Passphrase`, `Channel`, `Country`, `Mode`, `IPAddress`,
`ClientNetworks`, `InternetPassThroughEnabled`, `DirectPin`.

Exhaustive validation (`Config.Validate`):

- SSID: 1-32 bytes, charset `[A-Za-z0-9()!\ ._'-]` only (matches the
  existing client-side JS convention).
- Passphrase: empty when security disabled (a non-empty value here is
  itself rejected, not merely ignored); 8-63 printable-ASCII characters
  when enabled.
- Channel: one of 1-11 - the exact, exhaustive set
  `debian/wpa_supplicant_ap.conf.template` maps to a frequency. Anything
  else is rejected outright, rather than the template's own existing
  silent fallback to channel 1.
- Country: format-only - exactly two uppercase ASCII letters, or empty.
  **Not** validated against a channel/country regulatory legality table
  - this project has no authoritative source for one; see "Known
  limitations."
- Mode: one of AP (0) / Wi-Fi Direct (1) / AP+Client (2).
- AP address: valid IPv4, not multicast/loopback/unspecified, and not a
  `.0`/`.255` last octet (the network/broadcast address in the fixed
  `/24` this project's own DHCP-range derivation already assumes -
  `wifiadmin.DerivedDHCPRange` reproduces that exact existing algorithm
  and is proven, by `TestDerivedDHCPRange_NeverContainsAPAddress`, never
  to collide with any address `Validate` accepts).
- Client networks: at most `MaxClientNetworks` (10), no duplicate SSIDs,
  each entry independently SSID/passphrase-validated (an empty password
  is allowed - an open client network).
- Wi-Fi Direct PIN: 4 or 8 digits, or empty.

## Preview/apply/confirm workflow

1. **`POST /previewWifiAdminSettings`** - validates the complete
   proposed configuration, computes a field-by-field diff against the
   current last-known-good (secrets shown only as `(set)`/`(empty)`,
   never plaintext), and issues a confirmation token
   (`wifiadmin.ConfirmationToken`) bound to both the exact proposed
   configuration's checksum and the exact current baseline's fingerprint
   - reusing this project's own established
   `configbackup.ConfirmationToken` shape. TTL: 300s (matching
   Configuration-Backup's own preview TTL). This step never touches live
   configuration.
2. **`POST /applyWifiAdminSettings`** (with that token) - re-verifies
   the token and every precondition, marks the token used, persists a
   `PendingTransactionRecord` (naming the *previous* configuration as the
   rollback target), then calls the real `Executor.Apply`: writes all
   four config files atomically as a batch and runs the existing
   `ifdown`/`ifup wlan0` cycle. On success, issues a **separate**
   reconnection-confirmation token and returns to the client - the
   original apply token is already spent by this point.
3. **`POST /confirmWifiAdminReconnection`** (with the reconnect token) -
   reaching this endpoint at all is the strongest proof this
   architecture can offer that the new configuration is actually
   reachable (an HTTP request arrived at the running daemon). On success,
   the proposed configuration becomes the new last-known-good,
   persisted, and the pending-transaction record is cleared.
4. If step 3 does not happen before the deadline, `CheckDeadline`
   (driven by a 5-second ticker in `main/wifiadminapi.go`) automatically
   rolls back to the previous configuration.
5. **`POST /cancelWifiAdminChange`** - discards a not-yet-applied
   preview; rejected once apply has actually started (by then only
   confirm/rollback can move the transaction forward).
6. **`POST /rollbackWifiAdminChange`** - manual rollback, usable while
   awaiting reconnection or in `RECOVERY_REQUIRED`.

## Reconnection-confirmation behavior

Deliberately modest in what it claims: reaching
`/confirmWifiAdminReconnection` proves an HTTP client reached this
daemon process over *some* currently-active network path. It does
**not** prove the request arrived over the newly-applied network
specifically (as opposed to, say, a client that was never disconnected
because only a non-disruptive field changed), and it does **not** prove
any particular application (ForeFlight or otherwise) is connected -
consistent with this project's own existing GDL90 client-tracking
limitation (network-level liveness only).

## Automatic rollback and last-known-good

`defaultReconnectTimeoutSeconds` = 90s (`wifiadmin/transaction.go`) -
derived from the existing `ifdown`/`ifup` cycle's own observed latency
plus realistic client-side Wi-Fi reassociation/DHCP-lease time plus
margin for a human to notice and switch networks by hand; injectable per
test via `SetReconnectTimeoutSeconds`. The last-known-good configuration
is never overwritten until a NEW configuration is explicitly confirmed -
an apply failure, a missed confirmation, or a startup-recovery rollback
all restore it, never silently accept something else in its place.

## Persistence and crash/reboot recovery

Two files under `PersistentDataPath`
(`wifi-admin-last-known-good.json`/`wifi-admin-pending-transaction.json`),
each written with this project's own established
temp-file+fsync+atomic-rename pattern
(`main/wifiadminsettings.go`'s `atomicWriteJSON`, following
`main/alertsettings.go`'s `saveAlertSettings` exactly). At startup,
`wifiadmin.NewManager` loads last-known-good (falling back to
`DefaultConfig()` if none has ever been persisted) and checks for a
pending-transaction record; if one exists, the process crashed or was
restarted mid-transaction, and the constructor immediately rolls back to
that record's own previous configuration - never trusting the
proposed/unconfirmed one - clearing the record on success, or leaving it
in place and entering `RECOVERY_REQUIRED` if the rollback itself fails
(see `wifiadmin.NewManager`'s own doc comment and
`TestManager_StartupRecovery_*`).

## Concurrency and lock ordering

`wifiadmin.Manager` holds one mutex guarding all its own state; it never
calls its injected `Executor`/`Persistence` while holding that lock for
longer than the specific operation needs, and never holds it across an
HTTP round trip. `Preconditions` (OTA-busy, Configuration-Restore-busy,
shutdown-pending, recording-active - `main/wifiadminapi.go`) are plain
read-only checks against each subsystem's own existing state, taking
only that subsystem's own lock, never this package's. No goroutine is
spawned per request; the one background goroutine
(`wifiAdminDeadlinePoller`) only ever calls `CheckDeadline`, itself
short, lock-guarded, and a no-op unless a transaction is actually
awaiting confirmation. `TestManager_ConcurrentApplyAttempts` proves
exactly one of many simultaneous `Apply` calls against the same token
succeeds, deterministically, with `-race` clean.

## API endpoints

| Endpoint | Method | Purpose |
|---|---|---|
| `/getWifiAdminStatus` | GET | Current transaction stage, last result, redacted last-known-good and pending-proposed configuration, reconnect deadline. |
| `/previewWifiAdminSettings` | POST | Validate + diff a proposed configuration; issues an apply token. |
| `/applyWifiAdminSettings` | POST | Consume an apply token; writes config and restarts networking; issues a reconnect token. |
| `/confirmWifiAdminReconnection` | POST | Consume a reconnect token; commits the new configuration as last-known-good. |
| `/cancelWifiAdminChange` | POST | Discard a not-yet-applied preview. |
| `/rollbackWifiAdminChange` | POST | Manual rollback while awaiting confirmation or in recovery. |

All: strict JSON (`DisallowUnknownFields`, single-value body only),
8KiB body cap, explicit status codes (400 malformed/invalid, 409 a
precondition or another transaction blocks the request, 410 an
unknown/expired/used/stale token, 503 not yet initialized). No endpoint
here modifies, removes, or reinterprets the pre-existing
`/setSettings`/`/getSettings`.

## Dashboard behavior

New "Wi-Fi Admin" page (`web/plates/wifiadmin.html`/
`web/plates/js/wifiadmin.js`) - see that controller's own doc comment.
Required disclaimer shown before any action; a passphrase field is
always blank on load and never pre-filled from a server response; the
preview step always shows a field-by-field diff and an explicit
disconnection warning before Apply is available; the awaiting-
reconnection state is shown with an explicit Confirm control and a
manual rollback escape hatch; recovery-required is surfaced prominently.
This page does not replace or alter the pre-existing Wi-Fi panel in
`settings.html`.

**Layout validation performed this mission**: `node --check` (both
changed JS files) and a structural HTML tag-balance check only - this is
**emulated/reviewed, not physically rendered** on any device. Physical
responsive-layout validation (desktop, iPad/iPhone portrait/landscape)
is reserved for a future deployment mission, per this mission's own
"implementation and artifact-verification only" scope.

## Readiness and Preflight integration

`preflight.Input.WifiAdminStage` (a plain string mirroring
`wifiadmin.Stage`'s own values) feeds a new `wifiAdminChecks` card -
never above `Caution`, since this feature cannot affect ADS-B/GPS/GDL90
traffic processing at all, only the AP/network stack's own
configuration. No automated readiness check for
`wpa_supplicant`/`dnsmasq` process health was added - none existed
before this feature either (only the pre-existing manual preflight
checkbox), and adding one was out of this mission's scope.

## Diagnostics sanitization

`wifiAdminDiagnosticsSummary` (`main/wifiadminapi.go`): transaction
stage, last result, last error, and the current last-known-good
configuration in its own already-redacted (`wifiadmin.Redacted`) form.
Never a passphrase, a token, or a raw configuration file - `Redacted`
structurally has no field capable of holding a secret value at all (see
`wifiadmin.TestRedact_NeverExposesSecrets`).

## Configuration Backup

**Deliberately not integrated.** Wi-Fi/network settings are already
excluded from Configuration Backup by explicit, existing, documented
policy (`configbackup/document.go`, `main/configbackupapi.go`,
`docs/configuration-backup-restore.md` - all three quoted in "Current
architecture" above), described there as "network/credential surface,
out of scope," not merely "not yet implemented." This mission preserves
that contract exactly rather than broadening it: no wifiadmin fields
were added to `configbackup.ConfigurationSection` or any other exported
backup section, and this feature's own settings are not restorable
through that mechanism. This is a deliberate design decision, not an
oversight.

## Test strategy and inventory

- `wifiadmin/config_test.go` (20 tests): every validation rule listed
  above, boundary values, Unicode/control-character/injection strings,
  determinism, no-partial-mutation, `Redact`'s secret-freedom, and
  `DerivedDHCPRange`'s own never-collides-with-the-AP-address proof
  across every valid last octet.
- `wifiadmin/transaction_test.go` (22 tests): the full state machine -
  happy path, token expiry/mismatch/reuse/invalidated-by-intervening-
  change, apply failure with automatic rollback, rollback-itself-fails
  (`RECOVERY_REQUIRED`), cancel (allowed/rejected by stage), 10-way
  concurrent apply (exactly one succeeds), missed-confirmation automatic
  rollback, deadline-not-yet-reached is a no-op, explicit manual
  rollback, reconnect-token mismatch/reuse, confirm-before-apply,
  startup recovery (clean rollback and rollback-itself-fails paths),
  fresh-install default fallback, precondition blocking both preview and
  apply, and a persistence-failure-before-executor-call ordering proof.
- `main/wifiadminapi_test.go` (20 tests): every endpoint's methods,
  strict-JSON handling (malformed/multi-value/unknown-field/oversized),
  the full HTTP-level preview/apply/confirm round trip, precondition
  conflicts (409), unknown-token (410), apply-failure auto-rollback via
  the API, cancel/rollback endpoints, the `wifiadmin.Mode`/existing-
  `WifiMode*`-constant equivalence guard, diagnostics-summary shape, and
  `toNetworkTemplateParams`'s own derivation.
- `preflight/wifiadmin_checks_test.go` (5 tests): every stage maps to
  the documented State/Severity, and nothing here is ever
  `SeverityBlocking`.

## Repeated-test and race results

`go test`: clean for every affected package. `go test -race -count=3`
(the pure `wifiadmin` package, and `-count=1` x3 for `main` given its
cgo link cost): clean, except the same `TestAutoRecordAwaitMountAndReload_ReloadsOnceMountBecomesReady`
race already independently confirmed pre-existing on this branch's exact
base commit in an earlier mission (byte-identical
`main/autorecordrun.go`/`main/autorecordrun_test.go` between that base
and this branch's own base - no new evidence needed to re-confirm it).

## Static analysis

`go vet ./wifiadmin/... ./main/...`: clean except the same two
pre-existing, unrelated `main/datalog.go` unreachable-code findings
already on `master`. `gofmt -l`: clean on every changed file.
`node --check`: clean on both changed JavaScript files.

## Deployment checklist (not performed this mission)

This is an implementation-and-artifact-verification-only mission - see
the mission's own final report for the authoritative record. Before any
future deployment mission:

- [ ] Confirm the atomic four-file-batch apply behaves correctly against
      the real `/overlay/robase` overlay-unlock/lock cycle on real
      hardware (only exercised via a fake `Executor` this mission).
- [ ] Confirm a real `ifdown`/`ifup wlan0` cycle's actual timing on
      target hardware still comfortably fits within the 90s reconnect
      deadline.
- [ ] Confirm the dashboard's preview/apply/confirm flow end-to-end on a
      physical device, including an owner actually reconnecting to a
      renamed/re-secured network.
- [ ] Confirm automatic rollback recovers a genuinely bad configuration
      (e.g. an unreachable channel) on real hardware.
- [ ] Confirm ForeFlight/GDL90 reconnects normally after both a
      successful apply and an automatic rollback.
- [ ] Physical responsive-layout validation (desktop, iPad/iPhone
      portrait/landscape).

## Hardware-validation checklist

Identical to "Deployment checklist" above - not performed this mission.

## Recovery procedure

If `/getWifiAdminStatus` reports `recovery_required`: the automatic
rollback attempt itself failed (see `lastError` for why). Use
`/rollbackWifiAdminChange` (or the dashboard's "Retry rollback" button)
to try again; if that also fails, the device's Wi-Fi configuration may
need direct/serial/SD-card access to correct - this feature has no
capability beyond re-attempting the same rollback, by design (it does
not attempt increasingly aggressive recovery actions on its own).

## Rollback plan (for this feature itself)

This feature is purely additive: reverting this branch's commits removes
it entirely, leaving the pre-existing `/setSettings` path exactly as it
was. The two new persisted files
(`wifi-admin-last-known-good.json`/`wifi-admin-pending-transaction.json`)
are inert once the feature is removed - not read by anything else.

## Known limitations

- **Regulatory legality is not validated.** Country/channel format is
  checked; whether a given channel is actually legal to operate on in a
  given country, on this specific hardware, is not - this project has no
  authoritative source for that table.
- **A mid-sequence rename failure during the atomic four-file apply
  leaves a residual, narrow inconsistency window.** All four files are
  rendered to temp files first, but this project's overlay filesystem
  provides no cross-file transactional rename primitive - if the *first*
  rename in the batch succeeds and a *later* one fails, the files
  already renamed are live while the rest are not yet. Disclosed
  plainly, not hidden: see `wifiadminexecutor.go`'s own doc comment.
- **Reconnection confirmation cannot prove which network path a request
  arrived over**, nor which application is behind a connected client -
  see "Reconnection-confirmation behavior" above.
- **Not validated against real hardware this mission** - every
  Executor/Persistence dependency is a fake in every automated test; see
  "Deployment checklist."
