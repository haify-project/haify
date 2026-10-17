import { useState } from 'react';
import { toast } from 'sonner';
import { Sparkles, X, ShieldAlert, Check, Loader2, AlertTriangle } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import { api } from '@/services/api';
import {
  ApprovalGoneError,
  decideApproval,
  type AIApproval,
  type AIOutcome,
  type AISuggestion,
} from '@/lib/aiClient';

// The cards a Copilot answer can carry besides its text: a proposed action,
// a write call waiting for approval, and the turn's outcome. Kept apart from
// the panel, which only lays out the conversation.

// executeSuggestion maps a guarded action proposal onto the existing controller
// REST the UI already uses. Only a small, explicit allowlist is executable from
// the copilot (O3); everything else is surfaced as manual. Writes still go
// through the same endpoints as the normal UI buttons.
async function executeSuggestion(s: AISuggestion): Promise<string> {
  const p = s.params as Record<string, string | undefined>;
  switch (s.action) {
    case 'ha.evict':
    case 'ha.failover': {
      // Evicting the current primary IS a failover (demote here, promote elsewhere).
      if (!p.resource) throw new Error('missing resource');
      return (await api.evictHa(p.resource)).message;
    }
    case 'ha.create': {
      if (!p.resource) throw new Error('missing resource');
      return (
        await api.makeHa(p.resource, {
          vip: p.vip,
          mountPoint: p.mountPoint,
          fstype: p.fstype,
          services: p.services ? String(p.services).split(',').map((x) => x.trim()) : undefined,
        })
      ).message;
    }
    case 'snapshot.create': {
      if (!p.volume || !p.snapshotName) throw new Error('missing volume/snapshotName');
      return (await api.createSnapshot(p.volume, p.snapshotName, p.node)).message;
    }
    default:
      throw new Error(`"${s.action}" is not auto-executable — do it manually`);
  }
}

function severityClass(sev: string): string {
  switch (sev) {
    case 'high':
      return 'border-red-500/40 text-red-500';
    case 'low':
      return 'border-muted-foreground/30 text-muted-foreground';
    default:
      return 'border-amber-500/40 text-amber-500';
  }
}

function severityBorder(sev: string): string {
  switch (sev) {
    case 'high':
      return 'border-l-red-500';
    case 'low':
      return 'border-l-muted-foreground/40';
    default:
      return 'border-l-amber-500';
  }
}

export function SuggestionCard({ suggestion }: { suggestion: AISuggestion }) {
  const [state, setState] = useState<'idle' | 'running' | 'done' | 'failed' | 'dismissed'>('idle');
  const [result, setResult] = useState('');
  const blocked = suggestion.verdict?.blocked ?? false;

  const approve = async () => {
    setState('running');
    try {
      const msg = await executeSuggestion(suggestion);
      setState('done');
      setResult(msg || 'done');
      toast.success(`${suggestion.action} executed`);
    } catch (e) {
      setState('failed');
      setResult(e instanceof Error ? e.message : String(e));
      toast.error(`${suggestion.action} failed`);
    }
  };

  return (
    <div className={cn('mt-2 rounded-xl border border-l-2 border-border bg-secondary/40 p-3.5 text-[12.5px]', severityBorder(suggestion.severity))}>
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5">
          <Sparkles className="h-3.5 w-3.5 text-primary" />
          <span className="text-[0.7rem] font-medium text-muted-foreground">Suggested action</span>
        </div>
        <Badge variant="outline" className={cn('text-[0.65rem]', severityClass(suggestion.severity))}>
          {suggestion.severity}
        </Badge>
      </div>

      <div className="mt-1.5 font-mono text-[12.5px] font-medium">{suggestion.action}</div>
      <ArgChips args={suggestion.params} />
      {suggestion.reason && <p className="mt-1.5 text-muted-foreground">{suggestion.reason}</p>}

      {blocked && (
        <div className="mt-2 flex items-start gap-1.5 rounded bg-red-500/10 p-1.5 text-red-500">
          <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />
          <span>
            Blocked by red-line
            {suggestion.verdict?.ruleId ? ` (${suggestion.verdict.ruleId})` : ''}
            {suggestion.verdict?.reason ? `: ${suggestion.verdict.reason}` : ''}
          </span>
        </div>
      )}

      {state === 'done' || state === 'failed' ? (
        <div
          className={cn(
            'mt-2 flex items-center gap-1.5',
            state === 'done' ? 'text-emerald-500' : 'text-red-500',
          )}
        >
          {state === 'done' ? <Check className="h-3.5 w-3.5" /> : <X className="h-3.5 w-3.5" />}
          <span className="break-all">{result}</span>
        </div>
      ) : state === 'dismissed' ? (
        <div className="mt-2 flex items-center gap-1.5 text-muted-foreground">
          <X className="h-3.5 w-3.5" />
          <span>Dismissed</span>
        </div>
      ) : (
        <div className="mt-2 flex gap-2">
          <Button
            size="sm"
            className="h-[30px] px-3 text-[12.5px]"
            disabled={blocked || state === 'running'}
            onClick={approve}
          >
            {state === 'running' ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : 'Approve'}
          </Button>
          <Button
            size="sm"
            variant="outline"
            className="h-[30px] px-3 text-[12.5px]"
            disabled={state === 'running'}
            onClick={() => setState('dismissed')}
          >
            Dismiss
          </Button>
        </div>
      )}
    </div>
  );
}

