import { keepPreviousData, useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { PICKER_PAGE, SearchableSelect } from "@/components/ui/searchable-select"
import { useI18n } from "@/i18n"
import { admin, type Plan } from "@/lib/api"

// PlanSelect picks a plan of one product, searched on the server for
// the same reason ProductSelect is: a product with more plans than one
// page would otherwise have the rest of them unreachable — including,
// on the change-plan dialog, as a target to move a licence to.
type PlanSelectProps = {
  productId: string
  value: string
  onChange: (value: string) => void
  current?: Plan | null
  allLabel?: string
  placeholder?: string
  className?: string
  disabled?: boolean
  /** Show each plan's licence type beside its name. */
  withType?: boolean
}

export function PlanSelect(props: PlanSelectProps) {
  return <ProductPlanSelect key={props.productId} {...props} />
}

function ProductPlanSelect({
  productId,
  value,
  onChange,
  current,
  allLabel,
  placeholder,
  className = "w-48",
  disabled,
  withType,
}: PlanSelectProps) {
  const { t } = useI18n()
  const [search, setSearch] = useState("")
  const { data, isFetching } = useQuery({
    queryKey: ["admin", "plans", "picker", productId, search],
    queryFn: () => admin.listPlans({ product_id: productId, search, limit: PICKER_PAGE }),
    enabled: !!productId,
    placeholderData: keepPreviousData,
  })
  const plans = data?.plans || []
  const label = (p: Plan) => (withType && p.license_type ? `[${p.license_type}]` : undefined)

  return (
    <SearchableSelect
      value={value}
      onChange={onChange}
      options={plans.map((p) => ({ id: p.id, label: p.name, hint: label(p) }))}
      total={data?.total || 0}
      search={search}
      onSearchChange={setSearch}
      current={current?.product_id === productId ? { id: current.id, label: current.name, hint: label(current) } : null}
      allLabel={allLabel}
      placeholder={placeholder}
      searchPlaceholder={t("filter.searchPlans")}
      emptyLabel={t("filter.noMatches")}
      moreLabel={(hidden) => t("filter.moreMatches", { count: hidden })}
      className={className}
      disabled={disabled}
      loading={isFetching}
    />
  )
}
