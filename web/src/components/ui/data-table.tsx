import { ChevronLeft, ChevronRight, ChevronsLeft, ChevronsRight } from "lucide-react"
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
    <div className="flex items-center justify-between gap-4 px-1 py-3">
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        <span>
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

      <div className="flex items-center gap-1">
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

export {
  DataTable,
  DataTableBody,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTablePagination,
  DataTableRow,
  useClientPagination,
}
