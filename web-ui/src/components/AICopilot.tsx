import { useEffect, useRef, useState } from 'react';
import { Streamdown } from 'streamdown';
import { Sparkles, ArrowRight, X, Check, Loader2, AlertTriangle } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { usePageContext } from '@/lib/pageContext';
import {
  streamChat,
  type AIApproval,
  type AIEvent,
  type AIOutcome,
  type AISuggestion,
} from '@/lib/aiClient';
import { ApprovalCard, OutcomeStrip, SuggestionCard } from './AICopilotCards';

// The panel docks beside the page only while the page keeps at least this much
// width; below it the panel opens over the page instead. Squeezed any further,
// the dashboard's figures stack one word per line.
const MIN_MAIN_WIDTH = 720;

// What the empty panel offers to ask. Questions an operator actually has, each
// one answerable from the cluster's own state.
const EXAMPLE_QUESTIONS = [
  'Is any replica degraded?',
  'Which pool is fullest?',
  'What failed over today?',
  'Why is a resource out of sync?',
];

interface ToolTrace {
  name: string;
  args?: unknown;
  done: boolean;
}

// argSummary renders a tool call's arguments compactly as "k=v  k=v".
function argSummary(args: unknown): string {
  if (!args || typeof args !== 'object') return '';
  return Object.entries(args as Record<string, unknown>)
    .map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : String(v)}`)
    .join('  ');
}

interface Msg {
  id: string;
  role: 'user' | 'assistant';
  text: string;
  tools: ToolTrace[];
  suggestions: AISuggestion[];
  approvals: AIApproval[];
  outcome?: AIOutcome;
  error?: string;
}

let msgSeq = 0;
const nextId = () => `m${++msgSeq}`;

// StreamingMarkdown reveals `text` progressively and renders it with Streamdown,
// which safely handles incomplete markdown (unterminated **, code fences, tables)
// as the prefix grows — so the answer "streams" visually even when the backend
// delivers it in one frame. State is keyed per message, so a completed message
// keeps its full text and doesn't re-animate on re-render.
function StreamingMarkdown({ text }: { text: string }) {
  const [shown, setShown] = useState(0);
  useEffect(() => {
    if (shown >= text.length) return;
    const id = setInterval(() => {
      setShown((s) => Math.min(text.length, s + Math.max(2, Math.ceil((text.length - s) / 12))));
    }, 24);
    return () => clearInterval(id);
  }, [text, shown]);
  return (
    <Streamdown className="copilot-md text-sm [&_pre]:text-xs [&_code]:text-xs">
      {text.slice(0, shown)}
    </Streamdown>
  );
}

export function AICopilot({
  open,
  onClose,
  reservedWidth,
}: {
  open: boolean;
  onClose: () => void;
  /** Width already taken beside the page — the sidebar — so the panel can
   *  tell whether docking would leave the page enough room. */
  reservedWidth: number;
}) {
  const page = usePageContext();
  const [messages, setMessages] = useState<Msg[]>([]);
  const [input, setInput] = useState('');
  const [busy, setBusy] = useState(false);
  const sessionRef = useRef<string | undefined>(
    typeof localStorage !== 'undefined'
      ? localStorage.getItem('haify.ai_session') ?? undefined
      : undefined,
  );
  const scrollRef = useRef<HTMLDivElement>(null);

  // Resizable split pane: drag the left edge to change the panel width.
  const [width, setWidth] = useState<number>(() => {
    const s = typeof localStorage !== 'undefined' ? localStorage.getItem('haify.ai_width') : null;
    const n = s ? Number(s) : NaN;
    return Number.isFinite(n) ? n : 400;
  });
  // On phones (< md) the Copilot is a full-screen overlay, so the fixed pixel
  // width (and the drag-to-resize handle) only apply from md up. From md up it
  // docks beside the page when the page keeps MIN_MAIN_WIDTH, and otherwise
  // opens over it at its own width.
  const [viewport, setViewport] = useState<number>(() =>
    typeof window !== 'undefined' ? window.innerWidth : 1280,
  );
  useEffect(() => {
    const onResize = () => setViewport(window.innerWidth);
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
  }, []);
  const isDesktop = viewport >= 768;
  const docked = isDesktop && viewport - reservedWidth - width >= MIN_MAIN_WIDTH;
  const dragging = useRef(false);
  useEffect(() => {
    const clamp = (w: number) => Math.min(Math.max(w, 320), Math.round(window.innerWidth * 0.7));
    const onMove = (e: MouseEvent) => {
      if (!dragging.current) return;
      setWidth(clamp(window.innerWidth - e.clientX));
    };
    const onUp = () => {
      if (!dragging.current) return;
      dragging.current = false;
      document.body.style.userSelect = '';
      document.body.style.cursor = '';
      setWidth((w) => {
        localStorage.setItem('haify.ai_width', String(w));
        return w;
      });
    };
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', onUp);
    return () => {
      window.removeEventListener('mousemove', onMove);
      window.removeEventListener('mouseup', onUp);
    };
  }, []);

  useEffect(() => {
    scrollRef.current?.scrollTo({ top: scrollRef.current.scrollHeight });
  }, [messages]);

  const patchLast = (fn: (m: Msg) => void) =>
    setMessages((prev) => {
      if (prev.length === 0) return prev;
      const copy = [...prev];
      const last = { ...copy[copy.length - 1] };
      fn(last);
      copy[copy.length - 1] = last;
      return copy;
    });

  // Over the page the panel is a dialog: Escape closes it, as the backdrop
  // does. Docked it is part of the layout and Escape belongs to the page.
  useEffect(() => {
    if (!open || docked) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !e.defaultPrevented) onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, docked, onClose]);

  // Opening the panel is asking to type into it. Not on a phone, where the
  // keyboard would cover the example questions before they were read.
  const inputRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (open && isDesktop) inputRef.current?.focus();
  }, [open, isDesktop]);

  const send = async (question?: string) => {
    const q = (question ?? input).trim();
    if (!q || busy) return;
    setInput('');
    setBusy(true);
    const userMsg: Msg = { id: nextId(), role: 'user', text: q, tools: [], suggestions: [], approvals: [] };
    const botMsg: Msg = { id: nextId(), role: 'assistant', text: '', tools: [], suggestions: [], approvals: [] };
    setMessages((prev) => [...prev, userMsg, botMsg]);

    const onEvent = (e: AIEvent) => {
      switch (e.type) {
        case 'text':
          patchLast((m) => {
            m.text += e.delta;
          });
          break;
        case 'reset':
          patchLast((m) => {
            m.text = '';
          });
          break;
        case 'tool':
          patchLast((m) => {
            m.tools = [...m.tools, { name: e.name, args: e.args, done: false }];
          });
          break;
        case 'tool_result':
          patchLast((m) => {
            // mark the last still-running call of this tool as done
            const tools = [...m.tools];
            for (let i = tools.length - 1; i >= 0; i--) {
              if (tools[i].name === e.name && !tools[i].done) {
                tools[i] = { ...tools[i], done: true };
                break;
              }
            }
            m.tools = tools;
          });
          break;
        case 'suggestion':
          patchLast((m) => {
            // Dedupe: the model (or agent-go's duplicate-call re-execution) can
            // emit the same proposal twice in one turn; show it once.
            const key = (s: AISuggestion) => `${s.action}|${JSON.stringify(s.params)}`;
            if (m.suggestions.some((s) => key(s) === key(e.suggestion))) return;
            m.suggestions = [...m.suggestions, e.suggestion];
          });
          break;
        case 'approval':
          patchLast((m) => {
            m.approvals = [...m.approvals, e.approval];
          });
          break;
        case 'outcome':
          patchLast((m) => {
            m.outcome = e.outcome;
          });
          break;
        case 'error':
          patchLast((m) => {
            m.error = e.message;
          });
          break;
        default:
          break;
      }
    };

    try {
      const { sessionId } = await streamChat(q, sessionRef.current, onEvent, undefined, page);
      if (sessionId) {
        sessionRef.current = sessionId;
        localStorage.setItem('haify.ai_session', sessionId);
      }
    } catch (e) {
      patchLast((m) => {
        m.error = e instanceof Error ? e.message : 'connection failed';
      });
    } finally {
      setBusy(false);
    }
  };

  if (!open) return null;

  return (
    <>
      {!docked && isDesktop && (
        <button
          type="button"
          aria-label="Close Copilot"
          tabIndex={-1}
          onClick={onClose}
          className="fixed inset-0 z-40 bg-black/30"
        />
      )}
      <aside
        role={docked ? 'complementary' : 'dialog'}
        aria-modal={docked ? undefined : true}
        aria-label="Haify Copilot"
        // Never wider than the window, whatever width was dragged to before.
        style={isDesktop ? { width: Math.min(width, viewport) } : undefined}
        className={cn(
          'flex shrink-0 flex-col border-l border-border bg-card',
          docked
            ? 'relative'
            : // Over the page: full screen on a phone, a sheet from the right
              // edge above that.
              'fixed inset-y-0 right-0 z-40 w-full shadow-2xl md:w-auto',
        )}
      >
        {/* Drag handle to resize the panel — desktop only. */}
        <div
          onMouseDown={() => {
            dragging.current = true;
            document.body.style.userSelect = 'none';
            document.body.style.cursor = 'col-resize';
          }}
          title="Drag to resize"
          className="absolute -left-1 top-0 z-10 hidden h-full w-2 cursor-col-resize hover:bg-primary/30 md:block"
        />
        <header className="flex h-14 items-center justify-between border-b border-border px-4">
          <div className="flex items-center gap-2.5">
            <span className="flex size-[26px] items-center justify-center rounded-lg bg-gradient-to-br from-[var(--tile-mint)] to-[var(--tile-sky)]">
              <Sparkles className="size-3.5 text-foreground" />
            </span>
            <span className="text-[14px] font-semibold tracking-tight">Copilot</span>
          </div>
          <Button
            variant="ghost"
            size="icon"
            className="h-7 w-7"
            onClick={onClose}
            aria-label="Close Copilot"
            title={docked ? 'Close' : 'Close (Esc)'}
          >
            <X className="h-4 w-4" />
          </Button>
        </header>

        <div ref={scrollRef} className="flex-1 space-y-4 overflow-auto p-4">
          {page && (
            <span className="inline-block rounded-md bg-secondary px-2 py-0.5 text-[11.5px] text-foreground/75">
              Looking at {page}
            </span>
          )}
          {messages.length === 0 && (
            <div className="mt-8 flex flex-col items-center gap-4">
              <p className="text-center text-[13px] text-muted-foreground">
                Changes it proposes wait for your approval.
              </p>
              <div className="flex w-full max-w-xs flex-col gap-1.5">
                {EXAMPLE_QUESTIONS.map((q) => (
                  <button
                    key={q}
                    type="button"
                    disabled={busy}
                    onClick={() => send(q)}
                    className="rounded-md border border-border bg-card px-3 py-2 text-left text-[13px] transition-colors hover:border-ring/40 hover:bg-accent focus-visible:ring-[3px] focus-visible:ring-ring/50 focus-visible:outline-none disabled:opacity-50"
                  >
                    {q}
                  </button>
                ))}
              </div>
            </div>
          )}
          {messages.map((m) => (
            <div key={m.id} className={cn('text-[13px] leading-relaxed', m.role === 'user' ? 'text-right' : '')}>
              {m.role === 'user' ? (
                <div className="inline-block max-w-[85%] rounded-2xl rounded-br-md bg-foreground px-3 py-2 text-left text-background">
                  {m.text}
                </div>
              ) : (
                <div className="space-y-2">
                  {m.tools.length > 0 && (
                    <div className="space-y-1 border-l-2 border-border pl-2.5">
                      {m.tools.map((t, i) => {
                        const summary = argSummary(t.args);
                        return (
                          <div key={i} className="flex items-start gap-1.5 text-xs">
                            {t.done ? (
                              <Check className="mt-0.5 h-3 w-3 shrink-0 text-status-ok" />
                            ) : (
                              <Loader2 className="mt-0.5 h-3 w-3 shrink-0 animate-spin text-muted-foreground" />
                            )}
                            <span className="font-mono text-foreground/80">{t.name}</span>
                            {summary && (
                              <span className="truncate font-mono text-muted-foreground" title={summary}>
                                {summary}
                              </span>
                            )}
                          </div>
                        );
                      })}
                    </div>
                  )}
                  {m.approvals.map((a) => (
                    <ApprovalCard key={a.id} approval={a} />
                  ))}
                  {m.text && <StreamingMarkdown text={m.text} />}
                  {m.outcome && <OutcomeStrip outcome={m.outcome} />}
                  {m.suggestions.map((s, i) => (
                    <SuggestionCard key={i} suggestion={s} />
                  ))}
                  {m.error && (
                    <div className="flex items-start gap-1.5 rounded-md border border-red-500/40 bg-red-500/10 p-2 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                      <span className="break-all">{m.error}</span>
                    </div>
                  )}
                  {busy && m === messages[messages.length - 1] && !m.text && m.tools.length === 0 && (
                    <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />
                  )}
                </div>
              )}
            </div>
          ))}
        </div>

        <footer className="border-t border-border p-3">
          <form
            className="flex items-center gap-2 rounded-xl border border-border bg-card py-1.5 pr-1.5 pl-3 focus-within:border-ring/50"
            onSubmit={(e) => {
              e.preventDefault();
              send();
            }}
          >
            <input
              ref={inputRef}
              value={input}
              onChange={(e) => setInput(e.target.value)}
              placeholder="Ask about the cluster"
              aria-label="Ask the Copilot"
              disabled={busy}
              className="min-w-0 flex-1 bg-transparent text-[13px] outline-none placeholder:text-muted-foreground"
            />
            <Button
              type="submit"
              size="icon"
              aria-label="Send"
              className="size-7 rounded-lg"
              disabled={busy || !input.trim()}
            >
              {busy ? <Loader2 className="size-3.5 animate-spin" /> : <ArrowRight className="size-3.5" />}
            </Button>
          </form>
        </footer>
      </aside>
    </>
  );
}
