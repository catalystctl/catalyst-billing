package main

// Where a server goes, and how big it is: the configuration a *service* owns, not the adapter.
//
// # The split this file exists to enforce
//
// The panel URL and the API key are the integration's credentials: one panel, one key, set once by
// an operator. Everything else — which template, which location, which node, how much memory — is a
// property of *the thing being sold*. A Minecraft product is a Minecraft template; a "EU" product is
// a European location; a premium tier is more memory. Baking those into the adapter's own settings
// would mean one adapter per product.
//
// So each of them is resolved from, in order:
//
//  1. `metadata[key]` — a per-service value set from the adapter's own `provisioner.fields`
//     declaration, whose dropdowns are filled from the panel. This is the typed path: the operator
//     picks "US East 1" from a list and the panel has no way to accept a typo.
//  2. `metadata["env."+KEY]` — a product config option. A product can declare a config option whose
//     environment variable is `LOCATION`, and every service bought from that product inherits the
//     placement without an operator touching it. This is the path that makes a catalogue of
//     products work, and it is why the lookup is case-insensitive.
//  3. The adapter's own environment (`CCTL_TEMPLATE_ID`, …) — a default for an installation with one
//     product and no wish to configure anything.
//  4. A built-in default, where one is safe (size only).
//
// # Why names and ids are both accepted
//
// The protocol's dropdown carries a flat list of strings: there is no value/label pair to render
// "US East 1" beside `cmv8fxgml…`. So a choice is written `US East 1` when the name is unique and
// `US East 1 (cmv8fxgml…)` when it is not, and resolution accepts any of the three forms — the bare
// id, the exact name, or the rendered form. An operator who pastes an id is right; a stored value
// that survives a rename keeps working; and a genuinely ambiguous name is refused with the ids it
// could have meant rather than silently picking one.

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/catalyst/billing/internal/extproto"
)

// The metadata keys this adapter reads. They are also the `provisioner.fields` keys, so the form,
// the stored value and the create call cannot disagree about a name.
const (
	fieldLocation    = "location"
	fieldNode        = "node"
	fieldTemplate    = "template"
	fieldMemoryMB    = "memory_mb"
	fieldDiskMB      = "disk_mb"
	fieldCPUCores    = "cpu_cores"
	fieldServerName  = "server_name"
	fieldNetworkMode = "network_mode"
	fieldPort        = "port"
	fieldPrimaryIP   = "primary_ip"
)

// autoNode is the value that means "let the adapter choose within the location".
const autoNode = "auto"

// placement is the resolved answer to "where does this server go, and how big is it".
type placement struct {
	LocationID   string
	LocationName string
	NodeID       string
	NodeName     string
	TemplateID   string
	TemplateName string
	MemoryMB     int64
	DiskMB       int64
	CPUCores     int64
	NetworkMode  string
	// Port is a specific port to claim. Zero means "take the first free one on the node".
	Port int32
	// PrimaryIP is the address the server binds for macvlan and static-LAN networking. Empty means
	// "use the node's own address", which is right for host and bridge networking.
	PrimaryIP string
	// Name is the server's display name at the provider.
	Name string
}

// setting looks a placement value up through the four layers described above.
//
// `envKey` is the product-config-option form (LOCATION, TEMPLATE, …). It is upper-case and
// compared case-insensitively because an operator typing a config option's environment variable will
// write it the way an environment variable is normally written.
func (s *server) setting(metadata map[string]string, key, envKey, fallback string) string {
	if v := strings.TrimSpace(metadata[key]); v != "" {
		return v
	}
	for name, value := range metadata {
		if !strings.HasPrefix(name, "env.") {
			continue
		}
		if strings.EqualFold(strings.TrimPrefix(name, "env."), envKey) {
			if v := strings.TrimSpace(value); v != "" {
				return v
			}
		}
	}
	if v := strings.TrimSpace(fallback); v != "" {
		return v
	}
	return ""
}

