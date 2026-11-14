import { Badge } from "@/components/ui/badge"
import { type TranslationKeys, useI18n } from "@/i18n"
import { boolColor, cn } from "@/lib/utils"

export function ActiveBadge({
  active,
  inactiveLabelKey = "common.inactive",
  className,
}: {
  active: boolean
  inactiveLabelKey?: TranslationKeys
  className?: string
}) {
  const { t } = useI18n()
  return <Badge className={cn(boolColor(active), className)}>{active ? t("common.active") : t(inactiveLabelKey)}</Badge>
}
