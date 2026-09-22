import { ArrowDown, ArrowUp, ArrowUpDown, ChevronLeft, ChevronRight, ChevronsLeft, ChevronsRight } from "lucide-react"
import * as React from "react"
import { Button } from "@/components/ui/button"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { cn } from "@/lib/utils"

// ─── Enhanced Table Components ───

const DataTable = React.forwardRef<HTMLTableElement, React.HTMLAttributes<HTMLTableElement>>(
  ({ className, ...props }, ref) => (
    <div className="relative w-full overflow-auto rounded-md border">
      <table ref={ref} className={cn("w-full caption-bottom text-sm border-collapse", className)} {...props} />
    </div>
  ),
)
DataTable.displayName = "DataTable"

const DataTableHeader = React.forwardRef<HTMLTableSectionElement, React.HTMLAttributes<HTMLTableSectionElement>>(
  ({ className, ...props }, ref) => (
    <thead ref={ref} className={cn("bg-muted/50 [&_tr]:border-b [&_tr:hover]:bg-transparent", className)} {...props} />
  ),
)
DataTableHeader.displayName = "DataTableHeader"

const DataTableBody = React.forwardRef<HTMLTableSectionElement, React.HTMLAttributes<HTMLTableSectionElement>>(
  ({ className, ...props }, ref) => (
    <tbody ref={ref} className={cn("[&_tr:last-child]:border-0", className)} {...props} />
  ),
)
DataTableBody.displayName = "DataTableBody"

const DataTableRow = React.forwardRef<HTMLTableRowElement, React.HTMLAttributes<HTMLTableRowElement>>(
  ({ className, ...props }, ref) => (
    <tr
      ref={ref}
      className={cn("border-b transition-colors hover:bg-muted/40 data-[state=selected]:bg-muted", className)}
      {...props}
    />
  ),
)
DataTableRow.displayName = "DataTableRow"

const DataTableHead = React.forwardRef<HTMLTableCellElement, React.ThHTMLAttributes<HTMLTableCellElement>>(
  ({ className, ...props }, ref) => (
    <th
      ref={ref}
      className={cn(
        "h-10 px-3 text-left align-middle text-xs font-semibold uppercase tracking-wider text-muted-foreground [&:has([role=checkbox])]:pr-0",
        className,
      )}
      {...props}
    />
  ),
)
DataTableHead.displayName = "DataTableHead"

// ─── Sortable column header ───

// A header that asks the server to reorder the list. The label stays a
// button rather than the whole cell so the hit area is the text, and
// aria-sort sits on the th, which is the element a screen reader reads
// the sort state off.
function DataTableSortHead({
  sort,
  column,
  firstOrder = "asc",
  className,
  children,
}: {
  sort: ServerSort
  column: string
  // The direction this column reads best in on the first click. Dates
  // want newest first; names want A to Z. It mirrors the default the
  // API applies for the same column, so the arrow drawn here matches
  // the order that comes back.
  firstOrder?: SortOrder
  className?: string
  children: React.ReactNode
}) {
  const active = sort.sort === column
  return (
    <th
      aria-sort={active ? (sort.order === "asc" ? "ascending" : "descending") : "none"}
      className={cn("h-10 p-0 text-left align-middle [&:has([role=checkbox])]:pr-0", className)}
    >
      <button
        type="button"
        onClick={() => sort.toggle(column, firstOrder)}
        className={cn(
          "group flex h-10 w-full items-center gap-1 px-3 text-left text-xs font-semibold uppercase tracking-wider transition-colors",
          active ? "text-foreground" : "text-muted-foreground hover:text-foreground",
        )}
      >
        {children}
        {active ? (
          sort.order === "asc" ? (
            <ArrowUp className="h-3 w-3 shrink-0" />
          ) : (
            <ArrowDown className="h-3 w-3 shrink-0" />
          )
        ) : (
          <ArrowUpDown className="h-3 w-3 shrink-0 opacity-0 transition-opacity group-hover:opacity-50" />
        )}
      </button>
    </th>
  )
}

const DataTableCell = React.forwardRef<HTMLTableCellElement, React.TdHTMLAttributes<HTMLTableCellElement>>(
  ({ className, ...props }, ref) => (
    <td
      ref={ref}
      className={cn("px-3 py-2.5 align-middle text-sm [&:has([role=checkbox])]:pr-0", className)}
      {...props}
    />
  ),
)
DataTableCell.displayName = "DataTableCell"

