import { Badge } from "@/components/ui/badge"
import { type TranslationKeys, useI18n } from "@/i18n"
import { cn, statusColor } from "@/lib/utils"

export function StatusBadge({ status, className }: { status: string; className?: string }) {
  const { t } = useI18n()
  return <Badge className={cn(statusColor(status), className)}>{t(`status.${status}` as TranslationKeys)}</Badge>
}
