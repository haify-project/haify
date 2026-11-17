/**
 * Helpers for reading drbd-reactor promoter TOML in the HA OCF agent editor.
 */

/**
 * Extract the items of the `start = [ ... ]` array from arbitrary TOML.
 * Handles multi-line arrays and both quote styles. Returns null if no start
 * array is found.
 */
export function extractStartArrayItems(content: string): string[] | null {
  // Locate `start` assignment (start of line, optional whitespace).
  const startMatch = content.match(/^[ \t]*start[ \t]*=[ \t]*\[/m);
  if (!startMatch || startMatch.index === undefined) return null;

  const openIndex = content.indexOf('[', startMatch.index);
  // Scan to the matching closing bracket, respecting quotes.
  let depth = 0;
  let inQuote: '"' | "'" | null = null;
  let endIndex = -1;
  for (let i = openIndex; i < content.length; i++) {
    const ch = content[i];
    if (inQuote) {
      if (ch === inQuote) inQuote = null;
      continue;
    }
    if (ch === '"' || ch === "'") {
      inQuote = ch;
    } else if (ch === '[') {
      depth++;
    } else if (ch === ']') {
      depth--;
      if (depth === 0) {
        endIndex = i;
        break;
      }
    }
  }
  if (endIndex === -1) return null;

  const inner = content.slice(openIndex + 1, endIndex);
  // Extract quoted strings ("..." or '...').
  const items: string[] = [];
  const itemRegex = /"((?:[^"\\]|\\.)*)"|'([^']*)'/g;
  let m: RegExpExecArray | null = itemRegex.exec(inner);
  while (m) {
    const raw = m[1] !== undefined ? m[1].replace(/\\(.)/g, '$1') : m[2];
    const trimmed = raw.trim();
    if (trimmed) items.push(trimmed);
    m = itemRegex.exec(inner);
  }
  return items;
}

/**
 * Render a configured OCF agent as its promoter `start[]` line:
 *   ocf:<provider>:<name> <instance> k=v k='v v' ...
 * Values containing whitespace or commas are single-quoted. Empty params are
 * skipped. Mirrors how the backend composes the start array.
 */
export function ocfAgentToStartLine(agent: {
  provider: string;
  name: string;
  instance: string;
  params: Record<string, string>;
}): string {
  const head = `ocf:${agent.provider}:${agent.name} ${agent.instance}`;
  const paramStr = Object.entries(agent.params)
    .filter(([, value]) => value !== undefined && value !== '')
    .map(([key, value]) => {
      const v = String(value);
      if (v.includes(' ') || v.includes(',')) {
        return `${key}='${v}'`;
      }
      return `${key}=${v}`;
    })
    .join(' ');
  return paramStr ? `${head} ${paramStr}` : head;
}

/**
 * Systemd mount unit name for a mount path, matching the backend
 * generatePromoterConfig: only the single leading slash is trimmed and the
 * remaining slashes become dashes (no full systemd-escape), e.g.
 *   /var/lib/redisha -> var-lib-redisha   (".mount" appended by the caller)
 */
