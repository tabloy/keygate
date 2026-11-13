import { useState } from "react"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { cn } from "@/lib/utils"

export interface SearchableOption {
  id: string
  label: string
  hint?: string
}

// How many rows a picker asks for. The list is searched on the server,
// so this is the size of one page of candidates, not a ceiling on what
// can be chosen — that was the bug this replaced: an install with more
// products than the picker fetched could not select the rest of them.
export const PICKER_PAGE = 50

/**
 * A Select whose candidates come from the server and are narrowed by
 * typing, for lists too long to render whole (products, plans).
 *
 * Three things it has to get right:
 *   - the search box lives inside the dropdown, and Radix must not
 *     treat what is typed there as its own typeahead;
 *   - the value in hand always shows, even when it is not on the page
 *     of candidates currently loaded — the row's own value (`current`)
 *     and whatever was picked here are both kept available, because a
 *     Select takes its trigger text from the item that is selected and
 *     goes blank when a new search drops that item from the list;
 *   - it says when there are more matches than are shown, so nobody
 *     concludes a missing row does not exist.
 */
export function SearchableSelect({
  value,
  onChange,
  options,
  total,
  search,
  onSearchChange,
  current,
  allLabel,
  noneLabel,
  placeholder,
  searchPlaceholder,
  moreLabel,
  emptyLabel,
  className = "w-48",
  disabled,
  loading,
}: {
  value: string
  onChange: (value: string) => void
  options: SearchableOption[]
  total: number
  search: string
  onSearchChange: (value: string) => void
  current?: SearchableOption | null
  allLabel?: string
  noneLabel?: string
  placeholder?: string
  searchPlaceholder?: string
  /** Called with how many matches are not shown. */
  moreLabel?: (hidden: number, total: number) => string
  emptyLabel?: string
  className?: string
  disabled?: boolean
  loading?: boolean
}) {
  const sentinel = allLabel ? "all" : noneLabel ? "none" : ""
  // What was chosen here, kept so narrowing the search afterwards does
  // not empty the box: the option is gone from the list, the value is
  // still set, and the trigger would have nothing to render.
  const [picked, setPicked] = useState<SearchableOption | null>(null)
  const inList = options.some((o) => o.id === value)
  const held = picked?.id === value ? picked : current?.id === value ? current : null
  const offList = !inList && value && value !== sentinel ? held : null
  const hidden = Math.max(0, total - options.length)

  return (
    <Select
      value={sentinel ? value || sentinel : value}
      onValueChange={(v) => {
        setPicked(options.find((o) => o.id === v) ?? null)
        onChange(sentinel && v === sentinel ? "" : v)
      }}
      disabled={disabled}
    >
      <SelectTrigger className={className}>
        <SelectValue placeholder={placeholder || allLabel} />
      </SelectTrigger>
      <SelectContent>
        {/* Radix runs a typeahead over the items and would swallow
            what is typed here, and it takes pointer events on the
            content as a pick, so the input keeps both to itself. */}
        <div className="p-1">
          <Input
            value={search}
            onChange={(e) => onSearchChange(e.target.value)}
            placeholder={searchPlaceholder}
            className="h-8"
            autoComplete="off"
            onKeyDown={(e) => {
              e.stopPropagation()
              if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return
              e.preventDefault()
              const selector = search.trim()
                ? '[role="option"][data-picker-result]:not([data-disabled])'
                : '[role="option"]:not([data-disabled])'
              const items = e.currentTarget.closest('[role="listbox"]')?.querySelectorAll<HTMLElement>(selector)
              const target = e.key === "ArrowDown" ? items?.[0] : items?.[items.length - 1]
              target?.focus()
            }}
            onKeyUp={(e) => e.stopPropagation()}
            onPointerDown={(e) => e.stopPropagation()}
          />
        </div>
        {allLabel && <SelectItem value="all">{allLabel}</SelectItem>}
        {noneLabel && <SelectItem value="none">{noneLabel}</SelectItem>}
        {offList && (
          <SelectItem key={offList.id} value={offList.id}>
            {offList.label}
            {offList.hint ? <span className="ml-2 text-xs text-muted-foreground">{offList.hint}</span> : null}
          </SelectItem>
        )}
        {options.map((o) => (
          <SelectItem key={o.id} value={o.id} data-picker-result="">
            {o.label}
            {o.hint ? <span className="ml-2 text-xs text-muted-foreground">{o.hint}</span> : null}
          </SelectItem>
        ))}
        {options.length === 0 && !loading && !offList && (
          <p className={cn("px-2 py-1.5 text-xs text-muted-foreground")}>{emptyLabel}</p>
        )}
        {hidden > 0 && moreLabel && (
          <p className="px-2 py-1.5 text-xs text-muted-foreground">{moreLabel(hidden, total)}</p>
        )}
      </SelectContent>
    </Select>
  )
}