// resolvePlacement turns a purchase's metadata into a concrete placement.
//
// Every failure here is a business rejection with a sentence an operator can act on: a create that
// cannot say where the server should go must not guess, because guessing bills a customer for a
// server in the wrong place.
func (s *server) resolvePlacement(ctx context.Context, metadata map[string]string) (placement, error) {
	var out placement

	locationValue := s.setting(metadata, fieldLocation, "LOCATION", s.cfg.locationID)
	if locationValue == "" {
		return out, fmt.Errorf("a location is required: set the %q provisioning field or a LOCATION config option", fieldLocation)
	}
	locationID, locationName, err := s.resolveLocation(ctx, locationValue)
	if err != nil {
		return out, err
	}
	out.LocationID, out.LocationName = locationID, locationName

	templateValue := s.setting(metadata, fieldTemplate, "TEMPLATE", s.cfg.templateID)
	if templateValue == "" {
		return out, fmt.Errorf("a template is required: set the %q provisioning field or a TEMPLATE config option", fieldTemplate)
	}
	templateID, templateName, err := s.resolveTemplate(ctx, templateValue)
	if err != nil {
		return out, err
	}
	out.TemplateID, out.TemplateName = templateID, templateName

	// Size. Precedence: an explicit field, then a configured size bundle matching the billing
	// catalogue's plan id, then the built-in default. Numbers are parsed strictly enough to refuse
	// "-1" or "2GB", because a size is a resource commitment rather than a label.
	defaults := s.sizeDefaults()
	mem, err := sizeSetting(s.setting(metadata, fieldMemoryMB, "MEMORY_MB", ""), defaults.MemoryMB, "memory_mb")
	if err != nil {
		return out, err
	}
	disk, err := sizeSetting(s.setting(metadata, fieldDiskMB, "DISK_MB", ""), defaults.DiskMB, "disk_mb")
	if err != nil {
		return out, err
	}
	cpu, err := sizeSetting(s.setting(metadata, fieldCPUCores, "CPU_CORES", ""), defaults.CPUCores, "cpu_cores")
	if err != nil {
		return out, err
	}
	out.MemoryMB, out.DiskMB, out.CPUCores = mem, disk, cpu

	out.Name = s.setting(metadata, fieldServerName, "SERVER_NAME", "")
	if out.Name == "" {
		out.Name = serverName(metadata, metadata["service_id"])
	}

	if raw := s.setting(metadata, fieldPrimaryIP, "PRIMARY_IP", ""); raw != "" {
		if !looksLikeIP(raw) {
			return out, fmt.Errorf("primary_ip %q is not an IP address", raw)
		}
		out.PrimaryIP = raw
	}

	if mode := s.setting(metadata, fieldNetworkMode, "NETWORK_MODE", s.cfg.networkMode); mode != "" {
		if !validNetworkMode(mode) {
			return out, fmt.Errorf("network_mode %q is not one of bridge, macvlan, host, mc-lan-static, mc-lan-dynamic", mode)
		}
		out.NetworkMode = mode
	}

	if raw := s.setting(metadata, fieldPort, "PORT", ""); raw != "" && !strings.EqualFold(raw, autoNode) {
		port, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || port < 1 || port > 65535 {
			return out, fmt.Errorf("port %q is not a port number", raw)
		}
		out.Port = int32(port)
	}

	// Node last, because pinning one inside a location is checked against the location: a node in
	// another location is a configuration mistake, not a valid placement.
	nodeValue := s.setting(metadata, fieldNode, "NODE", s.cfg.nodeID)
	node, err := s.chooseNode(ctx, locationID, locationName, nodeValue, mem, cpu)
	if err != nil {
		return out, err
	}
	out.NodeID, out.NodeName = node.ID, node.Name
	return out, nil
}

// sizeSetting parses a positive integer size with a default.
func sizeSetting(raw string, fallback int64, label string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s %q must be a positive whole number", label, raw)
	}
	return n, nil
}

// validNetworkMode reports whether the panel accepts the mode.
func validNetworkMode(mode string) bool {
	switch mode {
	case "bridge", "macvlan", "host", "mc-lan-static", "mc-lan-dynamic":
		return true
	}
	return false
}

// resolveLocation resolves a location by id, name, or the rendered "Name (id)" form.
func (s *server) resolveLocation(ctx context.Context, value string) (string, string, error) {
	locations, err := s.client.locations(ctx)
	if err != nil {
		return "", "", err
	}
	loc, ok := matchNamed(value, locations, func(l panelLocation) (string, string) { return l.ID, l.Name })
	if !ok {
		return "", "", fmt.Errorf("location %q does not exist on this panel; known locations: %s",
			value, namedChoices(locations, func(l panelLocation) (string, string) { return l.ID, l.Name }))
	}
	return loc.ID, loc.Name, nil
}

