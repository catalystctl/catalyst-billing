# CatalystCTL game servers

`extensions/catalystctl` connects a CatalystCTL panel to this billing panel, so a customer buys a game
server here and then manages it here too: live state, performance, the connection address, a button that
opens the panel's console, and start / stop / restart / force-stop.

It speaks the standard provisioner protocol, so the queue, leases, fencing and reconciliation described
in [extensions.md](extensions.md) apply unchanged. This page is about the two things that are specific to
it: **what you configure**, and **what a customer sees**.

## The split: credentials belong to the installation, placement belongs to the product

This is the whole design, and getting it wrong is what makes a panel integration painful later.

| Who owns it | What |
| --- | --- |
| **The installation** (once) | The panel's URL, and the API key. |
| **The product / service** (per thing you sell) | Which template, which location, which node, how much memory, disk and CPU, the network mode, the port, the server's name, which panel user owns it. |

One adapter therefore serves an entire catalogue. A "Minecraft · EU · 4 GB" product is a template, a
location and a memory size stored on that product — not a second deployment of the adapter.

### Configure the installation

**Settings → Integrations → Servers**, or the equivalent settings rows:

| Setting | Value |
| --- | --- |
| `servers.provider` | `catalystctl` |
| `servers.panel_url` | The panel's origin, e.g. `https://panel.catalystctl.com` — no `/api` suffix |
| `servers.api_key` | A panel API key starting `catalyst_…`. Write-only: it is never read back, only reported as set |

The key needs the panel permissions that cover what you use: servers (read, create, delete), console, and
nodes (read) for the port allocations, plus network/locations/templates read for the placement dropdowns.

The binary must be where the panel looks for it — `Billing → Integrations` shows whether it was found:

```sh
make extensions                      # builds bin/catalystctl with the rest
# or, to point at a build elsewhere:
BILLING_PROVISIONER_EXTENSION=/opt/catalyst/catalystctl
```

Optional environment variables, for an installation that wants a default rather than setting it per
service:

| Variable | Effect |
| --- | --- |
| `CCTL_TEMPLATE_ID` / `CCTL_LOCATION_ID` | The default template and location, when a service sets none |
| `CCTL_NODE_ID` | Pin every server to one node, disabling load balancing |
| `CCTL_OWNER_ID` | The panel user that owns new servers |
| `CCTL_PLANS` | `id:memory_mb:disk_mb:cpu_cores,…` bundles, so a billing plan id maps to a size |
| `CCTL_NETWORK_MODE` | `bridge`, `macvlan`, `host`, `mc-lan-static`, `mc-lan-dynamic` |
| `CCTL_PANEL_PATH` | The customer-facing path of a server; default `/servers/{id}`. Set it to `/` to turn the deep link off |
| `CCTL_STATE_FILE` | Where the adapter records what it has done; default `/var/lib/catalyst/catalystctl/operations.json` |
| `CCTL_TIMEOUT_MS` | Per-call HTTP timeout; default `30000` |
| `CCTL_VERIFY_TLS` | Default on. Turning it off prints a warning on the adapter's stderr |

