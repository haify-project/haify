import { getApiToken } from '@/services/api';

// aiClient — talks to the `oss-agent serve` NDJSON stream that backs the Haify AI
// Copilot. The agent runs as a separate process (see ai/README.md); this client
// only speaks its wire protocol over HTTP.
//
// Wire format (oss-agent /api/chat/stream, one JSON object per line — NDJSON):
//   {"t":"tool","name":..,"args":{..}}   agent invoked a tool
//   {"t":"tool_result","name":..}         that tool returned
//   {"t":"text","d":".."}                 answer delta
//   {"t":"suggestion", ...}               a guarded action proposal (v0.4.0)
//   {"t":"approval","id":..,"name":..,"args":{..}}  a write call held until
//                                         decideApproval answers it
//   {"t":"outcome","status":"complete"|"blocked","d":".."}  how the turn ended
//   {"t":"error","d":".."} | {"t":"done"}
//
// The `suggestion` frame shape is normalized in parseSuggestion() below and will
// be aligned to the oss-agent v0.4.0 EventSuggestion payload once it is tagged.

// Same-origin: the controller's UI server proxies /ai to the Copilot backend,
// so this works behind a reverse proxy where :7634 is not published. Override
// with localStorage['haify.ai_base'] to point at a Copilot running elsewhere.
export function aiBase(): string {
  const override =
    typeof localStorage !== 'undefined' ? localStorage.getItem('haify.ai_base') : null;
  if (override) return override.replace(/\/$/, '');
  return '';
}

export type AISeverity = 'low' | 'medium' | 'high';

export interface AIVerdict {
  blocked: boolean;
  ruleId?: string;
  reason?: string;
  severity?: string;
}

export interface AISuggestion {
  action: string; // e.g. "ha.evict" — maps to a controller REST operation
  params: Record<string, unknown>;
  reason: string;
  severity: AISeverity;
  verdict?: AIVerdict; // red-line wall result attached by the agent
}

/** A write tool call the agent is holding until the operator decides. */
export interface AIApproval {
  id: string;
  server: string;
  name: string;
  args: Record<string, unknown>;
}

/** How a turn ended. text is empty when the answer already says it. */
export interface AIOutcome {
  status: 'complete' | 'blocked';
  text: string;
}

export type AIEvent =
  | { type: 'text'; delta: string }
  | { type: 'reset' } // discard answer text streamed so far (a preamble)
  | { type: 'tool'; name: string; args?: unknown }
  | { type: 'tool_result'; name: string }
  | { type: 'suggestion'; suggestion: AISuggestion }
  | { type: 'approval'; approval: AIApproval }
  | { type: 'outcome'; outcome: AIOutcome }
  | { type: 'error'; message: string }
  | { type: 'done' };

// Raw NDJSON frame as emitted by oss-agent serve.
interface RawFrame {
  t: string;
  id?: string;
  status?: string;
  server?: string;
  name?: string;
  args?: unknown;
  d?: string;
  // suggestion fields (v0.4.0 — kept permissive until the tag lands)
  action?: string;
  params?: Record<string, unknown>;
  reason?: string;
  severity?: string;
  verdict?: AIVerdict;
  suggestion?: Partial<AISuggestion>;
}

function normSeverity(s?: string): AISeverity {
  return s === 'low' || s === 'high' ? s : 'medium';
}

// parseSuggestion tolerates either a flat frame ({t:"suggestion",action,...}) or
// a nested one ({t:"suggestion",suggestion:{...}}) so it survives the exact
// v0.4.0 shape either way.
function parseSuggestion(f: RawFrame): AISuggestion | null {
  const src = f.suggestion ?? f;
  const action = typeof src.action === 'string' ? src.action : '';
  if (!action) return null;
  return {
    action,
    params: (src.params as Record<string, unknown>) ?? {},
    reason: typeof src.reason === 'string' ? src.reason : '',
    severity: normSeverity(src.severity),
    verdict: src.verdict,
  };
}

function frameToEvent(f: RawFrame): AIEvent | null {
  switch (f.t) {
    case 'text':
      return f.d ? { type: 'text', delta: f.d } : null;
    case 'reset':
      return { type: 'reset' };
    case 'tool':
      return { type: 'tool', name: f.name ?? '', args: f.args };
    case 'tool_result':
      return { type: 'tool_result', name: f.name ?? '' };
    case 'suggestion': {
      const s = parseSuggestion(f);
      return s ? { type: 'suggestion', suggestion: s } : null;
    }
    case 'approval':
      return f.id
        ? {
            type: 'approval',
            approval: {
              id: f.id,
              server: f.server ?? '',
              name: f.name ?? '',
              args: (f.args as Record<string, unknown>) ?? {},
            },
          }
        : null;
    case 'outcome':
      return {
        type: 'outcome',
        outcome: { status: f.status === 'blocked' ? 'blocked' : 'complete', text: f.d ?? '' },
      };
    case 'error':
      return { type: 'error', message: f.d ?? 'stream error' };
    case 'done':
      return { type: 'done' };
    default:
      return null;
  }
}