// resolveTemplate resolves a template by id or name.
//
// An id is tried against the panel directly first. A panel has hundreds of templates and the list is
// paged, so "resolve the id from the list" fails for anything late in the catalogue — and that failure
// would tell an operator their template does not exist, which is exactly wrong.
func (s *server) resolveTemplate(ctx context.Context, value string) (string, string, error) {
	if looksLikePanelID(value) {
		tpl, err := s.client.templateByID(ctx, value)
		switch {
		case err == nil:
			return tpl.ID, tpl.Name, nil
		case !isNotFound(err):
			return "", "", err
		}
		// Not an id the panel knows. Fall through to the list, because an operator may have typed a
		// name that happens to look like one.
	}

	templates, err := s.client.templates(ctx)
	if err != nil {
		return "", "", err
	}
	tpl, ok := matchNamed(value, templates, func(t panelTemplate) (string, string) { return t.ID, t.Name })
	if !ok {
		return "", "", fmt.Errorf("template %q does not exist on this panel; known templates: %s",
			value, namedChoices(templates, func(t panelTemplate) (string, string) { return t.ID, t.Name }))
	}
	return tpl.ID, tpl.Name, nil
}

// chooseNode picks the node a server lands on.
//
// Three cases, and the third is the one the feature exists for:
//
//   - a value names a node: pin to it, after checking it belongs to the chosen location;
//   - the adapter's own CCTL_NODE_ID is set: use it;
//   - otherwise load-balance: among online nodes in the location, take the one with the most free
//     memory, breaking a tie on free CPU and then on id so the same request set always produces the
//     same answer. Determinism matters because a create that times out is retried, and "which node"
//     must not change between attempts for no reason.
func (s *server) chooseNode(ctx context.Context, locationID, locationName, value string, needMemory, needCPU int64) (panelNode, error) {
	nodes, err := s.client.nodes(ctx)
	if err != nil {
		return panelNode{}, err
	}

	var inLocation []panelNode
	for _, node := range nodes {
		if node.LocationID == locationID {
			inLocation = append(inLocation, node)
		}
	}
	if len(inLocation) == 0 {
		return panelNode{}, fmt.Errorf("location %q has no nodes yet; add one on the panel's nodes screen", locationName)
	}

	if value != "" && !strings.EqualFold(value, autoNode) {
		node, ok := matchNamed(value, inLocation, func(n panelNode) (string, string) { return n.ID, n.Name })
		if !ok {
			// A node that exists but is in another location is a different mistake from one that does
			// not exist, and the sentence has to say which.
			if elsewhere, ok := matchNamed(value, nodes, func(n panelNode) (string, string) { return n.ID, n.Name }); ok {
				// Name the location the operator picked, not its id: the sentence is read by someone
				// looking at a form that says "US East 1", not at the panel's database.
				return panelNode{}, fmt.Errorf("node %q is in another location, not %q",
					elsewhere.Name, locationName)
			}
			return panelNode{}, fmt.Errorf("node %q does not exist on this panel; nodes in this location: %s",
				value, namedChoices(inLocation, func(n panelNode) (string, string) { return n.ID, n.Name }))
		}
		return node, nil
	}

	// Load-balance across the online nodes of the location.
	candidates := make([]nodeCapacity, 0, len(inLocation))
	for _, node := range inLocation {
		if !node.IsOnline {
			continue
		}
		stats, err := s.client.nodeStats(ctx, node.ID)
		if err != nil {
			// One unreadable node must not fail the purchase while another node has room, so it is
			// skipped. If every node is unreadable the loop ends empty and the error below says so.
			continue
		}
		res := stats.Resources
		freeMemory := res.MaxMemoryMb - res.AllocatedMemoryMb
		freeCPU := res.MaxCpuCores - res.AllocatedCpuCores
		if freeMemory < needMemory || freeCPU < needCPU {
			continue
		}
		candidates = append(candidates, nodeCapacity{node: node, freeMemory: freeMemory, freeCPU: freeCPU})
	}
	if len(candidates) == 0 {
		return panelNode{}, fmt.Errorf(
			"no online node in location %q has %d MB of memory and %d CPU core(s) free; "+
				"add a node or choose a smaller plan", locationName, needMemory, needCPU)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].freeMemory != candidates[j].freeMemory {
			return candidates[i].freeMemory > candidates[j].freeMemory
		}
		if candidates[i].freeCPU != candidates[j].freeCPU {
			return candidates[i].freeCPU > candidates[j].freeCPU
		}
		return candidates[i].node.ID < candidates[j].node.ID
	})
	return candidates[0].node, nil
}

// nodeCapacity is one load-balancing candidate.
type nodeCapacity struct {
	node       panelNode
	freeMemory int64
	freeCPU    int64
}

