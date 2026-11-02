// The Haify install wizard. Every answer it shows comes from the installer
// script on the server: --probe-json for the nodes and disks, --dry-run for
// the review, and the install's own output for the progress.
'use strict';

const ENVS = [
  ['libvirt', 'KVM / libvirt', 'Guest disks on DRBD; live migration and VM failover through a libvirt hook.'],
  ['proxmox', 'Proxmox VE', 'Storage plugin with a panel in the PVE web UI.'],
  ['xcp-ng', 'XCP-ng', 'Storage driver and a shared SR on every host of the pool.'],
  ['kubernetes', 'Kubernetes', 'CSI driver through Helm. Also k3s, RKE2, MicroShift, OpenShift.'],
  ['openstack', 'OpenStack', 'Cinder driver and Horizon panel.'],
  ['opennebula', 'OpenNebula', 'Datastore and transfer driver.'],
  ['nomad', 'Nomad', 'CSI plugin jobs.'],
  ['docker', 'Docker / Podman', 'Volume plugin on every host.'],
  ['plain', 'No platform', 'Replicated block devices, NFS / iSCSI / NVMe-oF / SMB gateways, HA services.'],
];
// The order a node that runs several is classified in, as the script does.
const PRECEDENCE = ['proxmox', 'xcp-ng', 'harvester', 'kubernetes', 'openstack', 'opennebula', 'nomad', 'libvirt', 'docker'];
const STEPS = [['nodes', 'Nodes'], ['platform', 'Platform'], ['storage', 'Storage'], ['controller', 'Controller'], ['review', 'Review'], ['install', 'Install']];
const INSTALL_STEPS = ['Nodes and SSH', 'DRBD and tools', 'Controller access', 'Controller', 'Nodes and pool', 'Self-HA', ''];
const PVE_STEPS = ['Cluster and SSH', 'DRBD and tools', 'Controller access', 'Controller', 'Nodes and pool', 'Self-HA', 'Storage plugin', 'storage.cfg', 'Quorum'];

const S = {
  screen: 'nodes', probe: null, probing: false, error: '', extra: '',
  env: '', storage: {}, devices: {}, pool: 'vg0', poolType: 'lvm-thin', replicas: 2, selfHA: true, vip: '',
  plan: null, planning: false, job: null, from: 0, lines: [], logNode: '', follow: true,
};

const $ = (sel) => document.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);
const envTitle = (e) => (ENVS.find((x) => x[0] === e) || [e, e])[1];
const reachable = () => (S.probe?.nodes || []).filter((n) => n.ssh);
const storageNodes = () => reachable().filter((n) => S.storage[n.name] !== false);

async function api(path, body) {
  const r = await fetch(path, body ? { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) } : {});
  const data = await r.json().catch(() => ({ error: `${r.status} ${r.statusText}` }));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

function choices() {
  const nodes = {};
  for (const n of reachable()) nodes[n.name] = n.ip;
  const devices = {};
  for (const n of storageNodes()) if (!n.poolExists) devices[n.name] = S.devices[n.name] || [];
  return {
    env: S.env, nodes, storageNodes: storageNodes().map((n) => n.name), devices,
    pool: S.pool, poolType: S.poolType, replicas: S.replicas, selfHA: S.selfHA && storageNodes().length >= 2, vip: S.vip,
  };
}

// ---------------------------------------------------------------- probe

async function probe(extra) {
  S.probing = true; S.error = '';
  render();
  const known = (S.probe?.nodes || []).map((n) => (n.ssh ? `${n.name}=${n.ip}` : n.ip));
  const list = [...known, ...String(extra || '').split(/[\s,]+/).filter(Boolean)];
  try {
    S.probe = await api('/api/probe' + (list.length ? '?nodes=' + encodeURIComponent(list.join(',')) : ''));
    S.extra = '';
    if (!S.env) S.env = detectedEnv();
    for (const n of S.probe.nodes) {
      if (!S.devices[n.name]) {
        const blank = (n.disks || []).filter((d) => d.state === 'clean');
        S.devices[n.name] = blank.length === 1 ? [blank[0].path] : [];
      }
    }
    S.replicas = Math.min(2, Math.max(1, storageNodes().length));
  } catch (e) {
    S.error = e.message;
  }
  S.probing = false;
  render();
}

function detectedEnv() {
  const found = new Set();
  for (const n of reachable()) found.add(PRECEDENCE.find((e) => (n.envs || []).includes(e)) || 'plain');
  if (S.probe?.localEnv === 'proxmox') return 'proxmox';
  return found.size === 1 ? [...found][0] : '';
}

// ---------------------------------------------------------------- screens