function mountUnitName(mountPoint: string): string {
  return mountPoint.replace(/^\//, '').replace(/\//g, '-');
}

/**
 * service-ip@ instance name for a VIP, matching the backend
 * vipServiceIPInstance:
 *   192.168.1.50/24 -> 192.168.1.50-24
 *   192.168.1.50    -> 192.168.1.50-32   (a bare IP defaults to /32)
 * Returns "" for an empty VIP.
 */
function vipServiceIpInstance(vip: string): string {
  const v = vip.trim();
  if (!v) return '';
  let inst = v.replace(/\//g, '-');
  if (!inst.includes('-')) inst = `${inst}-32`;
  return inst;
}

/**
 * Render one OCF agent exactly as the backend renderOcfStartEntry does:
 *   ocf:<provider>:<name> <instance> <k>=<v> ...
 * Keys are sorted and values are emitted verbatim (no quoting), matching the
 * Go generator. Returns "" when provider or name is empty.
 */
function renderOcfStartEntry(agent: {
  provider: string;
  name: string;
  instance: string;
  params: Record<string, string>;
}): string {
  const provider = agent.provider.trim();
  const name = agent.name.trim();
  if (!provider || !name) return '';
  let entry = `ocf:${provider}:${name}`;
  const inst = agent.instance.trim();
  if (inst) entry += ` ${inst}`;
  for (const key of Object.keys(agent.params).sort()) {
    entry += ` ${key}=${agent.params[key]}`;
  }
  return entry;
}

/**
 * Build the drbd-reactor promoter TOML that MakeHa would generate for the given
 * HA form state, reproducing the backend generatePromoterConfig output byte for
 * byte: the header comment, the `[[promoter]]` / `[promoter.resources.<name>]`
 * blocks, the ordered `start = [ ... ]` list (mount unit, then VIP service-ip
 * unit, then each systemd service, then each OCF agent) and the trailing
 * promoter options. Note the backend's own indentation quirk: the mount and VIP
 * entries are flush while services and OCF agents are indented two spaces — that
 * is preserved here so the preview equals the real file. Returns a placeholder
 * comment when no resource is selected yet.
 */
/** A resolved start[] entry from the ordered HA editor: either a systemd/mount
 * unit string, or an OCF agent. */
export type PreviewStartItem =
  | { kind: 'unit'; unit: string }
  | {
      kind: 'ocf';
      agent: { provider: string; name: string; instance: string; params: Record<string, string> };
    };

/**
 * mountUnitName / vipServiceIpInstance are re-exported as helpers so the HA
 * editor can turn a mount path or VIP CIDR into the exact unit string the
 * backend expects.
 */
export function mountUnitFor(mountPoint: string): string {
  return `${mountUnitName(mountPoint)}.mount`;
}
export function vipUnitFor(vip: string): string {
  const inst = vipServiceIpInstance(vip);
  return inst ? `service-ip@${inst}.service` : '';
}

/**
 * The unit drbd-reactor runs an OCF agent in: ocf.rs@<instance>_<resource>,
 * escaped the way systemd escapes an instance name (reactorEscape in the
 * controller).
 */
export function ocfUnitFor(instance: string, resource: string): string {
  const name = `${instance}_${resource}`;
  let out = '';
  for (let i = 0; i < name.length; i++) {
    const c = name[i];
    if (c === '/') out += '-';
    else if (/[:_0-9a-zA-Z]/.test(c) || (c === '.' && i > 0)) out += c;
    else out += `\\x${c.charCodeAt(0).toString(16).padStart(2, '0')}`;
  }
  return `ocf.rs@${out}.service`;
}

/**
 * Build the promoter TOML from an ORDERED start list where systemd/mount units
 * and OCF agents are peers — byte-identical to the backend
 * generatePromoterConfigOrdered (every entry indented two spaces, emitted in
 * the given order). Returns a placeholder until a resource is chosen.
 */
export function buildPromoterTomlPreviewOrdered(resource: string, items: PreviewStartItem[]): string {
  const res = resource.trim();
  if (!res) return '# select a resource to preview';

  const startActions: string[] = [];
  for (const it of items) {
    const entry = it.kind === 'ocf' ? renderOcfStartEntry(it.agent) : it.unit.trim();
    if (!entry) continue;
    startActions.push(`  "${entry}"`);
  }

  return `# drbd-reactor promoter configuration for HA resource: ${res}
# Generated by haify-controller

[[promoter]]
[promoter.resources.${res}]
runner = "systemd"
start = [
${startActions.join(',\n')}
]
on-drbd-demote-failure = "reboot"

`;
}

export function buildPromoterTomlPreview(input: {
  resource: string;
  vip: string;
  mountPoint: string;
  services: string[];
  ocfAgents: {
    provider: string;
    name: string;
    instance: string;
    params: Record<string, string>;
  }[];
}): string {
  const resource = input.resource.trim();
  if (!resource) return '# select a resource to preview';

  const startActions: string[] = [];

  // Mount unit (flush, no indent — matches the backend).
  if (input.mountPoint !== '') {
    startActions.push(`"${mountUnitName(input.mountPoint)}.mount"`);
  }

  // VIP service-ip unit (flush, no indent — matches the backend).
  const inst = vipServiceIpInstance(input.vip);
  if (inst) {
    startActions.push(`"service-ip@${inst}.service"`);
  }

  // Systemd services (indented two spaces — matches the backend).
  for (const svc of input.services) {
    startActions.push(`  "${svc}"`);
  }

  // Extra OCF agents, in order, after the built-in items (indented two spaces).
  for (const agent of input.ocfAgents) {
    const entry = renderOcfStartEntry(agent);
    if (!entry) continue;
    startActions.push(`  "${entry}"`);
  }

  return `# drbd-reactor promoter configuration for HA resource: ${resource}
# Generated by haify-controller

[[promoter]]
[promoter.resources.${resource}]
runner = "systemd"
start = [
${startActions.join(',\n')}
]
on-drbd-demote-failure = "reboot"

`;
}