// looksLikePanelID reports whether a value is shaped like one of the panel's ids.
//
// The panel's ids are long, unpunctuated and have no spaces (a cuid or a nanoid). The test is
// deliberately loose — it only decides whether to *ask the panel* about the value before searching names,
// and a wrong guess costs one request that returns 404.
func looksLikePanelID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 10 || len(value) > 64 || strings.ContainsAny(value, " :@/") {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// named is anything with an id and a name the resolvers can search.
type named interface {
	panelLocation | panelTemplate | panelNode
}

// matchNamed finds one object by id, exact name, or the rendered "Name (id)" form.
//
// The three forms exist because the protocol's dropdown carries one string per choice: the rendered
// form is what an operator sees and copies, the name is what a human types, and the id is what
// survives a rename. Matching is case-insensitive on the name because a location called "US East 1"
// and an operator typing "us east 1" mean the same thing.
func matchNamed[T named](value string, options []T, fields func(T) (string, string)) (T, bool) {
	var zero T
	value = strings.TrimSpace(value)

	// The rendered "Name (id)" form: the id in the trailing parentheses is authoritative.
	if open := strings.LastIndex(value, " ("); open > 0 && strings.HasSuffix(value, ")") {
		value = strings.TrimSpace(value[open+2 : len(value)-1])
	}

	for _, option := range options {
		id, _ := fields(option)
		if id == value {
			return option, true
		}
	}
	for _, option := range options {
		_, name := fields(option)
		if strings.EqualFold(strings.TrimSpace(name), value) {
			return option, true
		}
	}
	return zero, false
}

// namedChoices renders the choices for a field, and for an error message.
//
// A name that appears twice is disambiguated with the id, because a dropdown with two identical
// entries is a dropdown that cannot express the operator's choice.
func namedChoices[T named](options []T, fields func(T) (string, string)) string {
	counts := map[string]int{}
	for _, option := range options {
		_, name := fields(option)
		counts[strings.ToLower(strings.TrimSpace(name))]++
	}
	out := make([]string, 0, len(options))
	for _, option := range options {
		id, name := fields(option)
		if counts[strings.ToLower(strings.TrimSpace(name))] > 1 {
			out = append(out, fmt.Sprintf("%s (%s)", name, id))
			continue
		}
		out = append(out, name)
	}
	return strings.Join(out, ", ")
}

// declaredFields is the `provisioner.fields` answer: what this service must say about placement.
//
// The dropdowns are filled from the panel on every call, so a template added five minutes ago is
// selectable and a deleted one is not. When the panel cannot be reached the dropdown becomes a text
// field instead of disappearing: a form that silently loses a required input cannot be completed,
// whereas a text field lets an operator who knows the id proceed off a cached list.
func (s *server) declaredFields(ctx context.Context) []extproto.ProvisionField {
	fields := make([]extproto.ProvisionField, 0, 9)

	locations, locErr := s.client.locations(ctx)
	locationChoices := make([]string, 0, len(locations))
	for _, loc := range locations {
		locationChoices = append(locationChoices, renderChoice(loc.ID, loc.Name))
	}
	fields = append(fields, choiceField(fieldLocation, "Location", locationChoices, locErr, true,
		"Where the server runs. Its node is chosen automatically unless you pin one."))

	// Nodes are listed across every location: the field is validated against the chosen location at
	// create time, and narrowing it here would mean the list changes under the operator as they pick
	// a location, which the form cannot express.
	nodes, nodeErr := s.client.nodes(ctx)
	nodeChoices := []string{autoNode}
	for _, node := range nodes {
		label := node.Name
		if !node.IsOnline {
			label += " — offline"
		}
		nodeChoices = append(nodeChoices, renderChoice(node.ID, label))
	}
	fields = append(fields, choiceField(fieldNode, "Node", nodeChoices, nodeErr, false,
		fmt.Sprintf("%q balances the load across the location's online nodes and pins nothing.", autoNode)))

	templates, tplErr := s.client.templates(ctx)
	templateChoices := make([]string, 0, len(templates))
	for _, tpl := range templates {
		templateChoices = append(templateChoices, renderChoice(tpl.ID, tpl.Name))
	}
	fields = append(fields, choiceField(fieldTemplate, "Template", templateChoices, tplErr, true,
		"The game or service this server runs, e.g. Paper for Minecraft."))

	// The three sizes carry defaults, so none of them is required.
	//
	// Marking one required while the adapter happily defaults it makes an operator's screen show
	// "required, not set" for a configuration that provisions perfectly — a red state on a valid input
	// teaches people to ignore red states. The hint names the default, so leaving it empty is an
	// informed choice rather than an accident.
	fields = append(fields,
		extproto.ProvisionField{
			Key: fieldMemoryMB, Label: "Memory (MB)", Type: extproto.FieldNumber,
			Hint: "At least 512. Defaults to 2048; a Minecraft server wants 2048 or more.",
		},
		extproto.ProvisionField{
			Key: fieldDiskMB, Label: "Disk (MB)", Type: extproto.FieldNumber,
			Hint: "At least 1024. Defaults to 10240 (10 GB).",
		},
		extproto.ProvisionField{
			Key: fieldCPUCores, Label: "CPU cores", Type: extproto.FieldNumber,
			Hint: "Whole cores, at least 1. Defaults to 1.",
		},
		extproto.ProvisionField{
			Key: fieldServerName, Label: "Server name", Type: extproto.FieldText,
			Hint: "What the customer sees in the panel. Defaults to \"Server <service id>\".",
		},
		extproto.ProvisionField{
			Key: fieldPort, Label: "Port", Type: extproto.FieldNumber,
			Hint: "Leave empty to claim the next free port on the node.",
		},
		// Not required: an empty answer takes the installation default, which is bridge. Bridge is the
		// supported mode — it is the only one where the panel reserves a host port through an allocation,
		// so concurrent creates cannot collide and the customer's address is stable. The other modes are
		// listed because a panel may already have servers on them, not because they are recommended.
		choiceField(fieldNetworkMode, "Network mode",
			[]string{"bridge", "macvlan", "host", "mc-lan-static", "mc-lan-dynamic"}, nil, false,
			"Leave empty for bridge, which is the supported mode."),
		extproto.ProvisionField{
			Key: fieldPrimaryIP, Label: "Static IP", Type: extproto.FieldText,
			Hint: "Only for macvlan or static-LAN networking. Leave empty to bind the node's own address.",
		},
	)
	return fields
}

// choiceField builds a dropdown when the choices are known, and a text field when they are not.
//
// A required field whose list could not be fetched becomes an optional text field rather than a
// required one: the operator can type an id they know, and the create call still validates it against
// the panel. Requiring a field with no options would make the form unsubmittable.
func choiceField(key, label string, choices []string, err error, required bool, hint string) extproto.ProvisionField {
	usable := make([]string, 0, len(choices))
	for _, choice := range choices {
		if strings.TrimSpace(choice) != "" {
			usable = append(usable, choice)
		}
	}
	if err != nil || len(usable) == 0 {
		return extproto.ProvisionField{
			Key: key, Label: label, Type: extproto.FieldText,
			Hint: hint + " The panel could not be listed, so enter the id.",
		}
	}
	return extproto.ProvisionField{
		Key: key, Label: label, Type: extproto.FieldDropdown, Choices: usable, Required: required, Hint: hint,
	}
}

// renderChoice is the one string a dropdown shows for an object.
func renderChoice(id, name string) string {
	if strings.TrimSpace(name) == "" {
		return id
	}
	return name
}

// placementOptions is the set of config-option names this adapter consumes rather than forwards.
//
// It is the upper-case form of every placement field, plus the owner: each one is an instruction to
// the adapter, and forwarding it would put it in a game server's process environment.
var placementOptions = map[string]struct{}{
	"LOCATION":     {},
	"NODE":         {},
	"TEMPLATE":     {},
	"MEMORY_MB":    {},
	"DISK_MB":      {},
	"CPU_CORES":    {},
	"SERVER_NAME":  {},
	"NETWORK_MODE": {},
	"PORT":         {},
	"OWNER":        {},
	"OWNER_ID":     {},
}

// isPlacementOption reports whether a config option is one this adapter consumes.
func isPlacementOption(name string) bool {
	_, consumed := placementOptions[strings.ToUpper(strings.TrimSpace(name))]
	return consumed
}

// looksLikeIP reports whether a value parses as an IP address.
//
// The check is here rather than only at the panel because a wrong address is refused much later, by the
// container runtime, and the customer would see a server that never comes up instead of a purchase that
// was refused for a reason they can fix.
func looksLikeIP(value string) bool {
	return net.ParseIP(strings.TrimSpace(value)) != nil
}

// usesBridgeNetworking reports whether a network mode needs a panel allocation.
//
// The panel reserves a host port through an allocation, and it accepts an `allocationId` ONLY for bridge
// networking: every other mode binds an address directly and refuses the allocation with
// SERVER_NETWORK_MODE_INVALID. Verified against the live panel, and the reason a create that always sent
// one was refused for any template whose default mode is host.
func usesBridgeNetworking(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "bridge")
}