const SCREENS = {
  nodes() {
    const nodes = S.probe?.nodes || [];
    const bad = nodes.filter((n) => !n.ssh);
    const rows = nodes.map((n) => `<tr>
      <td><b>${esc(n.name)}</b>${n.name === S.probe.local ? ' <span class="hint">this machine</span>' : ''}</td>
      <td class="mono">${esc(n.ip)}</td>
      <td>${n.ssh ? esc(`${n.os} · ${n.arch}`) : '—'}</td>
      <td>${n.ssh ? esc((n.envs || []).map(envTitle).join(', ') || 'none') : '—'}</td>
      <td>${n.ssh ? '<span class="dot" style="background:var(--ok)"></span>ok'
        : `<span class="dot" style="background:var(--bad)"></span>no root SSH: <span class="mono">ssh-copy-id root@${esc(n.ip)}</span>`}</td>
      <td><button class="row-x" data-drop="${esc(n.ip)}" aria-label="Remove ${esc(n.name)}">×</button></td></tr>`).join('');
    return `<h1>Nodes</h1>
      <p class="sub">Every machine that gets Haify. A node's name must be the name your platform uses for it.</p>
      <div class="card" style="margin-top:22px"><table>
        <tr><th>Node</th><th>Address</th><th>System</th><th>Platform found</th><th>SSH</th><th></th></tr>
        ${S.probing && !nodes.length ? '<tr><td colspan="6"><span class="spin"></span>Looking at the nodes…</td></tr>' : rows}
        <tr><td colspan="3"><input class="input" id="extra" placeholder="Add nodes: address or name=address, comma-separated" value="${esc(S.extra)}"></td>
          <td colspan="3"><button class="btn" id="scan" ${S.probing ? 'disabled' : ''}>${S.probing ? '<span class="spin"></span>Checking' : 'Add and check'}</button></td></tr>
      </table></div>
      ${bad.length ? `<p class="hint">Root SSH from ${esc(S.probe.local || 'this machine')} must work without a password. Run the command shown on this machine, then check again.</p>` : ''}
      ${S.error ? `<div class="err">${esc(S.error)}</div>` : ''}
      <div class="bar"><span></span><button class="btn dark" data-go="platform" ${!reachable().length || bad.length || S.probing ? 'disabled' : ''}>Continue</button></div>`;
  },

  platform() {
    const counts = {};
    for (const n of reachable()) for (const e of n.envs || []) counts[e] = (counts[e] || 0) + 1;
    const cards = ENVS.map(([e, title, text]) => {
      const off = e === 'proxmox' && S.probe.localEnv !== 'proxmox';
      return `<button class="pf ${S.env === e ? 'sel' : ''}" data-env="${e}" ${off ? 'disabled' : ''}>
        ${counts[e] ? `<span class="found">${counts[e]} node${counts[e] > 1 ? 's' : ''}</span>` : ''}
        <b>${esc(title)}</b><p>${esc(off ? 'Run the installer on a PVE node to install there.' : text)}</p></button>`;
    }).join('');
    const found = Object.keys(counts).filter((e) => e !== 'harvester').map(envTitle);
    return `<h1>Platform</h1>
      <p class="sub">What Haify provides storage to.${found.length ? ` Found on your nodes: ${esc(found.join(', '))}.` : ''}</p>
      ${counts.harvester ? '<div class="err">Harvester nodes are immutable: follow docs/harvester.md instead.</div>' : ''}
      <div class="grid">${cards}</div>
      <div class="bar"><button class="btn" data-go="nodes">Back</button><button class="btn dark" data-go="storage" ${S.env ? '' : 'disabled'}>Continue</button></div>`;
  },

  storage() {
    const cards = reachable().map((n) => {
      const holds = S.storage[n.name] !== false;
      const disks = (n.disks || []).map((d) => {
        const ok = d.state === 'clean';
        const sel = (S.devices[n.name] || []).includes(d.path);
        const why = ok ? 'blank' : d.state.replace(/^(in-use|wipeable): /, '');
        return `<div class="disk ${sel ? 'sel' : ''} ${ok && holds && !n.poolExists ? '' : 'no'}" ${ok && holds && !n.poolExists ? `data-disk="${esc(n.name)}|${esc(d.path)}"` : ''}>
          <span class="chk">${sel ? '<svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="3.5"><path d="M5 12l5 5 9-10"/></svg>' : ''}</span>
          <span class="mono">${esc(d.path)}</span><span class="z">${esc(d.size)} · ${esc(why)}</span></div>`;
      }).join('');
      return `<div class="card nd"><h3>${esc(n.name)} <label class="toggle ${holds ? 'on' : ''}" data-holds="${esc(n.name)}"><i></i></label></h3>
        ${n.poolExists ? `<p class="hint">Pool ${esc(S.pool)} is already here.</p>` : holds ? disks || '<p class="hint">No disks found.</p>' : '<p class="hint">Uses volumes over the network, holds none.</p>'}</div>`;
    }).join('');
    const max = Math.min(3, storageNodes().length);
    const missing = storageNodes().filter((n) => !n.poolExists && !(S.devices[n.name] || []).length);
    return `<h1>Storage</h1>
      <p class="sub">The disks that hold the pool. Only blank disks can be chosen; nothing on a disk in use is touched.</p>
      <div class="nodes3">${cards}</div>
      <div class="opts">
        <div class="card opt"><label for="pool">Pool name</label><input class="input mono" id="pool" value="${esc(S.pool)}"></div>
        <div class="card opt"><label>Pool type</label><div class="seg">
          <span class="${S.poolType === 'lvm-thin' ? 'on' : ''}" data-ptype="lvm-thin">Thin (snapshots)</span><span class="${S.poolType === 'lvm' ? 'on' : ''}" data-ptype="lvm">Thick</span></div></div>
        <div class="card opt"><label>Copies of each volume</label><div class="seg">
          ${[1, 2, 3].filter((k) => k <= max).map((k) => `<span class="${S.replicas === k ? 'on' : ''}" data-rep="${k}">${k}</span>`).join('')}</div></div>
      </div>
      ${missing.length ? `<p class="hint">Choose a disk on ${esc(missing.map((n) => n.name).join(', '))}, or turn it off as a storage node.</p>` : ''}
      <div class="bar"><button class="btn" data-go="platform">Back</button><button class="btn dark" data-go="controller" ${missing.length || !storageNodes().length ? 'disabled' : ''}>Continue</button></div>`;
  },

  controller() {
    const many = storageNodes().length >= 2;
    const first = reachable()[0]?.ip || '192.168.1.10';
    const example = first.replace(/\.\d+$/, '.250') + '/24';
    const vipOK = !S.selfHA || !many || /^[0-9a-fA-F.:]+\/\d+$/.test(S.vip);
    return `<h1>Controller</h1>
      <p class="sub">The controller runs on one storage node at a time${many ? '; with Self-HA another takes over when that node fails' : ''}.</p>
      <div class="card opt" style="margin-top:22px;max-width:560px">
        ${many ? `<label class="toggle ${S.selfHA ? 'on' : ''}" data-selfha><i></i>Self-HA on ${esc(storageNodes().map((n) => n.name).join(', '))}</label>
          ${S.selfHA ? `<label for="vip" style="margin-top:16px">Floating address, in CIDR form</label>
            <input class="input mono" id="vip" placeholder="${esc(example)}" value="${esc(S.vip)}">
            <p class="hint">A free address in the nodes' subnet. The web UI and every client use it.</p>`
            : '<p class="hint">The controller stays on one node: if it fails, volumes keep working but nothing can be created or moved until it is back.</p>'}`
          : `<p>The controller runs on ${esc(storageNodes()[0]?.name || '')}. Self-HA needs two storage nodes.</p>`}
      </div>
      <div class="bar"><button class="btn" data-go="storage">Back</button><button class="btn dark" data-go="review" ${vipOK ? '' : 'disabled'}>Continue</button></div>`;
  },

  review() {
    if (!S.plan && !S.planning) makePlan();
    if (S.planning) return `<h1>Review</h1><p class="sub"><span class="spin"></span>Checking the plan against the nodes…</p>`;
    const out = S.plan?.output || '';
    const lines = out.split('\n');
    const planAt = lines.indexOf('Plan:');
    const plan = planAt >= 0 ? lines.slice(planAt + 1, lines.findIndex((l, i) => i > planAt && !l.trim())) : [];
    const cmds = lines.filter((l) => l.startsWith('[dry-run]')).map((l) => l.replace(/^\[dry-run\]\s+/, ''));
    const errs = lines.filter((l) => l.startsWith('ERROR'));
    return `<h1>Review</h1>
      <p class="sub">Checked against the nodes as they are now. ${cmds.length ? 'Install makes these changes.' : 'The nodes already match this plan; Install only checks them again.'}</p>
      ${S.plan?.ok ? '' : `<div class="err">${esc(errs.join('\n') || out.split('\n').slice(-6).join('\n'))}</div>`}
      ${plan.length ? `<div class="card plan">${plan.map((l) => {
        const m = l.match(/^\s+([^:]+):\s+(.*)$/);
        return m ? `<div class="pr"><span>${esc(m[1])}</span><span>${esc(m[2])}</span></div>` : `<div class="pr"><span></span><span class="mono">${esc(l.trim())}</span></div>`;
      }).join('')}</div>` : ''}
      ${cmds.length ? `<div class="card cmds"><h3>Commands per node <span class="hint">${cmds.length} commands</span></h3>
        <pre>${cmds.map((c) => { const [n, ...r] = c.split(/\s+/); return `<b>${esc(n)}</b>  ${esc(r.join(' '))}`; }).join('\n')}</pre></div>` : ''}
      <div class="bar"><button class="btn" data-go="controller">Back</button><button class="btn dark" id="install" ${S.plan?.ok ? '' : 'disabled'}>Install</button></div>`;
  },

  install() {
    const j = S.job || { state: 'running', step: 0 };
    const titles = (j.env || S.env) === 'proxmox' ? PVE_STEPS : INSTALL_STEPS.map((t, i) => (i === 6 ? envTitle(j.env || S.env) : t));
    const steps = titles.map((t, i) => {
      const n = i + 1;
      const cls = j.failedStep === n ? 'f' : n < j.step || (j.state === 'done' && n <= j.step) ? 'd' : n === j.step && j.state === 'running' ? 'r' : 'p';
      const icon = cls === 'd' ? '<svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="3.5"><path d="M5 12l5 5 9-10"/></svg>' : '';
      return `<div class="ps ${cls}"><span class="i">${icon}</span><span class="t">${esc(t)}</span></div>`;
    }).join('');
    const pct = j.state === 'done' ? 100 : Math.round((Math.max(j.step - 1, 0) / titles.length) * 100);
    // A node's lines start with [node], after the clock on the installer's own.
    const nodes = [...new Set(S.lines.map((l) => (l.match(/^(?:\[[\d:]+\] )?\[([\w.-]+)\]/) || [])[1]).filter((n) => n && n !== 'dry-run'))];
    const shown = S.logNode ? S.lines.filter((l) => l.includes(`[${S.logNode}]`)) : S.lines;
    const ui = (S.lines.find((l) => l.includes('web UI:')) || '').replace(/.*web UI:\s*/, '').trim();
    return `<h1>${j.state === 'done' ? 'Installed' : j.state === 'failed' ? 'Install stopped' : 'Installing'}</h1>
      <p class="sub">${j.state === 'running' ? 'You can close this page; the install keeps running and the page picks up where it is.' : j.state === 'failed' ? 'Fix the cause in the log, then go back to Review and install again: steps that are done are skipped.' : 'Haify is running on every node.'}</p>
      ${j.state === 'done' ? `<div class="card finished"><span>${ui ? `Web UI at <span class="mono">${esc(ui)}</span>` : 'Done.'}</span>${ui ? `<a class="btn dark" href="${esc(ui)}" target="_blank" rel="noopener">Open Haify</a>` : ''}</div>` : ''}
      ${j.state === 'failed' ? `<div class="bar" style="margin-top:12px"><span></span><button class="btn" data-go="review" data-replan>Back to Review</button></div>` : ''}
      <div class="ins"><div class="card prog"><div class="progressbar"><i style="width:${pct}%"></i></div>
        <div class="note">${j.state === 'running' ? `${Math.floor((j.elapsed || 0) / 60)} min so far` : ''}</div>${steps}</div>
      <div class="card log"><div class="hd"><span class="${S.logNode ? '' : 'on'}" data-log="">All nodes</span>${nodes.map((n) => `<span class="${S.logNode === n ? 'on' : ''}" data-log="${esc(n)}">${esc(n)}</span>`).join('')}</div>
        <pre id="log">${esc(shown.slice(-400).join('\n'))}</pre></div></div>`;
  },
};