function ArgChips({ args }: { args: Record<string, unknown> }) {
  if (Object.keys(args).length === 0) return null;
  return (
    <div className="mt-1.5 flex flex-wrap gap-1">
      {Object.entries(args).map(([k, v]) => (
        <span key={k} className="rounded-md bg-card px-1.5 py-0.5 font-mono text-[11.5px] break-all">
          {k}=<span className="text-foreground">{typeof v === 'object' ? JSON.stringify(v) : String(v)}</span>
        </span>
      ))}
    </div>
  );
}

// ApprovalCard answers a write call the agent is holding. Nothing has run yet:
// Approve runs exactly these arguments, Reject tells the agent it may not.
export function ApprovalCard({ approval }: { approval: AIApproval }) {
  const [state, setState] = useState<'waiting' | 'sending' | 'approved' | 'rejected' | 'gone'>('waiting');

  const decide = async (approve: boolean) => {
    setState('sending');
    try {
      await decideApproval(approval.id, approve);
      setState(approve ? 'approved' : 'rejected');
    } catch (e) {
      if (e instanceof ApprovalGoneError) {
        setState('gone');
        return;
      }
      setState('waiting');
      toast.error(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="mt-2 rounded-xl border border-border bg-secondary/40 p-3.5 text-[12.5px]">
      <div className="flex items-center gap-2 text-[12px] text-muted-foreground">
        <span className="size-[7px] rounded-full bg-status-warn" />
        Needs your approval
      </div>
      <div className="mt-1.5 font-mono text-[12.5px] font-medium break-all">{approval.name}</div>
      <ArgChips args={approval.args} />

      {state === 'approved' || state === 'rejected' || state === 'gone' ? (
        <div
          className={cn(
            'mt-2 flex items-center gap-1.5',
            state === 'approved' ? 'text-emerald-500' : 'text-muted-foreground',
          )}
        >
          {state === 'approved' ? <Check className="h-3.5 w-3.5" /> : <X className="h-3.5 w-3.5" />}
          <span>{state === 'approved' ? 'Approved' : state === 'rejected' ? 'Rejected' : 'No longer pending'}</span>
        </div>
      ) : (
        <div className="mt-2 flex gap-2">
          <Button
            size="sm"
            className="h-[30px] px-3 text-[12.5px]"
            disabled={state === 'sending'}
            onClick={() => decide(true)}
          >
            {state === 'sending' ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : 'Approve'}
          </Button>
          <Button
            size="sm"
            variant="outline"
            className="h-[30px] px-3 text-[12.5px]"
            disabled={state === 'sending'}
            onClick={() => decide(false)}
          >
            Reject
          </Button>
        </div>
      )}
    </div>
  );
}

export function OutcomeStrip({ outcome }: { outcome: AIOutcome }) {
  const blocked = outcome.status === 'blocked';
  return (
    <div
      className={cn(
        'flex items-start gap-1.5 rounded-md border p-2 text-xs',
        blocked
          ? 'border-amber-500/40 bg-amber-500/10 text-amber-600 dark:text-amber-400'
          : 'border-emerald-500/40 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
      )}
    >
      {blocked ? (
        <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      ) : (
        <Check className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      )}
      <span>
        <span className="font-medium">{blocked ? 'Blocked' : 'Done'}</span>
        {outcome.text && <span className="text-foreground/80"> — {outcome.text}</span>}
      </span>
    </div>
  );
}
