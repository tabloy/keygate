import type { ReactNode } from "react"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"
import { useI18n } from "@/i18n"

interface ConfirmDialogProps {
  open?: boolean
  onOpenChange?: (open: boolean) => void
  trigger?: ReactNode
  title: ReactNode
  description: ReactNode
  actionLabel?: ReactNode
  cancelLabel?: ReactNode
  variant?: "default" | "destructive"
  onConfirm: () => void
  loading?: boolean
}

export function ConfirmDialog({
  open,
  onOpenChange,
  trigger,
  title,
  description,
  actionLabel,
  cancelLabel,
  variant = "destructive",
  onConfirm,
  loading = false,
}: ConfirmDialogProps) {
  const { t } = useI18n()

  const dialogProps = trigger ? {} : { open, onOpenChange }

  return (
    <AlertDialog {...dialogProps}>
      {trigger && <AlertDialogTrigger asChild>{trigger}</AlertDialogTrigger>}
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        <div className="flex justify-end gap-2 mt-4">
          <AlertDialogCancel disabled={loading}>{cancelLabel ?? t("common.cancel")}</AlertDialogCancel>
          <AlertDialogAction
            disabled={loading}
            className={variant === "destructive" ? "bg-destructive text-white hover:bg-destructive/90" : undefined}
            onClick={() => onConfirm()}
          >
            {actionLabel ?? (variant === "destructive" ? t("common.delete") : t("common.confirm"))}
          </AlertDialogAction>
        </div>
      </AlertDialogContent>
    </AlertDialog>
  )
}
