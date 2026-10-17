import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Search } from 'lucide-react';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';

/** The row above a list: search, filters, and the sort at the far end. */
export function ListToolbar({ children, end }: { children: ReactNode; end?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      {children}
      {end && <div className="ml-auto flex items-center gap-2">{end}</div>}
    </div>
  );
}

/**
 * A search box that reports what was typed once the typing pauses, so a list
 * served a page at a time is not asked again for every keystroke.
 */
export function SearchBox({
  value,
  onChange,
  placeholder,
  'aria-label': ariaLabel,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder: string;
  'aria-label': string;
}) {
  const [text, setText] = useState(value);
  const last = useRef(value);
  useEffect(() => {
    if (value !== last.current) {
      last.current = value;
      setText(value);
    }
  }, [value]);
  useEffect(() => {
    if (text === last.current) return;
    const t = setTimeout(() => {
      last.current = text;
      onChange(text);
    }, 250);
    return () => clearTimeout(t);
  }, [text, onChange]);
  return (
    <label className="flex h-9 min-w-[200px] flex-1 items-center gap-2 rounded-[10px] border border-border bg-card px-3 text-muted-foreground focus-within:border-ring/50 sm:max-w-[360px]">
      <Search className="size-[15px] shrink-0" />
      <input
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder={placeholder}
        aria-label={ariaLabel}
        className="min-w-0 flex-1 bg-transparent text-[13px] text-foreground outline-none placeholder:text-muted-foreground"
      />
    </label>
  );
}

const ANY = '__any__';

/** A dropdown filter that reads "Label  Value", with Any for no filter. */
export function FilterSelect({
  label,
  value,
  onChange,
  options,
  anyLabel = 'Any',
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
  /** What the empty value is called; omit options' own empty choice. */
  anyLabel?: string | null;
}) {
  return (
    <Select value={value || ANY} onValueChange={(v) => onChange(v === ANY ? '' : v)}>
      <SelectTrigger aria-label={label} className="h-9 gap-1.5 rounded-[10px] bg-card px-3 text-[13px] text-foreground/75">
        <span>{label}</span>
        <span className="font-medium text-foreground">
          <SelectValue />
        </span>
      </SelectTrigger>
      <SelectContent>
        {anyLabel !== null && <SelectItem value={ANY}>{anyLabel}</SelectItem>}
        {options.map((o) => (
          <SelectItem key={o.value} value={o.value}>
            {o.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