// ---------------------------------------------------------------- actions

async function makePlan() {
  S.planning = true;
  try {
    S.plan = await api('/api/plan', choices());
  } catch (e) {
    S.plan = { ok: false, output: 'ERROR: ' + e.message };
  }
  S.planning = false;
  render();
}

async function startInstall() {
  try {
    await api('/api/install', choices());
    S.lines = []; S.from = 0; S.job = { state: 'running', step: 1, env: S.env };
    go('install');
    poll();
  } catch (e) {
    S.plan = { ok: false, output: 'ERROR: ' + e.message };
    render();
  }
}

async function poll() {
  try {
    const j = await api('/api/job?from=' + S.from);
    S.lines.push(...j.lines);
    S.from = j.next;
    S.job = j;
    if (S.screen === 'install') render();
    if (j.state === 'running') setTimeout(poll, 1000);
  } catch {
    setTimeout(poll, 3000);
  }
}

function go(screen) {
  S.screen = screen;
  render();
  window.scrollTo(0, 0);
}

document.addEventListener('click', (ev) => {
  const t = ev.target.closest('[data-go],[data-env],[data-disk],[data-holds],[data-ptype],[data-rep],[data-selfha],[data-drop],[data-log],#scan,#install');
  if (!t) return;
  if (t.dataset.go) {
    if ('replan' in t.dataset || ['storage', 'controller', 'platform'].includes(S.screen)) S.plan = null;
    return go(t.dataset.go);
  }
  if (t.dataset.env) S.env = t.dataset.env;
  if (t.dataset.disk) {
    const [n, p] = t.dataset.disk.split('|');
    const cur = S.devices[n] || [];
    S.devices[n] = cur.includes(p) ? cur.filter((x) => x !== p) : [...cur, p];
  }
  if (t.dataset.holds) S.storage[t.dataset.holds] = S.storage[t.dataset.holds] === false;
  if (t.dataset.ptype) S.poolType = t.dataset.ptype;
  if (t.dataset.rep) S.replicas = Number(t.dataset.rep);
  if ('selfha' in t.dataset) S.selfHA = !S.selfHA;
  if ('log' in t.dataset) { S.logNode = t.dataset.log; S.follow = true; }
  if (t.dataset.drop) {
    S.probe.nodes = S.probe.nodes.filter((n) => n.ip !== t.dataset.drop);
  }
  if (t.id === 'scan') return probe($('#extra').value);
  if (t.id === 'install') return startInstall();
  S.replicas = Math.min(S.replicas, Math.max(1, storageNodes().length));
  render();
});

