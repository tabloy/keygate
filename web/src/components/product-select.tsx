import { keepPreviousData, useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { QueryReadState } from "@/components/query-read-state"
import { PICKER_PAGE, SearchableSelect } from "@/components/ui/searchable-select"
import { useI18n } from "@/i18n"
import { admin } from "@/lib/api"
import { useDebounced } from "@/lib/use-debounced"

// ProductSelect is the product picker every admin page shares — the
// filter above a table, and the field in a create or edit form.
//
// The candidates are searched on the server rather than fetched whole:
// a Select puts every item it is given into the DOM whether or not it
// is open, and an install with tens of thousands of products used to
// render all of them, on eight pages, until the tab stopped answering
// clicks. Typing narrows the list, so a product past the first page is
// still reachable.
//
// allLabel adds the "all products" entry and makes the empty string
// mean it; noneLabel is the same for "no product" (an API key bound to
// none). `current` is the product a row already points at, shown even
// when it is not among the candidates loaded.
export function ProductSelect({
  value,
  onChange,
  allLabel,
  noneLabel,
  current,
  placeholder,
  className = "w-48",
  disabled,
  withType,
  types,
}: {
  value: string
  onChange: (value: string) => void
  allLabel?: string
  noneLabel?: string
  current?: { id: string; name: string; type?: string } | null
  placeholder?: string
  className?: string
  disabled?: boolean
  /** Show each product's type beside its name, as the plan form does. */
  withType?: boolean
  /**
   * Narrow the candidates to these product kinds. The releases pages
   * pass the ones that ship binaries: offering a saas product there
   * only leads to a filled-in form the server then refuses.
   */
  types?: string[]
}) {
  const { t } = useI18n()
  const [search, setSearch] = useState("")
  const query = useDebounced(search, 250)
  const type = types?.join(",")
  const readQuery = useQuery({
    queryKey: ["admin", "products", "picker", query, type],
    queryFn: () => admin.listProducts({ search: query, type, limit: PICKER_PAGE }),
    placeholderData: keepPreviousData,
  })
  const { data, isFetching } = readQuery
  const products = data?.products || []

  const picker = (
    <SearchableSelect
      value={value}
      onChange={onChange}
      options={products.map((p) => ({ id: p.id, label: p.name, hint: withType && p.type ? `[${p.type}]` : undefined }))}
      total={data?.total || 0}
      search={search}
      onSearchChange={setSearch}
      current={current ? { id: current.id, label: current.name } : null}
      allLabel={allLabel}
      noneLabel={noneLabel}
      placeholder={placeholder}
      searchPlaceholder={t("filter.searchProducts")}
      emptyLabel={t("filter.noMatches")}
      moreLabel={(hidden) => t("filter.moreMatches", { count: hidden })}
      className={className}
      disabled={disabled}
      loading={isFetching}
    />
  )
  return <QueryReadState queries={[readQuery]}>{picker}</QueryReadState>
}
