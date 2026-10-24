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

### Networking: bridge

This integration uses **bridge networking**, and the adapter sends it explicitly on every create. Bridge is
the only mode where the panel reserves a host port through an allocation, which is what makes two
concurrent creates unable to collide on the same port and gives the customer a stable address to connect
to.

Left unset, the panel picks its own default — `mc-lan-static` on the reference installation — which needs a
static IP pool per node and hands out an address an operator has to manage. Sending `bridge` explicitly
means one code path, one pool requirement, and no dependence on what a particular panel happens to default
to.

`CCTL_NETWORK_MODE` can still override it, and a service's own `network_mode` field wins over both, for an
installation that already runs servers on another network.

### Panel prerequisites

A panel cannot accept a server until it can give it an address, and **bridge needs one IP pool on the
node**. With none, the panel refuses every create with `SERVER_CREATE_FAILED: No IP pool configured for
this network` — a failure that surfaces five queue attempts later, not at configuration time.

```sh
curl -X POST "$PANEL/api/admin/ip-pools" -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"nodeId":"…","networkName":"bridge","cidr":"192.168.1.0/24",
       "startIp":"192.168.1.200","endIp":"192.168.1.250"}'
```

The pool's CIDR is the bridge subnet, so pick one that cannot collide with the node's own network or with
anything else routed on your LAN — `172.30.0.0/16` is a safe habit. Point it at the node's existing bridge
subnet if you already have one.

The panel's IPAM then assigns each server an address from that pool. The adapter does **not** send an
address unless a service asks for a static one, because a self-chosen address would sit outside the pool
and collide with the node.

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
| `CCTL_OWNER_ID` | A fixed panel user that owns new servers, instead of a panel account per customer |
| `CCTL_USER_MODE` | `create` (default) gives each customer a panel account of their own; `none` leaves ownership to the panel |
| `CCTL_USER_ROLE_ID` | The panel role a new customer account gets. Choose the least-privileged role (`User`), never `Administrator` |
| `CCTL_PLANS` | `id:memory_mb:disk_mb:cpu_cores,…` bundles, so a billing plan id maps to a size |
| `CCTL_NETWORK_MODE` | Defaults to `bridge`. Override only for a panel that already runs servers on another network. |
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
| `primary_ip` | Only for a non-bridge network that needs a static address. Empty lets the panel's IPAM allocate. |
| `template` | **Required.** The game or service, e.g. Paper. |
| `memory_mb`, `disk_mb`, `cpu_cores` | Defaults 2048 / 10240 / 1. The panel's minimums are 512 / 1024 / 1. |
| `server_name` | What the customer sees in the panel; defaults to `Server <service id>`. |
| `port` | Empty takes the next free port on the node. A named port is claimed exactly or the purchase is refused. |
| `network_mode` | Empty uses `bridge`, which is the supported mode. |

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

## The panel account a server belongs to

A game panel is multi-tenant: every server has an owner, and an owner is a panel login. A billing service
is not a panel login, so a create **resolves or creates a panel account for the customer** and gives the
server to it. Without that, every customer's server lives in the API key's own account: one leaked panel
login exposes all of them, and the console link we show the customer opens a server that is not theirs.

- Matching is by **email address**, case-insensitively, which is what the panel enforces uniqueness on.
- When no account exists, one is created with the customer's address, a derived username (`alice`, then
  `alice1` if taken), and a **random password that is never stored, logged or shown**. Billing has no
  password to hand over, and inventing a predictable one would make every customer account guessable. The
  customer reaches the panel through its own password reset, which is the flow that verifies the address.
- An explicit owner wins: a service whose `OWNER_ID` option is set (a reseller product) places the server in
  that panel account and creates nothing.
- A service with no customer email (an import, a back-office fixture) creates no account and leaves
  ownership to the panel.

Two facts learned against a live panel, both of which are handled:

- The create reply is a **bare** user object, not the `{success,data}` envelope most writes use. A parser
  that assumed the envelope created the account and then reported failure — leaving a stranded account and
  a permanent 409 for that customer.
- The account **search is not immediately consistent**. A lookup issued in the same millisecond as the
  create can miss the account it just made, so the adopt-existing path retries with a short backoff and
  falls back to reading the account list.

Deleting a panel account that still owns a server is refused by the panel (`500`). Terminating a service
therefore leaves the account in place: it may own other services, and one termination must not revoke
somebody's panel login. Removing a customer entirely is a separate, deliberate operation on the panel.

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
| `SERVER_CREATE_FAILED: No IP pool configured for this network` | The node has no IP pool for that network. See [panel prerequisites](#panel-prerequisites) |
| `SERVER_NETWORK_MODE_INVALID: Allocation IDs are only valid for bridge networking` | A service's `network_mode` field names a non-bridge mode while the adapter claims an allocation. Fixed for bridge; rebuild the binary if this appears with no `network_mode` set |
| A create is refused naming a location | `location` is unset on the service and the product has no `LOCATION` option |
| A template that exists is reported missing | The panel has more templates than one page. The adapter pages the list and resolves an id directly; rebuild the binary if this appears |
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

And end to end against a real panel — registers a customer, buys a product, pays, provisions, then checks
the panel itself:

```sh
PANEL_URL=https://panel.example.com PANEL_KEY=catalyst_… \
BILLING_PROVISIONER_EXTENSION=$PWD/bin/catalystctl ./scripts/e2e-catalystctl.sh
```

It asserts, against the panel rather than the billing database: that a **panel account was created** for
the customer and the server is owned by it, that the server landed on the requested template, node and
size, that the portal card reports the same name, address and link, that a power action from the portal
changes the panel's state and writes an attributed audit row, and that suspend and resume reach the panel.
`KEEP=1` leaves the panel server in place; the default removes it, because a live panel accumulates strays.