// ─── Pagination ───

interface PaginationProps {
  page: number
  totalPages: number
  total: number
  pageSize: number
  onPageChange: (page: number) => void
  onPageSizeChange?: (size: number) => void
  pageSizeOptions?: number[]
}

function DataTablePagination({
  page,
  totalPages,
  total,
  pageSize,
  onPageChange,
  onPageSizeChange,
  pageSizeOptions = [10, 20, 30, 50],
}: PaginationProps) {
  const from = total === 0 ? 0 : page * pageSize + 1
  const to = Math.min((page + 1) * pageSize, total)

  // Generate visible page numbers
  const pages: (number | "ellipsis")[] = []
  if (totalPages <= 7) {
    for (let i = 0; i < totalPages; i++) pages.push(i)
  } else {
    pages.push(0)
    if (page > 2) pages.push("ellipsis")
    for (let i = Math.max(1, page - 1); i <= Math.min(totalPages - 2, page + 1); i++) {
      pages.push(i)
    }
    if (page < totalPages - 3) pages.push("ellipsis")
    pages.push(totalPages - 1)
  }

  return (
    // Two rows on a phone, one on a wide screen. Side by side at 390px
    // the controls ran past the edge and the next/last buttons became
    // unreachable, which is a pager that cannot page.
    <div className="flex flex-col gap-3 px-1 py-3 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        <span className="whitespace-nowrap">
          {from}-{to} of {total.toLocaleString()}
        </span>
        {onPageSizeChange && (
          <>
            <span className="text-border">|</span>
            <div className="flex items-center gap-1.5">
              <span>Rows</span>
              <Select value={String(pageSize)} onValueChange={(v) => onPageSizeChange(Number(v))}>
                <SelectTrigger className="h-7 w-16 text-xs">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {pageSizeOptions.map((size) => (
                    <SelectItem key={size} value={String(size)}>
                      {size}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </>
        )}
      </div>

      <div className="flex items-center justify-center gap-1 sm:justify-end">
        <Button variant="ghost" size="icon" className="h-7 w-7" disabled={page === 0} onClick={() => onPageChange(0)}>
          <ChevronsLeft className="h-3.5 w-3.5" />
        </Button>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          disabled={page === 0}
          onClick={() => onPageChange(page - 1)}
        >
          <ChevronLeft className="h-3.5 w-3.5" />
        </Button>

        {/* A list of a thousand pages does not fit on a phone as
          buttons, and a row of them that overflows takes the next
          and last buttons off the screen with it. Narrow screens get
          the position instead; the buttons come back at sm. */}
        <span className="px-2 text-xs tabular-nums text-muted-foreground sm:hidden">
          {page + 1} / {totalPages}
        </span>

        <div className="hidden items-center gap-1 sm:flex">
          {pages.map((p, i) =>
            p === "ellipsis" ? (
              <span
                key={`ellipsis-${i < pages.length / 2 ? "start" : "end"}`}
                className="px-1 text-xs text-muted-foreground"
              >
                ...
              </span>
            ) : (
              <Button
                key={p}
                variant={p === page ? "default" : "ghost"}
                size="icon"
                className={cn("h-7 w-7 text-xs", p === page && "pointer-events-none")}
                onClick={() => onPageChange(p)}
              >
                {p + 1}
              </Button>
            ),
          )}
        </div>

        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          disabled={page >= totalPages - 1}
          onClick={() => onPageChange(page + 1)}
        >
          <ChevronRight className="h-3.5 w-3.5" />
        </Button>
        <Button
          variant="ghost"
          size="icon"
          className="h-7 w-7"
          disabled={page >= totalPages - 1}
          onClick={() => onPageChange(totalPages - 1)}
        >
          <ChevronsRight className="h-3.5 w-3.5" />
        </Button>
      </div>
    </div>
  )
}

// ─── Empty State ───

function DataTableEmpty({ colSpan, message }: { colSpan: number; message?: string }) {
  return (
    <DataTableRow>
      <DataTableCell colSpan={colSpan} className="h-24 text-center text-muted-foreground">
        {message || "-"}
      </DataTableCell>
    </DataTableRow>
  )
}

// ─── Client-side pagination hook ───

function useClientPagination<T>(items: T[], defaultPageSize = 20) {
  const [page, setPage] = React.useState(0)
  const [pageSize, setPageSize] = React.useState(defaultPageSize)

  const total = items.length
  const totalPages = Math.max(1, Math.ceil(total / pageSize))

  // Reset page when items change significantly
  const safePageRef = React.useRef(page)
  safePageRef.current = page
  React.useEffect(() => {
    if (safePageRef.current >= totalPages) {
      setPage(Math.max(0, totalPages - 1))
    }
  }, [totalPages])

  const paginatedItems = React.useMemo(() => {
    const start = page * pageSize
    return items.slice(start, start + pageSize)
  }, [items, page, pageSize])

  return {
    page,
    setPage,
    pageSize,
    setPageSize: (size: number) => {
      setPageSize(size)
      setPage(0)
    },
    total,
    totalPages,
    paginatedItems,
  }
}

// ─── Server-side pagination hook ───

// The same shape useClientPagination returns, so a page that outgrows
// client-side paging swaps the hook and leaves its table, its pager and
// its JSX alone. `items` stands where `paginatedItems` stood; `params`
// goes straight into the request.
//
// totalPages is worked out from the limit the *server* says it applied,
// not the one we asked for: a request for 1000 rows is served 200, and
// a pager built on the asking number would be five times too short.
function useServerPagination(defaultPageSize = 20, filters?: unknown) {
  const [page, setPage] = React.useState(0)
  const [pageSize, setPageSize] = React.useState(defaultPageSize)

  // Changing a filter starts again at the first page. Staying on page
  // 5 of a list that a new search has cut to one page shows an empty
  // table under a pager that says there is nothing to page through.
  // Adjusted while rendering rather than in an effect: the table is
  // then drawn once, with the right page, instead of drawing the empty
  // one first and correcting it.
  const filterKey = JSON.stringify(filters ?? null)
  const [lastFilterKey, setLastFilterKey] = React.useState(filterKey)
  if (filterKey !== lastFilterKey) {
    setLastFilterKey(filterKey)
    setPage(0)
  }

  return {
    page,
    setPage,
    pageSize,
    setPageSize: (size: number) => {
      setPageSize(size)
      setPage(0)
    },
    params: { limit: pageSize, offset: page * pageSize },
    // Reads one list response.
    //
    // A page that has fallen off the end — the rows on it were deleted
    // while it was open — steps back to the last page there is, the
    // same recovery the client-side hook does. Without it the table
    // sits empty under a pager whose highest page number is lower than
    // the page being shown.
    from<T>(data: { total?: number; limit?: number } | undefined, items: T[] | undefined) {
      const total = data?.total ?? 0
      const limit = data?.limit || pageSize
      const totalPages = Math.max(1, Math.ceil(total / limit))
      if (data && page > 0 && page >= totalPages) setPage(totalPages - 1)
      return { items: items ?? [], total, totalPages }
    },
  }
}

// ─── Server-side sorting ───

type SortOrder = "asc" | "desc"

interface ServerSort {
  sort: string
  order: SortOrder
  params: { sort: string; order: SortOrder }
  toggle: (column: string, firstOrder?: SortOrder) => void
}

// Sorting is server-side for the same reason paging is: the order has
// to be decided over the whole list, not over the fifty rows that
// happen to be on screen.
//
// `params` goes into the request beside the paging params. Feed
// `sort` and `order` into useServerPagination's filter list as well:
// reordering reshuffles every page, so the first page of the new order
// is the only page it makes sense to land on.
function useServerSort(defaultColumn: string, defaultOrder: SortOrder = "desc"): ServerSort {
  const [sort, setSort] = React.useState(defaultColumn)
  const [order, setOrder] = React.useState<SortOrder>(defaultOrder)

  return {
    sort,
    order,
    params: { sort, order },
    toggle(column: string, firstOrder: SortOrder = "asc") {
      if (column === sort) {
        setOrder((o) => (o === "asc" ? "desc" : "asc"))
        return
      }
      // A different column starts in its own natural direction rather
      // than inheriting the previous column's: clicking "Email" after
      // sorting by newest-first should read A to Z, not Z to A.
      setSort(column)
      setOrder(firstOrder)
    },
  }
}

export type { ServerSort, SortOrder }

export {
  DataTable,
  DataTableBody,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTablePagination,
  DataTableRow,
  DataTableSortHead,
  useClientPagination,
  useServerPagination,
  useServerSort,
}