export interface StreamChatResult {
  sessionId: string;
}

// streamChat POSTs one user message and invokes onEvent for every parsed frame.
// Resolves when the stream ends (or the abort signal fires). Rejects on transport
// errors so the caller can surface a connection banner.
export async function streamChat(
  message: string,
  sessionId: string | undefined,
  onEvent: (e: AIEvent) => void,
  signal?: AbortSignal,
  page?: string,
): Promise<StreamChatResult> {
  // The same token the REST calls use. haify-ai requires it whenever one is
  // configured; without this the Copilot was the one part of the UI that
  // reached an unauthenticated endpoint, and its knowledge-base routes are
  // writable.
  const token = getApiToken();
  const res = await fetch(`${aiBase()}/ai/chat/stream`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: JSON.stringify({ message, session_id: sessionId ?? '', page: page ?? '' }),
    signal,
  });
  if (!res.ok || !res.body) {
    const detail = await res.text().catch(() => '');
    throw new Error(`AI backend ${res.status}: ${detail || res.statusText}`);
  }
  const sid = res.headers.get('X-Session-Id') ?? sessionId ?? '';

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let nl: number;
    while ((nl = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, nl).trim();
      buf = buf.slice(nl + 1);
      if (!line) continue;
      let frame: RawFrame;
      try {
        frame = JSON.parse(line) as RawFrame;
      } catch {
        continue; // skip partial/garbage lines
      }
      const ev = frameToEvent(frame);
      if (ev) onEvent(ev);
    }
  }
  // flush any trailing frame without a newline
  const tail = buf.trim();
  if (tail) {
    try {
      const ev = frameToEvent(JSON.parse(tail) as RawFrame);
      if (ev) onEvent(ev);
    } catch {
      /* ignore */
    }
  }
  return { sessionId: sid };
}

/** The call is no longer waiting: decided already, or its conversation ended. */
export class ApprovalGoneError extends Error {}

// decideApproval answers a held write call. Approving runs it with exactly the
// arguments in its approval frame; the agent then continues from the result.
export async function decideApproval(id: string, approve: boolean, reason?: string): Promise<void> {
  const token = getApiToken();
  const res = await fetch(`${aiBase()}/ai/chat/approve`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: JSON.stringify({ id, approve, reason: reason ?? '' }),
  });
  if (res.status === 404) throw new ApprovalGoneError('no longer waiting for a decision');
  if (!res.ok) {
    throw new Error(`${res.status}: ${(await res.text().catch(() => '')) || res.statusText}`);
  }
}

// ── Copilot settings ────────────────────────────────────────────────────
//
// Which model answers, read and written while the Copilot runs. It used to take
// an edit to the unit's environment file and a restart — and that restart is a
// failover, because haify-ai is in the promoter's start list for haify-meta.

export interface AIConfig {
  llmBaseUrl: string;
  llmModel: string;
  /** A key is configured; a blank key field leaves it in place. */
  hasApiKey: boolean;
  /** Reported, never set: an index can only be queried by the embedder that built it. */
  embModel: string;
  embDim: number;
  /** False when this backend refuses writes; then readOnlyReason says why. */
  editable: boolean;
  readOnlyReason?: string;
}

export async function getAIConfig(): Promise<AIConfig> {
  const token = getApiToken();
  const res = await fetch(`${aiBase()}/ai/config`, {
    headers: { ...(token ? { Authorization: `Bearer ${token}` } : {}) },
  });
  if (!res.ok) {
    throw new Error(`${res.status}: ${(await res.text().catch(() => '')) || res.statusText}`);
  }
  return res.json();
}

/** Omit or blank a field to keep what is running — notably the API key, so
 *  renaming a model never means pasting the credential again. */
export async function saveAIConfig(patch: {
  llmBaseUrl?: string;
  llmModel?: string;
  llmApiKey?: string;
}): Promise<AIConfig> {
  const token = getApiToken();
  const res = await fetch(`${aiBase()}/ai/config`, {
    method: 'PUT',
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: JSON.stringify(patch),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(text || `${res.status} ${res.statusText}`);
  return JSON.parse(text);
}