document.addEventListener('input', (ev) => {
  if (ev.target.id === 'pool') S.pool = ev.target.value.trim();
  if (ev.target.id === 'extra') S.extra = ev.target.value;
  if (ev.target.id === 'vip') {
    S.vip = ev.target.value.trim();
    const ok = /^[0-9a-fA-F.:]+\/\d+$/.test(S.vip);
    const b = document.querySelector('[data-go="review"]');
    if (b) b.disabled = !ok;
  }
});

// The log follows new lines until the reader scrolls up, and again once
// they scroll back to the end.
document.addEventListener('scroll', (ev) => {
  const l = ev.target;
  if (l.id === 'log') S.follow = l.scrollTop + l.clientHeight >= l.scrollHeight - 30;
}, true);

document.addEventListener('keydown', (ev) => {
  if (ev.key !== 'Enter') return;
  if (ev.target.id === 'extra' && !S.probing) probe(ev.target.value);
  if (ev.target.id === 'vip') document.querySelector('[data-go="review"]:not(:disabled)')?.click();
});

function render() {
  const idx = STEPS.findIndex((s) => s[0] === S.screen);
  const sums = !S.probe ? { platform: envTitle(S.env || S.job?.env) } : { nodes: `${reachable().length} found`, platform: envTitle(S.env), storage: `${storageNodes().length} node${storageNodes().length === 1 ? '' : 's'}`, controller: S.selfHA && storageNodes().length >= 2 ? 'Self-HA' : 'one node' };
  $('#steps').innerHTML = STEPS.map(([k, t], i) =>
    `<li class="${i < idx ? 'done' : i === idx ? 'on' : ''}"><span class="n">${i < idx ? '✓' : i + 1}</span>${t}${i < idx && sums[k] ? `<span class="s">${esc(sums[k])}</span>` : ''}</li>`).join('');
  const where = S.probe?.local ? `Running on ${S.probe.local}. ` : '';
  $('#foot').textContent = where + (S.screen === 'install' ? 'Log: /var/log/haify/install.log' : 'Nothing changes before Install.');
  const focus = document.activeElement?.id;
  $('#main').innerHTML = SCREENS[S.screen]();
  const log = $('#log');
  if (log && S.follow) log.scrollTop = log.scrollHeight;
  if (focus && $('#' + focus) && $('#' + focus).tagName === 'INPUT') {
    const el = $('#' + focus);
    el.focus();
    el.setSelectionRange(el.value.length, el.value.length);
  }
}

// An install already running (or finished) is shown instead of a new wizard.
(async () => {
  render();
  try {
    const j = await api('/api/job?from=0');
    if (j.state !== 'idle') {
      S.job = j; S.lines = j.lines; S.from = j.next; S.env = j.env;
      go('install');
      if (j.state === 'running') setTimeout(poll, 1000);
      return;
    }
  } catch { /* the probe below reports what is wrong */ }
  probe();
})();