The state file must be writable and must survive a restart. See [why it exists](#why-there-is-a-state-file).

### Configure the placement, per service

On a service, **Provisioning inputs** shows the fields the adapter declares. The dropdowns are filled
from the panel on every render, so a template or node added five minutes ago is selectable and a deleted
one is not:

| Field | Meaning |
| --- | --- |
| `location` | **Required.** Where the server runs. The node is chosen inside it. |
| `node` | Default `auto`: balance across the location's online nodes. A named node pins the server there, and is refused if that node is in a different location. |
| `template` | **Required.** The game or service, e.g. Paper. |
| `memory_mb`, `disk_mb`, `cpu_cores` | Defaults 2048 / 10240 / 1. The panel's minimums are 512 / 1024 / 1. |
| `server_name` | What the customer sees in the panel; defaults to `Server <service id>`. |
| `port` | Empty takes the next free port on the node. A named port is claimed exactly or the purchase is refused. |
| `network_mode` | Empty uses the template's own default. |

Each field can also come from a **product config option** whose environment variable matches the field's
name in upper case — `LOCATION`, `TEMPLATE`, `MEMORY_MB`, `DISK_MB`, `CPU_CORES`, `NODE`, `PORT`,
`NETWORK_MODE`, `SERVER_NAME`, `OWNER_ID`. That is how a whole product line gets its placement without
anybody touching a service: declare the option on the product, and every service bought from it inherits
it.

Precedence, highest first:

1. the service's own provisioning field;
2. a product config option (`env.LOCATION`, matched case-insensitively);
3. the installation's environment default;
4. the built-in default (sizes only).

Options that name placement are **consumed**, not passed to the game server: a product's `LOCATION` decides
where the server is built, and does not appear as a `LOCATION` variable in the server's process. Every
other `env.*` option is passed through, which is how a `MINECRAFT_VERSION` option reaches the container.

### Load balancing

With `node` left on `auto`, the adapter:

1. takes the nodes in the chosen location;
2. drops the offline ones;
3. drops the ones without enough *unallocated* memory and CPU for the requested size;
4. picks the one with the most free memory, then the most free CPU, then the lowest id.

Step 4 is deterministic on purpose. A create that times out is retried, and "which node" must not change
between attempts for no reason. If no node fits, the purchase is refused with a sentence naming the size
that did not fit — not queued to fail later.

## What the customer sees

**Portal → Services → the service** gets a Server card:

- the server's **name**, taken from the panel on every read — a rename in the panel shows up here;
- its **state**, in the panel's own words plus a sentence, e.g. "Your server is stopped. Start it to play.";
- the **address** to connect to, with a copy button;
- **CPU, memory, disk and network**, each against its limit, with the timestamp of the figures;
- **Open in panel**, a deep link to the server's console;
- **Start / Stop / Restart / Force stop**, offering only what the provider says it would accept *now*;
- a silent refresh every 15 seconds while the tab is visible.

Force stop asks first: it cuts the power without saving, which is the one action that can lose progress.

Support sees the same card on **Services → the service**, so an agent can answer "is it running" without
opening the game panel either. The card is one component used by both pages; they cannot drift.

### When there is nothing to show

The card distinguishes three states, and none of them is an error page:

| State | What the customer reads |
| --- | --- |
| `provisioned: false` | "This service has no server yet." — normal for the first minute after a purchase |
| `available: false` | A sentence explaining that the panel could not be reached, and that the service is unaffected |
| live | The card above |

A game panel being down must never make the billing panel look broken, so an unreachable provider is a
`200` with a sentence — never a `5xx`. The provider's own error text is not repeated to a customer: it is
written for operators and can name internal hosts.

## Why there is a state file

The protocol requires `provisioner.lookup` — "did I already create a server for this purchase?" — to be
authoritative, because a create that times out may or may not have created anything, and the reconciliation
loop re-asks rather than re-does.

Pterodactyl answers that from its own database, because it stores an `external_id` on every server.
**Catalyst has no such field**: a server has a name, a description and an id, and nothing a create request
can set as a foreign key. So the adapter answers two ways:

1. **A local ledger** (`CCTL_STATE_FILE`). The operation key is written *before* the create is sent, and the
   server id is added as soon as it exists. This survives restarts and is the authoritative record.
2. **A description marker.** A created server carries `billing:<operation_key>` in its description, so a scan
   of the panel can re-establish the association if the ledger is lost.

A lookup that has a recorded id re-reads the panel to confirm the server still exists before claiming it
does. A lookup that has to scan treats *running out of pages* as an error, never as "not found" — reporting
absence is what would provision a second server.

Back the state file up with the database. Losing it degrades a lookup to a bounded scan, and a scan that
cannot find the marker refuses rather than duplicating.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| "Server control is not set up yet" on every service | `servers.provider` is not `catalystctl`, or no API key is saved |
| "The panel could not be listed, so enter the id" | The key cannot read locations/templates/nodes, or the panel is unreachable. The field becomes a text box so a known id can still be used |
| A create is refused naming a location | `location` is unset on the service and the product has no `LOCATION` option |
| "no free port allocation on node …" | The node has no unassigned ports. Add them on the panel's node screen |
| "no online node … has N MB of memory and N CPU core(s) free" | The requested size does not fit the location's capacity |
| The card says the panel does not recognise the service | The service's `external_id` names a server that was deleted in the panel |
| Every operation fails with an authentication error | The API key was revoked or lacks the permission for that call. It is a retryable failure, so it recovers once the key is fixed |

## Verifying an installation

```sh
go test ./extensions/catalystctl/ -count=1     # protocol, placement, power, ledger, conformance
go test ./internal/httpapi/ -run 'Server|Card' # the routes, against the real adapter
SERVICE_ID=<a provisioned service> node scripts/verify-server-card.mjs   # the card, in a browser
```
